// Package s3fake is an in-memory, path-style S3 endpoint for tests: PUT (with
// If-None-Match: *), GET, HEAD, DELETE, ListObjectsV2 with paging, and multipart uploads.
// It records which access key signed each request, so a test can check the credentials
// that were used without trusting the client's own report.
package s3fake

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Server is the fake. Create it with New and close it with Close.
type Server struct {
	*httptest.Server
	Bucket string

	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte
	nextID  int
	// AccessKeys is every access key ID that signed a request, in order.
	AccessKeys []string
	// Requests counts requests by method.
	Requests map[string]int
	// FailNext, when set, answers the next n requests with 500.
	FailNext int
}

// New starts a fake holding bucket.
func New(bucket string) *Server {
	f := &Server{Bucket: bucket, objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}, Requests: map[string]int{}}
	f.Server = httptest.NewServer(f)
	return f
}

// Object returns a stored object.
func (f *Server) Object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	return b, ok
}

// Keys lists stored keys under prefix, sorted.
func (f *Server) Keys(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Put stores an object directly.
func (f *Server) Put(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), body...)
}

func (f *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests[r.Method]++
	if f.FailNext > 0 {
		f.FailNext--
		http.Error(w, "injected failure", http.StatusInternalServerError)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=") {
		writeError(w, http.StatusForbidden, "AccessDenied", "request is not SigV4-signed")
		return
	}
	ak := strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 Credential=")
	ak, _, _ = strings.Cut(ak, "/")
	f.AccessKeys = append(f.AccessKeys, ak)

	prefix := "/" + f.Bucket
	if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
		writeError(w, http.StatusNotFound, "NoSuchBucket", "no such bucket")
		return
	}
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")
	q := r.URL.Query()

	switch {
	case r.Method == http.MethodGet && key == "" && q.Get("list-type") == "2":
		f.list(w, q.Get("prefix"), q.Get("continuation-token"), q.Get("max-keys"))
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.nextID++
		id := "upload-" + strconv.Itoa(f.nextID)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`,
			f.Bucket, key, id)
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		parts, ok := f.uploads[q.Get("uploadId")]
		n, err := strconv.Atoi(q.Get("partNumber"))
		if !ok || err != nil || n < 1 {
			writeError(w, http.StatusNotFound, "NoSuchUpload", "no such upload")
			return
		}
		body, _ := io.ReadAll(r.Body)
		parts[n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"part-%d-%d"`, n, len(body)))
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		parts, ok := f.uploads[q.Get("uploadId")]
		if !ok {
			writeError(w, http.StatusNotFound, "NoSuchUpload", "no such upload")
			return
		}
		var done struct {
			Parts []struct {
				PartNumber int    `xml:"PartNumber"`
				ETag       string `xml:"ETag"`
			} `xml:"Part"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := xml.Unmarshal(body, &done); err != nil || len(done.Parts) == 0 {
			writeError(w, http.StatusBadRequest, "MalformedXML", "bad completion")
			return
		}
		var all bytes.Buffer
		for i, p := range done.Parts {
			if p.PartNumber != i+1 {
				writeError(w, http.StatusBadRequest, "InvalidPartOrder", "parts out of order")
				return
			}
			b, ok := parts[p.PartNumber]
			if !ok || p.ETag != fmt.Sprintf(`"part-%d-%d"`, p.PartNumber, len(b)) {
				writeError(w, http.StatusBadRequest, "InvalidPart", "unknown part")
				return
			}
			all.Write(b)
		}
		f.objects[key] = all.Bytes()
		delete(f.uploads, q.Get("uploadId"))
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Key>%s</Key></CompleteMultipartUploadResult>`, key)
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		if r.Header.Get("If-None-Match") == "*" {
			if _, exists := f.objects[key]; exists {
				writeError(w, http.StatusPreconditionFailed, "PreconditionFailed", "object exists")
				return
			}
		}
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, len(body)))
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		body, ok := f.objects[key]
		if !ok {
			writeError(w, http.StatusNotFound, "NoSuchKey", "no such key")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodGet {
			// Write without the lock: a client streaming a large object must not stall every
			// other request, as it would not on a real bucket.
			f.mu.Unlock()
			_, _ = w.Write(body)
			f.mu.Lock()
		}
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func (f *Server) list(w http.ResponseWriter, prefix, after, maxKeys string) {
	limit := 1000
	if n, err := strconv.Atoi(maxKeys); err == nil && n > 0 && n < limit {
		limit = n
	}
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	truncated := len(keys) > limit
	if truncated {
		keys = keys[:limit]
	}
	type obj struct {
		Key  string `xml:"Key"`
		Size int    `xml:"Size"`
	}
	out := struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		Contents    []obj    `xml:"Contents"`
		IsTruncated bool     `xml:"IsTruncated"`
		NextToken   string   `xml:"NextContinuationToken,omitempty"`
	}{IsTruncated: truncated}
	for _, k := range keys {
		out.Contents = append(out.Contents, obj{Key: k, Size: len(f.objects[k])})
	}
	if truncated {
		out.NextToken = keys[len(keys)-1]
	}
	_ = xml.NewEncoder(w).Encode(out)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `<Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg)
}
