// Package backup sends this machine's resume records off-host, so a session survives not just
// a reboot but the loss of the machine itself — the case that matters on an ephemeral cloud
// instance, whose disk (and the local ~/.conductor/sessions records with it) vanishes when the
// instance is terminated.
//
// The S3 client here is deliberately dependency-free: it signs requests with AWS Signature
// Version 4 over net/http and crypto/hmac, rather than pulling in the AWS SDK. That keeps
// go.mod as small as the rest of the project (the dashboard makes no external requests, the
// MCP gateway is hand-rolled JSON-RPC, the intent engine is hand-rolled MinHash), and it works
// against any S3-compatible endpoint — real S3, MinIO, Cloudflare R2, Ceph — by setting an
// endpoint. What crosses the wire is the same thing every other Conductor channel carries:
// coordination metadata (how to reopen a session), never a transcript.
package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3 is a minimal S3 client for one bucket. The zero value is unusable; build one with
// New. It is safe for concurrent use.
type S3 struct {
	cfg  S3Config
	http *http.Client
	// now is injectable so the SigV4 signing (which is time-dependent) can be tested against
	// AWS's published example vector.
	now func() time.Time
}

// Credentials are the keys a request is signed with.
type Credentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	// Expires is when temporary credentials stop working; zero means they do not expire.
	Expires time.Time
	// Source says where the keys came from ("static (keychain)", "profile sso (dev)", …),
	// for `conductor storage test` and error messages. It is never a secret.
	Source string
}

// CredentialSource yields credentials for each request, so keys that expire (SSO, an
// assumed role, an instance role) can be refreshed between requests. See internal/awscreds.
type CredentialSource interface {
	Retrieve(ctx context.Context) (Credentials, error)
}

// S3Config is everything needed to reach a bucket.
type S3Config struct {
	Bucket string
	Region string
	// Credentials, when set, is asked for keys on every request and takes precedence over
	// the static AccessKey/SecretKey/SessionToken below.
	Credentials CredentialSource
	AccessKey   string
	SecretKey   string
	// SessionToken is set when credentials come from STS / an instance role.
	SessionToken string
	// Endpoint overrides the AWS host, for S3-compatible stores. Empty means AWS. When set,
	// PathStyle is usually required (MinIO, older setups).
	Endpoint  string
	PathStyle bool
	// Insecure allows plain http, for a local MinIO in a test. Without it an http:// endpoint
	// is refused. Ignored when Endpoint is empty.
	Insecure bool
}

const (
	service     = "s3"
	emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	isoLayout   = "20060102T150405Z"
	dayLayout   = "20060102"
)

// requestFloor is the time every request gets: the handshake, signing and the first byte.
const requestFloor = 30 * time.Second

// minTransferRate is the slowest link a body is assumed to cross, 256 KiB/s. A transfer that
// takes longer than its size at this rate is treated as stalled.
const minTransferRate = 256 << 10

// requestTimeout bounds one request: requestFloor, plus the time bodyBytes take at
// minTransferRate. The client used to carry one fixed 30 s timeout, which also bounded the
// body, so a large checkpoint on a slow link failed partway through.
func requestTimeout(bodyBytes int64) time.Duration {
	return requestFloor + time.Duration(float64(bodyBytes)*float64(time.Second)/minTransferRate)
}

// New builds a client. It does not validate credentials; the first request will. The client
// has no timeout of its own: each request carries a deadline sized to its body (requestTimeout).
func New(cfg S3Config) *S3 {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return &S3{cfg: cfg, http: &http.Client{}, now: func() time.Time { return time.Now().UTC() }}
}

// Put stores body at key with the given content type.
func (s *S3) Put(ctx context.Context, key string, body []byte, contentType string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout(int64(len(body))))
	defer cancel()
	req, err := s.newRequest(ctx, http.MethodPut, key, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := s.do(req, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return s3Error("PUT", key, resp)
	}
	return nil
}

// maxObjectBytes is the largest object Get returns: 128 MiB, which holds a 64 MiB WAL segment
// with the seal's overhead. A larger object is an error. It is never returned cut short, because
// a truncated checkpoint or segment would look valid until it failed to open.
const maxObjectBytes = 128 << 20

// Get fetches key. A missing key returns ErrNotFound; an object over maxObjectBytes is an error.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	// The size is unknown until the response arrives, so the deadline allows the largest object.
	// The cancel stays deferred until the body has been read.
	ctx, cancel := context.WithTimeout(ctx, requestTimeout(maxObjectBytes))
	defer cancel()
	req, err := s.newRequest(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		// Only a missing key is "nothing there". A 404 for anything else, such as a misspelled
		// bucket (NoSuchBucket), is an error: read as "nothing to restore", it would exit 0.
		if resp.StatusCode == http.StatusNotFound && s3ErrorCode(body) == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, s3ErrorFrom("GET", key, resp, body)
	}
	if resp.ContentLength > maxObjectBytes {
		return nil, objectTooLarge(key)
	}
	// One byte past the cap tells a body that is exactly the cap from one that is larger.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxObjectBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxObjectBytes {
		return nil, objectTooLarge(key)
	}
	return body, nil
}

func objectTooLarge(key string) error {
	return fmt.Errorf("backup: S3 GET %s: the object is larger than %d MiB and was not read", key, maxObjectBytes>>20)
}

// List returns every key under a prefix, sorted. S3 answers a page at a time (1000 keys at most),
// so the listing is followed through its continuation tokens until it is not truncated.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	used := map[string]bool{}
	for {
		used[token] = true
		page, err := s.listPage(ctx, prefix, token)
		if err != nil {
			return nil, err
		}
		for _, c := range page.Contents {
			keys = append(keys, c.Key)
		}
		if !page.IsTruncated {
			break
		}
		// A truncated page must name the next one. A token seen before would loop forever.
		if page.NextContinuationToken == "" || used[page.NextContinuationToken] {
			return nil, fmt.Errorf("backup: S3 LIST %s: the listing is truncated but names no new next page", prefix)
		}
		token = page.NextContinuationToken
	}
	sort.Strings(keys)
	return keys, nil
}

// listPage is one page of a ListObjectsV2 response.
type listPage struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

// listPage fetches the page that follows token (the first page when token is "").
func (s *S3) listPage(ctx context.Context, prefix, token string) (listPage, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout(0))
	defer cancel()
	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("prefix", prefix)
	if token != "" {
		q.Set("continuation-token", token)
	}
	req, err := s.newRequest(ctx, http.MethodGet, "?"+q.Encode(), nil)
	if err != nil {
		return listPage{}, err
	}
	resp, err := s.do(req, nil)
	if err != nil {
		return listPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return listPage{}, s3Error("LIST", prefix, resp)
	}
	var page listPage
	if err := xml.NewDecoder(resp.Body).Decode(&page); err != nil {
		return listPage{}, err
	}
	return page, nil
}

// Delete removes key. Deleting a key that does not exist succeeds, as it does in S3.
func (s *S3) Delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout(0))
	defer cancel()
	req, err := s.newRequest(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return s3Error("delete", key, resp)
	}
	return nil
}

// ErrNotFound is returned by Get when the key is absent.
var ErrNotFound = fmt.Errorf("backup: key not found")

func (s *S3) do(req *http.Request, body []byte) (*http.Response, error) {
	if err := s.sign(req, body); err != nil {
		return nil, err
	}
	return s.http.Do(req)
}

// newRequest builds an unsigned request. keyOrQuery is either an object key ("a/b.json") or,
// for List, a "?..." query against the bucket root.
func (s *S3) newRequest(ctx context.Context, method, keyOrQuery string, body []byte) (*http.Request, error) {
	endpoint, err := s.url(keyOrQuery)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	return http.NewRequestWithContext(ctx, method, endpoint, rdr)
}

// url builds the request URL for a key (or a "?query"), honouring path-style vs virtual-host.
func (s *S3) url(keyOrQuery string) (string, error) {
	scheme := "https"
	host := s.cfg.Bucket + ".s3." + s.cfg.Region + ".amazonaws.com"
	basePath := "/"
	if s.cfg.Endpoint != "" {
		u, err := url.Parse(s.cfg.Endpoint)
		if err != nil {
			return "", err
		}
		if u.Scheme != "" {
			scheme = strings.ToLower(u.Scheme)
		} else if s.cfg.Insecure {
			scheme = "http"
		}
		// Plain http sends the signed request — and every record and sealed checkpoint —
		// past anyone on the path, and lets them answer in the bucket's place. It is for a
		// local MinIO in a test, so it takes the explicit Insecure setting, whatever the
		// endpoint URL says.
		switch {
		case scheme == "http" && !s.cfg.Insecure:
			return "", fmt.Errorf("s3: endpoint %s is plain http; set CONDUCTOR_BACKUP_S3_INSECURE=1 to allow it", s.cfg.Endpoint)
		case scheme != "http" && scheme != "https":
			return "", fmt.Errorf("s3: endpoint scheme %q is not http or https", scheme)
		}
		host = u.Host
		if host == "" {
			host = u.Path // endpoint given without scheme, e.g. "localhost:9000"
		}
		if s.cfg.PathStyle {
			basePath = "/" + s.cfg.Bucket + "/"
		} else {
			host = s.cfg.Bucket + "." + host
		}
	}
	if strings.HasPrefix(keyOrQuery, "?") {
		return scheme + "://" + host + basePath + keyOrQuery, nil
	}
	return scheme + "://" + host + basePath + strings.TrimPrefix(keyOrQuery, "/"), nil
}

// sign applies AWS Signature Version 4 to req over the given body. This is the whole reason
// the AWS SDK is not a dependency; the algorithm is small and is pinned to AWS's published
// S3 GET-Object example vector in s3_test.go, so a canonicalization regression is caught.
func (s *S3) sign(req *http.Request, body []byte) error {
	creds := Credentials{AccessKey: s.cfg.AccessKey, SecretKey: s.cfg.SecretKey, SessionToken: s.cfg.SessionToken}
	if s.cfg.Credentials != nil {
		c, err := s.cfg.Credentials.Retrieve(req.Context())
		if err != nil {
			return fmt.Errorf("backup: S3 credentials: %w", err)
		}
		creds = c
	}
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return fmt.Errorf("backup: no S3 credentials (set the access key and secret)")
	}
	return SignV4(req, body, creds, s.cfg.Region, service, s.now())
}

// SignV4 applies AWS Signature Version 4 to req over body, for the named service ("s3",
// "sts") in region. It sets Host, X-Amz-Date, X-Amz-Content-Sha256, the session token when
// there is one, and Authorization. internal/awscreds uses it to sign STS calls.
func SignV4(req *http.Request, body []byte, creds Credentials, region, service string, now time.Time) error {
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return fmt.Errorf("sigv4: no credentials")
	}
	now = now.UTC()
	amzDate := now.Format(isoLayout)
	dateStamp := now.Format(dayLayout)

	payloadHash := emptySHA256
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(sum[:])
	}

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	// Canonical request.
	signedHeaders, canonicalHeaders := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	crHash := sha256.Sum256([]byte(canonicalRequest))

	// String to sign.
	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(crHash[:]),
	}, "\n")

	// Signing key and signature.
	kDate := hmacSHA256([]byte("AWS4"+creds.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKey, scope, signedHeaders, signature))
	return nil
}

func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	// AWS's UriEncode for the canonical URI percent-encodes every byte except the unreserved
	// set and '/'. Go's url.EscapedPath leaves reserved sub-delims (+ : = @ & , ; $) literal,
	// which produces a canonical URI real S3 will not reproduce (SignatureDoesNotMatch) for a
	// key containing one. Use the project's own encoder, which matches AWS exactly.
	return awsURIEncode(u.Path, false)
}

func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	vals := u.Query()
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := vals[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsURIEncode(k, true)+"="+awsURIEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

func canonicalHeaders(req *http.Request) (signed, canonical string) {
	names := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if req.Header.Get("Content-Type") != "" {
		names = append(names, "content-type")
	}
	if req.Header.Get("Range") != "" {
		names = append(names, "range")
	}
	if req.Header.Get("X-Amz-Security-Token") != "" {
		names = append(names, "x-amz-security-token")
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		var v string
		switch n {
		case "host":
			v = req.URL.Host
		default:
			v = req.Header.Get(n)
		}
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(strings.TrimSpace(v))
		b.WriteByte('\n')
	}
	return strings.Join(names, ";"), b.String()
}

// awsURIEncode encodes per RFC 3986 as SigV4 requires; when encodeSlash is false, '/' is
// left as-is (used for object paths).
func awsURIEncode(s string, encodeSlash bool) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0f])
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func s3Error(op, key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return s3ErrorFrom(op, key, resp, body)
}

func s3ErrorFrom(op, key string, resp *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("backup: S3 %s %s: %s: %s", op, key, resp.Status, msg)
}

// ErrExists is returned by PutIfAbsent when the key already holds an object.
var ErrExists = errors.New("backup: key already exists")

// Exists reports whether key holds an object (HEAD).
func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	req, err := s.newRequest(ctx, http.MethodHead, key, nil)
	if err != nil {
		return false, err
	}
	resp, err := s.do(req, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode/100 == 2:
		return true, nil
	}
	return false, s3Error("HEAD", key, resp)
}

// PutIfAbsent stores body at key only if nothing is there yet (If-None-Match: *, which S3,
// MinIO and R2 honour), returning ErrExists otherwise. A store that ignores the condition
// overwrites, so callers that must not overwrite check Exists first as well.
func (s *S3) PutIfAbsent(ctx context.Context, key string, body []byte, contentType string) error {
	// Sized to the body, as Put is: a WAL segment uploaded on a slow link must not outlive its deadline.
	ctx, cancel := context.WithTimeout(ctx, requestTimeout(int64(len(body))))
	defer cancel()
	req, err := s.newRequest(ctx, http.MethodPut, key, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("If-None-Match", "*")
	resp, err := s.do(req, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPreconditionFailed || resp.StatusCode == http.StatusConflict:
		return ErrExists
	case resp.StatusCode/100 != 2:
		return s3Error("PUT", key, resp)
	}
	return nil
}

// GetReader streams an object. The caller closes it. A missing key is ErrNotFound.
func (s *S3) GetReader(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := s.newRequest(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.doStreaming(req, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, s3Error("GET", key, resp)
	}
	return resp.Body, nil
}

// Object is one listing entry.
type Object struct {
	Key  string
	Size int64
}

// ListAll returns every object under prefix, following continuation tokens, sorted by key.
func (s *S3) ListAll(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for page := 0; page < 100000; page++ {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", prefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := s.newRequest(ctx, http.MethodGet, "?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.do(req, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 != 2 {
			defer resp.Body.Close()
			return nil, s3Error("LIST", prefix, resp)
		}
		var parsed struct {
			Contents []struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			} `xml:"Contents"`
			IsTruncated bool   `xml:"IsTruncated"`
			NextToken   string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&parsed)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("backup: S3 LIST %s: %w", prefix, err)
		}
		for _, c := range parsed.Contents {
			out = append(out, Object{Key: c.Key, Size: c.Size})
		}
		if !parsed.IsTruncated || parsed.NextToken == "" {
			sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
			return out, nil
		}
		token = parsed.NextToken
	}
	return nil, fmt.Errorf("backup: S3 LIST %s: too many pages", prefix)
}

// MinPartSize is S3's smallest allowed multipart part (except the last).
const MinPartSize = 5 << 20

// PutStream uploads everything read from r to key. Up to partSize bytes it is one PUT;
// beyond that a multipart upload in partSize parts (at least MinPartSize), so the object
// is never held in memory whole. A failed multipart upload is aborted. It returns the
// number of bytes stored.
func (s *S3) PutStream(ctx context.Context, key string, r io.Reader, partSize int, contentType string) (int64, error) {
	if partSize < MinPartSize {
		partSize = MinPartSize
	}
	first := make([]byte, partSize)
	n, err := io.ReadFull(r, first)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if n < partSize {
		return int64(n), s.Put(ctx, key, first[:n], contentType)
	}

	uploadID, err := s.createMultipart(ctx, key, contentType)
	if err != nil {
		return 0, err
	}
	abort := func(cause error) (int64, error) {
		_ = s.abortMultipart(context.WithoutCancel(ctx), key, uploadID)
		return 0, cause
	}
	var etags []string
	total := int64(0)
	buf := first[:n]
	for part := 1; ; part++ {
		etag, err := s.uploadPart(ctx, key, uploadID, part, buf)
		if err != nil {
			return abort(err)
		}
		etags = append(etags, etag)
		total += int64(len(buf))
		if part >= 10000 {
			return abort(fmt.Errorf("backup: %s needs more than 10000 parts; raise the part size", key))
		}
		next := make([]byte, partSize)
		m, err := io.ReadFull(r, next)
		if m == 0 {
			if err != nil && !errors.Is(err, io.EOF) {
				return abort(err)
			}
			break
		}
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return abort(err)
		}
		buf = next[:m]
	}
	if err := s.completeMultipart(ctx, key, uploadID, etags); err != nil {
		return abort(err)
	}
	return total, nil
}

func (s *S3) keyQueryRequest(ctx context.Context, method, key string, q url.Values, body []byte) (*http.Request, error) {
	base, err := s.url(key)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	return http.NewRequestWithContext(ctx, method, base+"?"+q.Encode(), rdr)
}

func (s *S3) createMultipart(ctx context.Context, key, contentType string) (string, error) {
	req, err := s.keyQueryRequest(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil)
	if err != nil {
		return "", err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := s.do(req, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", s3Error("CreateMultipartUpload", key, resp)
	}
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.UploadID == "" {
		return "", fmt.Errorf("backup: S3 CreateMultipartUpload %s: no upload ID", key)
	}
	return out.UploadID, nil
}

func (s *S3) uploadPart(ctx context.Context, key, uploadID string, part int, body []byte) (string, error) {
	q := url.Values{"partNumber": {fmt.Sprint(part)}, "uploadId": {uploadID}}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := s.keyQueryRequest(ctx, http.MethodPut, key, q, body)
		if err != nil {
			return "", err
		}
		resp, err := s.doStreaming(req, body)
		if err != nil {
			lastErr = err
			continue
		}
		etag := resp.Header.Get("ETag")
		if resp.StatusCode/100 == 2 && etag != "" {
			resp.Body.Close()
			return etag, nil
		}
		lastErr = s3Error("UploadPart", key, resp)
		resp.Body.Close()
		if resp.StatusCode/100 == 4 {
			break
		}
	}
	return "", lastErr
}

func (s *S3) completeMultipart(ctx context.Context, key, uploadID string, etags []string) error {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for i, e := range etags {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, xmlEscape(e))
	}
	b.WriteString("</CompleteMultipartUpload>")
	body := []byte(b.String())
	req, err := s.keyQueryRequest(ctx, http.MethodPost, key, url.Values{"uploadId": {uploadID}}, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml")
	resp, err := s.do(req, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// S3 can answer 200 with an <Error> body when the completion fails late.
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 || bytes.Contains(payload, []byte("<Error>")) {
		return fmt.Errorf("backup: S3 CompleteMultipartUpload %s: %s: %s", key, resp.Status, strings.TrimSpace(string(payload)))
	}
	return nil
}

func (s *S3) abortMultipart(ctx context.Context, key, uploadID string) error {
	req, err := s.keyQueryRequest(ctx, http.MethodDelete, key, url.Values{"uploadId": {uploadID}}, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// doStreaming is do without the client timeout, for transfers whose duration depends on
// their size (a base backup download, a large part); the caller's context still bounds it.
func (s *S3) doStreaming(req *http.Request, body []byte) (*http.Response, error) {
	if err := s.sign(req, body); err != nil {
		return nil, err
	}
	client := *s.http
	client.Timeout = 0
	return client.Do(req)
}

// s3ErrorCode is the <Code> of an S3 XML error body, or "" when there is none.
func s3ErrorCode(body []byte) string {
	var e struct {
		Code string `xml:"Code"`
	}
	if xml.Unmarshal(body, &e) != nil {
		return ""
	}
	return e.Code
}
