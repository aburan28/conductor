package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
)

// Local sign-in is the one unauthenticated way to obtain a token. These tests pin every
// condition it depends on, because each one is a hole if it regresses: a drive-by web page,
// a DNS-rebinding page, a request through a proxy, a server in enhanced mode.

// localServer starts a server with the given local-login options and makes alice the
// machine's owner, restoring the shared settings row afterwards (it is one row per database).
func localServer(t *testing.T, h *harness, opts Options) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	before, err := h.store.GetServerSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.store.Pool().Exec(context.Background(),
			`UPDATE server_settings SET local_owner_id = NULLIF($1,'')::uuid, security_mode = NULLIF($2,'') WHERE singleton`,
			before.LocalOwnerID, before.SecurityMode)
	})
	if _, err := h.store.SetLocalOwner(ctx, h.alice.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSecurityMode(ctx, ""); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(h.store, coord.New(h.store), opts).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// localPost sends POST /v1/local/session with optional header overrides.
func localPost(t *testing.T, srv *httptest.Server, headers map[string]string, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/local/session", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestLocalSignInMintsAWorkingTokenForTheOwner(t *testing.T) {
	h := newHarness(t)
	srv := localServer(t, h, Options{LocalLogin: LocalLoginOptions{DefaultMode: db.SecurityLocal}})

	code, out := localPost(t, srv, map[string]string{"Origin": srv.URL}, `{"client":"dashboard"}`)
	if code != http.StatusCreated {
		t.Fatalf("local sign-in = %d %v", code, out)
	}
	tok, _ := out["token"].(string)
	if !strings.HasPrefix(tok, db.TokenPrefix) || out["handle"] != "alice" {
		t.Fatalf("response = %v", out)
	}
	// The token is an ordinary one: it authenticates as the owner.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := srv.Client().Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("whoami with the local token = %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Signing in again from the same client replaces the previous token instead of piling up.
	_, again := localPost(t, srv, nil, `{"client":"dashboard"}`)
	tok2, _ := again["token"].(string)
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ = srv.Client().Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the replaced dashboard token still works (%d)", resp.StatusCode)
	}
	resp.Body.Close()
	if tok2 == "" || tok2 == tok {
		t.Error("a second sign-in did not mint a fresh token")
	}

	// Everything else still requires a token: local mode adds no ambient authentication.
	code, _ = h.doOn(srv, "", http.MethodGet, "/v1/whoami")
	if code != http.StatusUnauthorized {
		t.Errorf("tokenless whoami in local mode = %d, want 401", code)
	}
}

func TestLocalSignInRefusals(t *testing.T) {
	h := newHarness(t)
	srv := localServer(t, h, Options{LocalLogin: LocalLoginOptions{DefaultMode: db.SecurityLocal}})
	for name, c := range map[string]struct {
		headers map[string]string
		body    string
		want    int
	}{
		// A page on another site posting to localhost.
		"foreign origin": {map[string]string{"Origin": "https://evil.example"}, `{}`, http.StatusForbidden},
		// DNS rebinding: evil.example resolves to 127.0.0.1, so the TCP peer is loopback and
		// the browser thinks the request is same-origin — but the Host header gives it away.
		"rebinding host":   {map[string]string{"Host": "evil.example:8080", "Origin": "http://evil.example:8080"}, `{}`, http.StatusForbidden},
		"cross-site fetch": {map[string]string{"Sec-Fetch-Site": "cross-site"}, `{}`, http.StatusForbidden},
		// A cross-site HTML form can only send form encodings.
		"form post": {map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, `client=x`, http.StatusUnsupportedMediaType},
		// A loopback origin on a different port is a different site.
		"other local port": {map[string]string{"Origin": "http://127.0.0.1:1"}, `{}`, http.StatusForbidden},
		// A proxy nobody declared, in front of a loopback daemon: remote requests arrive from
		// loopback, but proxies announce themselves.
		"x-forwarded-for": {map[string]string{"X-Forwarded-For": "203.0.113.9"}, `{}`, http.StatusForbidden},
		"forwarded":       {map[string]string{"Forwarded": "for=203.0.113.9"}, `{}`, http.StatusForbidden},
		"via":             {map[string]string{"Via": "1.1 nginx"}, `{}`, http.StatusForbidden},
		"tailscale serve": {map[string]string{"Tailscale-User-Login": "brother@example.com"}, `{}`, http.StatusForbidden},
	} {
		code, out := localPost(t, srv, c.headers, c.body)
		if code != c.want || out["token"] != nil {
			t.Errorf("%s: %d %v, want %d and no token", name, code, out, c.want)
		}
	}

	// Stock nginx speaks HTTP/1.0 to its upstream and rewrites Host to the upstream address.
	if code := rawHTTP10(t, proxied0(t, h)); code != http.StatusForbidden {
		t.Errorf("HTTP/1.0 local sign-in = %d, want 403", code)
	}

	// Behind a proxy every peer is loopback, so the signal is worthless.
	proxied := localServer(t, h, Options{BehindProxy: true, LocalLogin: LocalLoginOptions{DefaultMode: db.SecurityLocal}})
	if code, _ := localPost(t, proxied, nil, `{}`); code != http.StatusForbidden {
		t.Errorf("behind a proxy = %d, want 403", code)
	}
	// A server whose default is enhanced (zero options, as any embedder gets) refuses.
	strict := localServer(t, h, Options{})
	if code, out := localPost(t, strict, nil, `{}`); code != http.StatusForbidden || out["code"] != "local_login_disabled" {
		t.Errorf("enhanced by default = %d %v", code, out)
	}
	// The public status endpoint tells a sign-in screen why.
	resp, err := strict.Client().Get(strict.URL + "/v1/local/status")
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st["security_mode"] != "enhanced" || st["local_login_available"] != false {
		t.Errorf("status = %v", st)
	}
	if _, named := st["owner"]; named {
		t.Error("the public status names the owner")
	}
}

func TestSecurityModeSwitching(t *testing.T) {
	h := newHarness(t)
	srv := localServer(t, h, Options{LocalLogin: LocalLoginOptions{DefaultMode: db.SecurityLocal}})
	_, out := localPost(t, srv, nil, `{"client":"cli"}`)
	localTok, _ := out["token"].(string)

	// bob (a contributor, not the owner, administers nothing) may not change it either way.
	if code, _ := h.doJSONOn(srv, h.bobTok, http.MethodPost, "/v1/security", map[string]string{"security_mode": "enhanced"}); code != http.StatusForbidden {
		t.Errorf("contributor tightening = %d, want 403", code)
	}
	// alice owns the machine: tightening revokes what local sign-in issued.
	code, body := h.doJSONOn(srv, h.aliceTok, http.MethodPost, "/v1/security", map[string]string{"security_mode": "enhanced"})
	if code != http.StatusOK || body["revoked_local_tokens"].(float64) < 1 {
		t.Fatalf("owner tightening = %d %v", code, body)
	}
	if code, _ := h.doOn(srv, localTok, http.MethodGet, "/v1/whoami"); code != http.StatusUnauthorized {
		t.Errorf("a local token survived enhanced mode (%d)", code)
	}
	if code, _ := h.doOn(srv, h.aliceTok, http.MethodGet, "/v1/whoami"); code != http.StatusOK {
		t.Errorf("a token minted on purpose was revoked too (%d)", code)
	}
	if code, _ := localPost(t, srv, nil, `{}`); code != http.StatusForbidden {
		t.Errorf("local sign-in after enhanced = %d, want 403", code)
	}

	// Loosening is the owner's alone, even against a project administrator: hand ownership
	// to bob, and alice (still an admin) can no longer turn local sign-in back on.
	if _, err := h.store.SetLocalOwner(context.Background(), h.bob.ID, false); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.doJSONOn(srv, h.aliceTok, http.MethodPost, "/v1/security", map[string]string{"security_mode": "local"}); code != http.StatusForbidden {
		t.Errorf("a non-owner admin loosening = %d, want 403", code)
	}
	if code, _ := h.doJSONOn(srv, h.bobTok, http.MethodPost, "/v1/security", map[string]string{"security_mode": "local"}); code != http.StatusOK {
		t.Errorf("owner loosening = %d", code)
	}

	// A mode pinned by flag cannot be changed at all.
	pinned := localServer(t, h, Options{LocalLogin: LocalLoginOptions{ForcedMode: db.SecurityEnhanced}})
	if code, _ := h.doJSONOn(pinned, h.aliceTok, http.MethodPost, "/v1/security", map[string]string{"security_mode": "local"}); code != http.StatusConflict {
		t.Errorf("changing a pinned mode = %d, want 409", code)
	}
}

func (h *harness) doOn(srv *httptest.Server, token, method, path string) (int, []byte) {
	h.t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (h *harness) doJSONOn(srv *httptest.Server, token, method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	encoded, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(string(encoded)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// proxied0 is a local-mode server for the raw HTTP/1.0 probe.
func proxied0(t *testing.T, h *harness) *httptest.Server {
	return localServer(t, h, Options{LocalLogin: LocalLoginOptions{DefaultMode: db.SecurityLocal}})
}

// rawHTTP10 sends POST /v1/local/session as HTTP/1.0 with a loopback Host, the way a default
// nginx proxy_pass would, and returns the status code.
func rawHTTP10(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"client":"cli"}`
	fmt.Fprintf(conn, "POST /v1/local/session HTTP/1.0\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", addr, len(body), body)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// A token from local sign-in — and anything it mints — stops working the moment the server
// is in enhanced mode, even when nothing revoked it (a daemon restarted with
// --security-mode enhanced).
func TestLocalTokensDieWithLocalMode(t *testing.T) {
	h := newHarness(t)
	srv := localServer(t, h, Options{LocalLogin: LocalLoginOptions{DefaultMode: db.SecurityLocal}})
	_, out := localPost(t, srv, nil, `{"client":"cli"}`)
	localTok, _ := out["token"].(string)
	code, minted := h.doJSONOn(srv, localTok, http.MethodPost, "/v1/tokens", map[string]any{"name": "ci"})
	if code != http.StatusCreated || minted["name"] != "local:ci" {
		t.Fatalf("a local token minted %v (%d); its child must stay in the local lineage", minted, code)
	}
	child, _ := minted["token"].(string)

	strict := httptest.NewServer(New(h.store, coord.New(h.store), Options{LocalLogin: LocalLoginOptions{ForcedMode: db.SecurityEnhanced}}).Handler())
	defer strict.Close()
	for name, tok := range map[string]string{"local token": localTok, "its child": child} {
		if code, _ := h.doOn(strict, tok, http.MethodGet, "/v1/whoami"); code != http.StatusUnauthorized {
			t.Errorf("%s under enhanced mode = %d, want 401", name, code)
		}
	}
	if code, _ := h.doOn(strict, h.aliceTok, http.MethodGet, "/v1/whoami"); code != http.StatusOK {
		t.Errorf("an ordinary token under enhanced mode = %d", code)
	}
	// Invented client names do not create extra credentials.
	_, a := localPost(t, srv, nil, `{"client":"x1"}`)
	_, b := localPost(t, srv, nil, `{"client":"x2"}`)
	if code, _ := h.doOn(srv, a["token"].(string), http.MethodGet, "/v1/whoami"); code != http.StatusUnauthorized {
		t.Errorf("two invented client names both kept a live token (%d)", code)
	}
	_ = b
}
