package backup

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is an in-memory, path-style S3 that records the requests it receives, so the client's
// URL building, method, headers, and list/XML handling are exercised over real HTTP. It also
// asserts every request carried a well-formed SigV4 Authorization header signing exactly the
// headers it sent — the canonicalization mistakes that a fake bucket would otherwise hide.
type fakeS3 struct {
	t      *testing.T
	mu     sync.Mutex
	bucket string
	obj    map[string][]byte
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	f := &fakeS3{t: t, bucket: "conductor-test", obj: map[string][]byte{}}
	return f, httptest.NewServer(f)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.assertSigned(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	// Path-style: /<bucket>/<key...>
	prefix := "/" + f.bucket + "/"
	switch {
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		f.list(w, r)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.obj[strings.TrimPrefix(r.URL.Path, prefix)] = body
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet:
		key := strings.TrimPrefix(r.URL.Path, prefix)
		body, ok := f.obj[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	type obj struct {
		Key string `xml:"Key"`
	}
	var out struct {
		XMLName  xml.Name `xml:"ListBucketResult"`
		Contents []obj    `xml:"Contents"`
	}
	for k := range f.obj {
		if strings.HasPrefix("/"+f.bucket+"/"+k, "/"+f.bucket+"/"+prefix) || strings.HasPrefix(k, prefix) {
			out.Contents = append(out.Contents, obj{Key: k})
		}
	}
	_ = xml.NewEncoder(w).Encode(out)
}

// assertSigned checks the Authorization header is a SigV4 header that signs exactly the
// headers actually present on the wire — the property that catches canonicalization drift.
func (f *fakeS3) assertSigned(r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		f.t.Errorf("missing/blank SigV4 Authorization on %s %s: %q", r.Method, r.URL, auth)
		return
	}
	if r.Header.Get("X-Amz-Date") == "" || r.Header.Get("X-Amz-Content-Sha256") == "" {
		f.t.Errorf("missing required amz headers on %s %s", r.Method, r.URL)
	}
	var signed string
	for _, part := range strings.Split(auth, ", ") {
		if v, ok := strings.CutPrefix(part, "SignedHeaders="); ok {
			signed = v
		}
	}
	for _, h := range strings.Split(signed, ";") {
		switch h {
		case "host", "x-amz-date", "x-amz-content-sha256":
		case "content-type":
			if r.Header.Get("Content-Type") == "" {
				f.t.Errorf("signed content-type but none sent")
			}
		default:
			f.t.Errorf("signed an unexpected header %q", h)
		}
	}
}

func testClient(server *httptest.Server) *S3 {
	u, _ := url.Parse(server.URL)
	return New(S3Config{
		Bucket: "conductor-test", Region: "us-east-1",
		AccessKey: "AKIAEXAMPLE", SecretKey: "secretexample",
		Endpoint: u.Scheme + "://" + u.Host, PathStyle: true, Insecure: true,
	})
}

func TestS3RoundTrip(t *testing.T) {
	_, srv := newFakeS3(t)
	defer srv.Close()
	c := testClient(srv)
	ctx := context.Background()

	// Missing key.
	if _, err := c.Get(ctx, "sessions/manifest.json"); err != ErrNotFound {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	// Put then Get.
	want := []byte(`{"records":[{"id":"p123"}]}`)
	if err := c.Put(ctx, "sessions/manifest.json", want, "application/json"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.Get(ctx, "sessions/manifest.json")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("round-trip mismatch: %s", got)
	}
	// A key with a space exercises path escaping in the canonical URI.
	if err := c.Put(ctx, "sessions/snap 1.json", []byte("x"), "application/json"); err != nil {
		t.Fatalf("Put spaced key: %v", err)
	}
	keys, err := c.List(ctx, "sessions/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("List returned %v, want 2 keys", keys)
	}
}

func TestS3NoCredentials(t *testing.T) {
	c := New(S3Config{Bucket: "b", Region: "us-east-1"})
	if err := c.Put(context.Background(), "k", []byte("v"), ""); err == nil ||
		!strings.Contains(err.Error(), "no S3 credentials") {
		t.Fatalf("expected a credentials error, got %v", err)
	}
}

// The signature must depend on every input that AWS folds into it: change any one and the
// signature must change; hold them all and it must reproduce exactly.
func TestSignatureIsSensitiveAndDeterministic(t *testing.T) {
	base := S3Config{Bucket: "b", Region: "us-east-1", AccessKey: "AK", SecretKey: "SK"}
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	sig := func(cfg S3Config, method, key string, body []byte) string {
		c := New(cfg)
		c.now = func() time.Time { return fixed }
		req, err := c.newRequest(context.Background(), method, key, body)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := c.sign(req, body); err != nil {
			t.Fatal(err)
		}
		return req.Header.Get("Authorization")
	}
	ref := sig(base, http.MethodPut, "a/b.json", []byte("hello"))
	if ref == "" {
		t.Fatal("no Authorization produced")
	}
	if again := sig(base, http.MethodPut, "a/b.json", []byte("hello")); again != ref {
		t.Errorf("signing is not deterministic:\n %s\n %s", ref, again)
	}
	// Each of these must move the signature.
	cases := map[string]string{
		"method": sig(base, http.MethodGet, "a/b.json", []byte("hello")),
		"key":    sig(base, http.MethodPut, "a/c.json", []byte("hello")),
		"body":   sig(base, http.MethodPut, "a/b.json", []byte("HELLO")),
		"secret": sig(S3Config{Bucket: "b", Region: "us-east-1", AccessKey: "AK", SecretKey: "SK2"}, http.MethodPut, "a/b.json", []byte("hello")),
		"region": sig(S3Config{Bucket: "b", Region: "eu-west-1", AccessKey: "AK", SecretKey: "SK"}, http.MethodPut, "a/b.json", []byte("hello")),
	}
	for name, other := range cases {
		if other == ref {
			t.Errorf("changing %s did not change the signature", name)
		}
	}
	// The scope and credential must name the region and service.
	if !strings.Contains(ref, "/us-east-1/s3/aws4_request") {
		t.Errorf("credential scope missing region/service: %s", ref)
	}
	// A session token, when present, must be signed.
	tokened := sig(S3Config{Bucket: "b", Region: "us-east-1", AccessKey: "AK", SecretKey: "SK", SessionToken: "TKN"},
		http.MethodPut, "a/b.json", []byte("hello"))
	if !strings.Contains(tokened, "x-amz-security-token") {
		t.Errorf("session token not folded into SignedHeaders: %s", tokened)
	}
}

func TestAWSURIEncode(t *testing.T) {
	cases := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"a/b.json", false, "a/b.json"},
		{"a/b.json", true, "a%2Fb.json"},
		{"snap 1.json", false, "snap%201.json"},
		{"~-_.chars", false, "~-_.chars"},
		{"a+b", true, "a%2Bb"},
		{"él", false, "%C3%A9l"},
	}
	for _, c := range cases {
		if got := awsURIEncode(c.in, c.encodeSlash); got != c.want {
			t.Errorf("awsURIEncode(%q, %v) = %q, want %q", c.in, c.encodeSlash, got, c.want)
		}
	}
}

func TestCanonicalQuerySorted(t *testing.T) {
	u, _ := url.Parse("https://h/?prefix=sessions/&list-type=2&a=1")
	got := canonicalQuery(u)
	want := "a=1&list-type=2&prefix=sessions%2F"
	if got != want {
		t.Errorf("canonicalQuery = %q, want %q", got, want)
	}
}

var _ = fmt.Sprintf

// TestAWSVectorGetObject pins the signer to AWS's published Signature Version 4 example for a
// GET Object request (docs: "Examples of the complete Version 4 signing process"). Matching the
// exact Authorization the docs show proves the canonical request, string-to-sign, scope, and
// signing-key chain equal what real S3 computes — the ground truth the round-trip and
// sensitivity tests cannot supply on their own.
func TestAWSVectorGetObject(t *testing.T) {
	// endpoint s3.amazonaws.com + bucket examplebucket => host examplebucket.s3.amazonaws.com,
	// exactly the host the published example signs (regionless, virtual-host style).
	c := New(S3Config{
		Bucket:    "examplebucket",
		Region:    "us-east-1",
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Endpoint:  "https://s3.amazonaws.com",
	})
	c.now = func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }

	req, err := c.newRequest(context.Background(), http.MethodGet, "test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	if err := c.sign(req, nil); err != nil {
		t.Fatal(err)
	}
	auth := req.Header.Get("Authorization")

	const wantSig = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	const wantSignedHeaders = "host;range;x-amz-content-sha256;x-amz-date"
	if !strings.Contains(auth, "Signature="+wantSig) {
		t.Errorf("signature mismatch against the AWS published vector.\n got:  %s\n want signature %s", auth, wantSig)
	}
	if !strings.Contains(auth, "SignedHeaders="+wantSignedHeaders) {
		t.Errorf("signed headers mismatch.\n got: %s\n want %s", auth, wantSignedHeaders)
	}
	if !strings.Contains(auth, "Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request") {
		t.Errorf("credential scope mismatch: %s", auth)
	}
}

// TestCanonicalURIEncodesReservedChars pins the fix for the EscapedPath under-encoding: a key
// with a reserved sub-delim must be percent-encoded in the canonical URI (encodeSlash=false).
func TestCanonicalURIEncodesReservedChars(t *testing.T) {
	cases := map[string]string{
		"/a/b.json":      "/a/b.json",
		"/team+a/x.json": "/team%2Ba/x.json",
		"/ns:1/x.json":   "/ns%3A1/x.json",
		"/a=b/x.json":    "/a%3Db/x.json",
		"/a b/x.json":    "/a%20b/x.json",
		"/él/x.json":     "/%C3%A9l/x.json",
	}
	for path, want := range cases {
		if got := canonicalURI(&url.URL{Path: path}); got != want {
			t.Errorf("canonicalURI(%q) = %q, want %q", path, got, want)
		}
	}
}
