package modelsrc

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/domain"
)

func TestListAnthropicPaginatesAndSendsHeaders(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		if r.Header.Get("x-api-key") != "k-test" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-a","display_name":"A","created_at":"2026-01-02T03:04:05Z","max_input_tokens":200000}],"has_more":true,"last_id":"claude-a"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-b","created_at":"2026-05-01T00:00:00Z"}],"has_more":false,"last_id":"claude-b"}`))
	}))
	defer srv.Close()

	got, err := NewHTTPLister().List(context.Background(), Target{Kind: domain.InferenceAnthropic, BaseURL: srv.URL, Key: "k-test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "claude-a" || got[1].ID != "claude-b" {
		t.Fatalf("models = %+v", got)
	}
	if got[0].ContextWindow != 200000 || got[0].Created.IsZero() || got[0].DisplayName != "A" {
		t.Fatalf("first model lost fields: %+v", got[0])
	}
	if len(seen) != 2 || !strings.HasPrefix(seen[0], "/v1/models") || !strings.Contains(seen[1], "after_id=claude-a") {
		t.Fatalf("requests = %v", seen)
	}
}

func TestListOpenAICompatibleBearerAndVersionJoin(t *testing.T) {
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-x","created":1767225600},{"id":"local/qwen","max_model_len":32768},{"id":""}]}`))
	}))
	defer srv.Close()

	for _, base := range []string{srv.URL, srv.URL + "/v1", srv.URL + "/v1/"} {
		got, err := NewHTTPLister().List(context.Background(), Target{Kind: domain.InferenceOpenAICompatible, BaseURL: base, Key: "sk-1"})
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if path != "/v1/models" || auth != "Bearer sk-1" {
			t.Fatalf("%s: path %q auth %q", base, path, auth)
		}
		if len(got) != 2 || got[0].Created.Year() != 2026 || got[1].ContextWindow != 32768 {
			t.Fatalf("%s: models = %+v", base, got)
		}
	}
}

func TestListOpenAIKeylessSendsNoAuthorization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, has := r.Header["Authorization"]; has {
			t.Errorf("keyless endpoint received an Authorization header")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	if _, err := NewHTTPLister().List(context.Background(), Target{Kind: domain.InferenceOpenAICompatible, BaseURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
}

func TestListOllama(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b","modified_at":"2026-09-01T10:00:00.123456789Z"},{"model":"llama:latest"}]}`))
	}))
	defer srv.Close()
	got, err := NewHTTPLister().List(context.Background(), Target{Kind: domain.InferenceOllama, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "qwen3:8b" || got[0].Created.IsZero() || got[1].ID != "llama:latest" {
		t.Fatalf("models = %+v", got)
	}
}

func TestListErrorsNameTheProblemWithoutEchoingTheBody(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"rejected key", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "secret-ish body sk-leak", http.StatusUnauthorized)
		}, "rejected the API key"},
		{"server error", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal path /srv/secret", http.StatusInternalServerError)
		}, "HTTP 500"},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://169.254.169.254/latest", http.StatusFound)
		}, "redirected"},
		{"not json", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>login</html>")) }, "not the expected JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			_, err := NewHTTPLister().List(context.Background(), Target{Kind: domain.InferenceOpenAI, BaseURL: srv.URL, Key: "k"})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			for _, leak := range []string{"sk-leak", "/srv/secret", "169.254"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("error %q echoes the response", err)
				}
			}
		})
	}
}

func TestValidateBaseURL(t *testing.T) {
	ok := []string{
		"https://api.anthropic.com", "https://api.openai.com/v1", "http://localhost:11434",
		"http://127.0.0.1:8000/v1", "http://192.168.1.20:1234/v1", "http://10.0.0.5", "http://gpu-box:8000",
		"http://gpu.local:8000", "https://llm.corp.example/v1",
	}
	for _, u := range ok {
		if err := ValidateBaseURL(u); err != nil {
			t.Errorf("ValidateBaseURL(%q) = %v, want ok", u, err)
		}
	}
	bad := []string{
		"", "api.anthropic.com", "ftp://x/y", "http://api.example.com", "http://8.8.8.8",
		"https://169.254.169.254/latest", "http://[fe80::1]:8000", "https://0.0.0.0",
		"https://user:pw@host/v1", "https://host/v1?key=1",
	}
	for _, u := range bad {
		if err := ValidateBaseURL(u); err == nil {
			t.Errorf("ValidateBaseURL(%q) = nil, want an error", u)
		}
	}
}

func TestProxyPathChecksTheDestinationBeforeAskingTheProxy(t *testing.T) {
	for _, h := range []string{"169.254.169.254", "0.0.0.0", "fe80::1"} {
		if err := checkDestination(context.Background(), h); err == nil {
			t.Errorf("checkDestination(%q) allowed a blocked address", h)
		}
	}
	for _, h := range []string{"127.0.0.1", "10.0.0.8", "93.184.216.34"} {
		if err := checkDestination(context.Background(), h); err != nil {
			t.Errorf("checkDestination(%q) = %v", h, err)
		}
	}

	// With a proxy in the environment the request is refused before the proxy ever hears of it.
	seen := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = true }))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	_, err := NewHTTPLister().List(context.Background(), Target{Kind: domain.InferenceOpenAI, BaseURL: "https://169.254.169.254/v1"})
	if err == nil || seen {
		t.Fatalf("a metadata address reached the proxy: err=%v proxyHit=%v", err, seen)
	}
}

func TestGuardDialRefusesMetadataAddresses(t *testing.T) {
	for _, a := range []string{"169.254.169.254:80", "0.0.0.0:80", "[fe80::1]:80"} {
		if err := guardDial("tcp", a, nil); err == nil {
			t.Errorf("guardDial(%q) allowed a blocked address", a)
		}
	}
	for _, a := range []string{"127.0.0.1:80", "10.1.2.3:443", "93.184.216.34:443"} {
		if err := guardDial("tcp", a, nil); err != nil {
			t.Errorf("guardDial(%q) = %v", a, err)
		}
	}
	if !privateOrLoopback(net.ParseIP("192.168.0.1")) || privateOrLoopback(net.ParseIP("8.8.8.8")) {
		t.Error("privateOrLoopback misclassified")
	}
}
