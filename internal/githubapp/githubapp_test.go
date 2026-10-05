package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	return k, string(p)
}

func TestAppJWTVerifiesWithThePublicKey(t *testing.T) {
	k, _ := testKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok, err := AppJWT(42, k, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", tok)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "42" || claims.Iat != now.Unix()-60 || claims.Exp-claims.Iat > 600 {
		t.Errorf("claims = %+v; GitHub rejects a JWT living over ten minutes", claims)
	}
}

func TestParsePrivateKeyAcceptsPKCS8(t *testing.T) {
	k, _ := testKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	p := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := ParsePrivateKey(p); err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePrivateKey([]byte("not pem")); err == nil {
		t.Error("garbage parsed as a key")
	}
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	sig := Sign("s3cret", body)
	if !VerifySignature("s3cret", body, sig) {
		t.Error("a correct signature was rejected")
	}
	for name, c := range map[string]struct{ secret, header string }{
		"wrong secret":     {"other", sig},
		"no secret":        {"", sig},
		"no prefix":        {"s3cret", strings.TrimPrefix(sig, "sha256=")},
		"sha1 header":      {"s3cret", "sha1=abc"},
		"not hex":          {"s3cret", "sha256=zz"},
		"empty header":     {"s3cret", ""},
		"truncated digest": {"s3cret", sig[:20]},
	} {
		if VerifySignature(c.secret, body, c.header) {
			t.Errorf("%s: accepted", name)
		}
	}
	if VerifySignature("s3cret", append(body, ' '), sig) {
		t.Error("an altered body was accepted")
	}
}

func TestManifest(t *testing.T) {
	raw, err := Manifest(ManifestOptions{Name: "Conductor (laptop)", BaseURL: "http://127.0.0.1:8080/"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["redirect_url"] != "http://127.0.0.1:8080/github/callback" {
		t.Errorf("redirect_url = %v", m["redirect_url"])
	}
	hook := m["hook_attributes"].(map[string]any)
	if hook["active"] != false {
		t.Error("a manifest without a public webhook URL must leave the hook inactive")
	}
	perms := m["default_permissions"].(map[string]any)
	for p, level := range perms {
		if level == "write" && p != "checks" {
			t.Errorf("permission %s is %v; Conductor writes nothing but check runs", p, level)
		}
	}
	raw, _ = Manifest(ManifestOptions{Name: "c", BaseURL: "https://c.example", WebhookURL: "https://c.example/github/webhook"})
	_ = json.Unmarshal(raw, &m)
	if hook := m["hook_attributes"].(map[string]any); hook["active"] != true {
		t.Error("a public webhook URL should be active")
	}
	if _, err := Manifest(ManifestOptions{}); err == nil {
		t.Error("an empty manifest was accepted")
	}
	if got := ManifestFormURL("", "acme", "x y"); got != "https://github.com/organizations/acme/settings/apps/new?state=x+y" {
		t.Errorf("org form URL = %s", got)
	}
}

// fakeGitHub is just enough of the API to exercise the client.
type fakeGitHub struct {
	t        *testing.T
	key      *rsa.PublicKey
	tokens   atomic.Int32
	checkRun map[string]any
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	requireJWT := func(r *http.Request) bool {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(tok, ".")
		if len(parts) != 3 {
			return false
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		return rsa.VerifyPKCS1v15(f.key, crypto.SHA256, sum[:], sig) == nil
	}
	mux.HandleFunc("POST /app-manifests/{code}/conversions", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("code") != "good" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_, pemKey := testKey(f.t)
		json.NewEncoder(w).Encode(map[string]any{
			"id": 7, "slug": "conductor-laptop", "name": "Conductor (laptop)", "html_url": "https://github.com/apps/conductor-laptop",
			"client_id": "Iv1.x", "client_secret": "cs", "webhook_secret": "ws", "pem": pemKey,
			"owner": map[string]any{"login": "aburan28"},
		})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !requireJWT(r) {
			http.Error(w, `{"message":"bad jwt"}`, http.StatusUnauthorized)
			return
		}
		f.tokens.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("GET /repos/{o}/{r}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !requireJWT(r) {
			http.Error(w, `{"message":"bad jwt"}`, http.StatusUnauthorized)
			return
		}
		if r.PathValue("o") != "acme" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 99})
	})
	mux.HandleFunc("GET /repos/{o}/{r}/pulls/{n}/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghs_x" {
			http.Error(w, `{"message":"bad token"}`, http.StatusUnauthorized)
			return
		}
		var files []map[string]any
		if r.URL.Query().Get("page") == "1" {
			for i := 0; i < 100; i++ {
				files = append(files, map[string]any{"filename": "pkg/f" + string(rune('a'+i%26)) + ".go"})
			}
		} else {
			files = append(files, map[string]any{"filename": "new/name.go", "previous_filename": "old/name.go"})
		}
		json.NewEncoder(w).Encode(files)
	})
	mux.HandleFunc("POST /repos/{o}/{r}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &f.checkRun)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":4242}`))
	})
	mux.HandleFunc("PATCH /repos/{o}/{r}/check-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "4242" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.checkRun = nil
		_ = json.Unmarshal(body, &f.checkRun)
		f.checkRun["patched"] = true
		w.Write([]byte(`{"id":4242}`))
	})
	return mux
}

func TestClientAgainstFakeGitHub(t *testing.T) {
	k, pemKey := testKey(t)
	fake := &fakeGitHub{t: t, key: &k.PublicKey}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	ctx := context.Background()

	// The manifest conversion needs no credentials.
	anon, _ := New(srv.URL, Credentials{})
	creds, err := anon.ConvertManifest(ctx, "good")
	if err != nil {
		t.Fatal(err)
	}
	if creds.AppID != 7 || creds.Slug != "conductor-laptop" || creds.Owner != "aburan28" || creds.WebhookSecret != "ws" {
		t.Errorf("converted credentials = %+v", creds)
	}
	if got := creds.InstallURL(""); got != "https://github.com/apps/conductor-laptop/installations/new" {
		t.Errorf("install URL = %s", got)
	}
	if _, err := anon.ConvertManifest(ctx, "bad"); err == nil {
		t.Error("an unknown manifest code converted")
	}
	if _, err := anon.ConvertManifest(ctx, "../x"); err == nil {
		t.Error("a code with path characters was sent")
	}

	c, err := New(srv.URL, Credentials{AppID: 7, PrivateKeyPEM: pemKey})
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.RepoInstallation(ctx, "acme", "widgets")
	if err != nil || id != 99 {
		t.Fatalf("RepoInstallation = %d, %v", id, err)
	}
	if _, err := c.RepoInstallation(ctx, "stranger", "x"); err == nil {
		t.Error("a repository the app is not installed on reported an installation")
	} else if ae, ok := err.(*APIError); !ok || !ae.NotFound() {
		t.Errorf("not-installed error = %v", err)
	}

	files, err := c.PullRequestFiles(ctx, 99, "acme", "widgets", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 102 || files[100] != "new/name.go" || files[101] != "old/name.go" {
		t.Errorf("files: %d, tail %v", len(files), files[len(files)-2:])
	}
	if fake.tokens.Load() != 1 {
		t.Errorf("installation token minted %d times; it should be cached", fake.tokens.Load())
	}

	runID, err := c.PostCheckRun(ctx, 99, "acme", "widgets", CheckRun{HeadSHA: "abc", Conclusion: "neutral", Title: "1 overlap", Summary: "s", Text: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if runID != 4242 || fake.checkRun["name"] != CheckName || fake.checkRun["head_sha"] != "abc" || fake.checkRun["conclusion"] != "neutral" {
		t.Errorf("check run %d body = %v", runID, fake.checkRun)
	}
	// A changed result updates the same run.
	if err := c.UpdateCheckRun(ctx, 99, "acme", "widgets", runID, CheckRun{Conclusion: "success", Title: "clear"}); err != nil {
		t.Fatal(err)
	}
	if fake.checkRun["patched"] != true || fake.checkRun["conclusion"] != "success" {
		t.Errorf("update body = %v", fake.checkRun)
	}
	var ae *APIError
	if err := c.UpdateCheckRun(ctx, 99, "acme", "widgets", 1, CheckRun{Conclusion: "success"}); !errors.As(err, &ae) || !ae.NotFound() {
		t.Errorf("updating a missing run = %v, want a not-found APIError", err)
	}
}

func TestSaveAndLoad(t *testing.T) {
	_, pemKey := testKey(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "github-app.json")
	if err := Save(p, Credentials{AppID: 3, Slug: "s", PrivateKeyPEM: pemKey, WebhookSecret: "w"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("credentials file mode = %v, want 0600", st.Mode().Perm())
	}
	got, ok, err := Load(p, func(string) string { return "" })
	if err != nil || !ok || got.AppID != 3 || got.WebhookSecret != "w" {
		t.Fatalf("Load = %+v, %v, %v", got, ok, err)
	}
	// The environment overrides the file.
	env := map[string]string{"CONDUCTOR_GITHUB_APP_ID": "9", "CONDUCTOR_GITHUB_WEBHOOK_SECRET": "env"}
	got, ok, err = Load(p, func(k string) string { return env[k] })
	if err != nil || !ok || got.AppID != 9 || got.WebhookSecret != "env" {
		t.Errorf("env override: %+v, %v, %v", got, ok, err)
	}
	// Nothing configured is not an error.
	if _, ok, err := Load(filepath.Join(dir, "missing.json"), func(string) string { return "" }); ok || err != nil {
		t.Errorf("missing file: ok=%v err=%v", ok, err)
	}
	// A broken key is.
	if err := Save(p, Credentials{AppID: 3, PrivateKeyPEM: "nope"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(p, func(string) string { return "" }); err == nil {
		t.Error("a credentials file with a bad key loaded")
	}
}

func TestParseRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/aburan28/conductor":        "aburan28/conductor",
		"https://github.com/aburan28/conductor.git":    "aburan28/conductor",
		"git@github.com:aburan28/conductor.git":        "aburan28/conductor",
		"ssh://git@github.com/aburan28/conductor":      "aburan28/conductor",
		"https://ghe.corp.example/team/repo.git":       "team/repo",
		"http://127.0.0.1:1234/git/aburan28/conductor": "aburan28/conductor",
	} {
		o, r, ok := ParseRemote(in)
		if !ok || o+"/"+r != want {
			t.Errorf("ParseRemote(%q) = %s/%s, %v; want %s", in, o, r, ok, want)
		}
	}
	for _, bad := range []string{"", "/local/path", "https://github.com/", "conductor"} {
		if _, _, ok := ParseRemote(bad); ok {
			t.Errorf("ParseRemote(%q) succeeded", bad)
		}
	}
}
