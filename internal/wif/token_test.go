package wif

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeToken(aud string, expiry time.Time) string {
	raw, _ := json.Marshal(map[string]any{
		"iss": "https://issuer.example", "sub": "workload-1",
		"aud": aud, "iat": time.Now().Add(-time.Minute).Unix(), "exp": expiry.Unix(),
	})
	return "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsxIn0." +
		base64.RawURLEncoding.EncodeToString(raw) + ".c2ln"
}

func TestCheckedToken(t *testing.T) {
	now := time.Now()
	valid := fakeToken(Audience, now.Add(10*time.Minute))
	if got, err := checkedToken(valid, now); err != nil || got != valid {
		t.Fatalf("valid token: %v", err)
	}
	for _, token := range []string{
		"", "bad", fakeToken("https://elsewhere.example", now.Add(time.Hour)),
		fakeToken(Audience, now.Add(-time.Minute)),
		strings.Replace(valid, "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsxIn0",
			base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1"}`)), 1),
	} {
		if _, err := checkedToken(token, now); err == nil {
			t.Fatalf("accepted invalid token %q", token)
		}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGCPProviderRequestsFullAnthropicToken(t *testing.T) {
	token := fakeToken(Audience, time.Now().Add(time.Hour))
	p := GCPProvider{Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "metadata.google.internal" ||
			r.URL.Query().Get("audience") != Audience ||
			r.URL.Query().Get("format") != "full" ||
			r.Header.Get("Metadata-Flavor") != "Google" {
			t.Fatalf("wrong metadata request: %s %v", r.URL, r.Header)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(token))}, nil
	})}}
	got, err := p.Token(t.Context())
	if err != nil || got != token {
		t.Fatalf("GCP token: %v", err)
	}
}

func TestFileProviderRotatedSymlink(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "version-1")
	if err := os.WriteFile(tokenPath, []byte(fakeToken(Audience, time.Now().Add(time.Hour))), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "token")
	if err := os.Symlink(tokenPath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileProvider{Path: link}).Token(t.Context()); err != nil {
		t.Fatalf("rotating projected-token symlink rejected: %v", err)
	}
}

type failingProvider struct{}

func (failingProvider) Token(context.Context) (string, error) { return "", errors.New("unavailable") }

func TestTokenFileFailsClosed(t *testing.T) {
	if _, err := StartTokenFile(t.Context(), failingProvider{}); err == nil {
		t.Fatal("started without an identity token")
	}
	provider := FileProvider{Path: filepath.Join(t.TempDir(), "source")}
	if err := os.WriteFile(provider.Path, []byte(fakeToken(Audience, time.Now().Add(time.Hour))), 0600); err != nil {
		t.Fatal(err)
	}
	tf, err := StartTokenFile(t.Context(), provider)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(tf.Path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("token file permissions: %v, %v", info, err)
	}
	if err := tf.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tf.Path); !os.IsNotExist(err) {
		t.Fatalf("token remained after stop: %v", err)
	}
}
