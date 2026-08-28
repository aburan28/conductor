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

// S3Config is everything needed to reach a bucket.
type S3Config struct {
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	// SessionToken is set when credentials come from STS / an instance role.
	SessionToken string
	// Endpoint overrides the AWS host, for S3-compatible stores. Empty means AWS. When set,
	// PathStyle is usually required (MinIO, older setups).
	Endpoint  string
	PathStyle bool
	// Insecure allows plain http, for a local MinIO in a test. Ignored when Endpoint is empty.
	Insecure bool
}

const (
	service     = "s3"
	emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	isoLayout   = "20060102T150405Z"
	dayLayout   = "20060102"
)

// New builds a client. It does not validate credentials; the first request will.
func New(cfg S3Config) *S3 {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return &S3{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}, now: func() time.Time { return time.Now().UTC() }}
}

// Put stores body at key with the given content type.
func (s *S3) Put(ctx context.Context, key string, body []byte, contentType string) error {
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

// Get fetches key. A missing key returns ErrNotFound.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	req, err := s.newRequest(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, s3Error("GET", key, resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// List returns the keys under a prefix (up to 1000, which is far more sessions than a machine
// holds). It is used to enumerate snapshots.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("prefix", prefix)
	req, err := s.newRequest(ctx, http.MethodGet, "?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, s3Error("LIST", prefix, resp)
	}
	var parsed struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(parsed.Contents))
	for _, c := range parsed.Contents {
		keys = append(keys, c.Key)
	}
	sort.Strings(keys)
	return keys, nil
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
			scheme = u.Scheme
		} else if s.cfg.Insecure {
			scheme = "http"
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
	if s.cfg.AccessKey == "" || s.cfg.SecretKey == "" {
		return fmt.Errorf("backup: no S3 credentials (set the access key and secret)")
	}
	now := s.now()
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
	if s.cfg.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.cfg.SessionToken)
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
	scope := strings.Join([]string{dateStamp, s.cfg.Region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(crHash[:]),
	}, "\n")

	// Signing key and signature.
	kDate := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, s.cfg.Region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.AccessKey, scope, signedHeaders, signature))
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
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("backup: S3 %s %s: %s: %s", op, key, resp.Status, msg)
}
