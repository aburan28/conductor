package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestIndexServedAtRootAndDeepLinks(t *testing.T) {
	h := Handler()
	for _, target := range []string{"/", "/tasks/T-1", "/usage", "/tasks/T-42?x=1"} {
		rec := get(t, h, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s: content-type %q", target, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: cache-control %q", target, cc)
		}
		if !strings.Contains(rec.Body.String(), "<title>Conductor</title>") {
			t.Fatalf("%s: body does not look like the index page", target)
		}
	}
	if string(Dashboard()) != get(t, h, "/").Body.String() {
		t.Fatal("Dashboard() should return the index page")
	}
}

func TestAssetsServedWithContentTypes(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"/static/app.js":         "text/javascript",
		"/static/app.css":        "text/css",
		"/static/lib/dom.js":     "text/javascript",
		"/static/views/usage.js": "text/javascript",
	}
	for target, want := range cases {
		rec := get(t, h, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, want) {
			t.Fatalf("%s: content-type %q, want prefix %q", target, ct, want)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
			t.Fatalf("%s: cache-control %q should be cacheable", target, cc)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s: empty body", target)
		}
	}
}

func TestMissingAssetIs404NotIndex(t *testing.T) {
	h := Handler()
	for _, target := range []string{"/static/missing.js", "/static/", "/static/../web.go", "/static/index.html"} {
		rec := get(t, h, target)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", target, rec.Code)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /: status %d", rec.Code)
	}
}

// TestNoExternalRequests is a privacy property, not a style check: a dashboard that loads a
// font or a script from a CDN tells that CDN which teams are running Conductor and when.
// The only URLs permitted in the bundle are XML namespaces (never fetched) and the loopback
// examples the Integrations page shows people how to type.
func TestNoExternalRequests(t *testing.T) {
	allowed := []string{
		"http://www.w3.org/", // SVG / XHTML namespaces
		"http://localhost",   // integration snippet examples
		"https://localhost",  //
		"http://127.0.0.1",   //
		"https://127.0.0.1",  //
	}
	urlPattern := regexp.MustCompile(`https?://[^\s"'<>)]+`)
	err := fs.WalkDir(staticFS, "static", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := staticFS.ReadFile(name)
		if err != nil {
			return err
		}
		for _, match := range urlPattern.FindAllString(string(body), -1) {
			ok := false
			for _, prefix := range allowed {
				if strings.HasPrefix(match, prefix) {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("%s references an external URL: %s", name, match)
			}
		}
		if strings.Contains(string(body), "<link") && strings.Contains(string(body), "fonts.") {
			t.Errorf("%s appears to load a remote font", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEveryModuleImportResolves(t *testing.T) {
	// Only real import/export statements count; prose inside UI strings can also contain
	// the word "from" followed by a quote.
	importPattern := regexp.MustCompile(`(?m)^\s*(?:import|export)[^'\n]*?from\s+'([^']+)'`)
	err := fs.WalkDir(staticFS, "static", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(name, ".js") {
			return err
		}
		body, err := staticFS.ReadFile(name)
		if err != nil {
			return err
		}
		for _, m := range importPattern.FindAllStringSubmatch(string(body), -1) {
			spec := m[1]
			if !strings.HasPrefix(spec, ".") {
				t.Errorf("%s imports a non-relative module %q", name, spec)
				continue
			}
			dir := name[:strings.LastIndex(name, "/")]
			target := resolve(dir, spec)
			if _, err := staticFS.ReadFile(target); err != nil {
				t.Errorf("%s imports %q, which does not exist (%s)", name, spec, target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func resolve(dir, spec string) string {
	parts := strings.Split(dir, "/")
	for _, seg := range strings.Split(spec, "/") {
		switch seg {
		case ".":
		case "..":
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/")
}
