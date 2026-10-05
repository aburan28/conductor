// Package ssotest provides fake identity providers for tests: an OpenID Connect issuer
// (discovery, JWKS, authorization and token endpoints, RS256 or ES256 signing) and GitHub's
// OAuth endpoints. Both consent automatically, so a test drives a whole browser sign-in with
// an http.Client that follows redirects.
package ssotest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// User is the account the fake provider signs in as.
type User struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	// Groups, when set, is sent as the groups claim.
	Groups []string
	// UPN is Entra's user principal name (NewEntra only; defaults to Email).
	UPN string
}

// grant is an issued authorization code.
type grant struct {
	clientID, redirectURI, nonce, challenge string
	user                                    User
}

// OIDC is a fake OpenID Connect issuer.
type OIDC struct {
	Server       *httptest.Server
	Issuer       string
	ClientID     string
	ClientSecret string

	mu     sync.Mutex
	user   User
	key    crypto.Signer
	kid    string
	keys   []crypto.Signer // published in the JWKS
	kids   []string
	alg    string
	codes  map[string]grant
	tamper func(claims map[string]any)
	signer crypto.Signer // overrides key for signing, to forge a token
	// JWKSFetches counts key set requests, for rotation tests.
	JWKSFetches int
	// entraTenant, when set, shapes ID tokens like Microsoft Entra ID's.
	entraTenant string
}

// NewOIDC starts a fake issuer signing with a fresh 2048-bit RSA key.
func NewOIDC(t testing.TB) *OIDC { return NewOIDCAt(t, "") }

// NewEntra starts a fake issuer shaped like a single Microsoft Entra ID tenant: its issuer
// is <server>/<tenant>/v2.0, and its ID tokens carry tid, upn and preferred_username but no
// email_verified, as Entra's do.
func NewEntra(t testing.TB, tenant string) *OIDC {
	f := NewOIDCAt(t, "/"+tenant+"/v2.0")
	f.entraTenant = tenant
	return f
}

// NewOIDCAt starts a fake issuer whose issuer URL is the server's address plus path.
func NewOIDCAt(t testing.TB, path string) *OIDC {
	t.Helper()
	f := &OIDC{ClientID: "conductor-test", ClientSecret: "s3cret", codes: map[string]grant{}, alg: "RS256"}
	f.key, f.kid = newRSA(t), "k1"
	f.keys, f.kids = []crypto.Signer{f.key}, []string{f.kid}
	f.user = User{Subject: "sub-1", Email: "user@example.com", EmailVerified: true, Name: "User"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+path+"/.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET "+path+"/jwks", f.jwks)
	mux.HandleFunc("GET "+path+"/authorize", f.authorize)
	mux.HandleFunc("POST "+path+"/token", f.token)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	f.Issuer = f.Server.URL + path
	return f
}

func newRSA(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// SetUser chooses who the next sign-in is.
func (f *OIDC) SetUser(u User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.user = u
}

// Tamper edits the claims of the next ID tokens before they are signed; nil stops.
func (f *OIDC) Tamper(fn func(claims map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tamper = fn
}

// ForgeWith signs ID tokens with a key the JWKS does not publish (under the published kid).
func (f *OIDC) ForgeWith(t testing.TB) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signer = newRSA(t)
}

// UseES256 switches to a P-256 key, published alongside the RSA one.
func (f *OIDC) UseES256(t testing.TB) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key, f.kid, f.alg = k, "ec1", "ES256"
	f.keys, f.kids = append(f.keys, k), append(f.kids, "ec1")
}

// Rotate replaces the signing key with a new one under a new kid and withdraws the old one.
func (f *OIDC) Rotate(t testing.TB) {
	k := newRSA(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kid = "k" + strconv.Itoa(len(f.kids)+1)
	f.key, f.alg = k, "RS256"
	f.keys, f.kids = []crypto.Signer{k}, []string{f.kid}
}

func (f *OIDC) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                f.Issuer,
		"authorization_endpoint":                f.Issuer + "/authorize",
		"token_endpoint":                        f.Issuer + "/token",
		"jwks_uri":                              f.Issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
	})
}

func (f *OIDC) jwks(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.JWKSFetches++
	var keys []map[string]any
	for i, k := range f.keys {
		keys = append(keys, publicJWK(k.Public(), f.kids[i]))
	}
	writeJSON(w, map[string]any{"keys": keys})
}

func publicJWK(pub crypto.PublicKey, kid string) map[string]any {
	b64 := base64.RawURLEncoding.EncodeToString
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return map[string]any{"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
			"n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes())}
	case *ecdsa.PublicKey:
		x, y := make([]byte, 32), make([]byte, 32)
		k.X.FillBytes(x)
		k.Y.FillBytes(y)
		return map[string]any{"kty": "EC", "kid": kid, "use": "sig", "alg": "ES256", "crv": "P-256",
			"x": b64(x), "y": b64(y)}
	}
	return nil
}

// authorize consents at once and redirects back with a code.
func (f *OIDC) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != f.ClientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("redirect_uri") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	code := randomString()
	f.codes[code] = grant{clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"),
		nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), user: f.user}
	f.mu.Unlock()
	redirectWithCode(w, r, q.Get("redirect_uri"), code, q.Get("state"))
}

func redirectWithCode(w http.ResponseWriter, r *http.Request, redirectURI, code, state string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	rq := u.Query()
	rq.Set("code", code)
	rq.Set("state", state)
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// token redeems a code, checking client authentication, redirect URI and PKCE.
func (f *OIDC) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id != url.QueryEscape(f.ClientID) || secret != url.QueryEscape(f.ClientSecret) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "invalid_client"})
		return
	}
	_ = r.ParseForm()
	f.mu.Lock()
	g, found := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	f.mu.Unlock()
	if !found || g.redirectURI != r.PostForm.Get("redirect_uri") || !pkceOK(r.PostForm.Get("code_verifier"), g.challenge) {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "invalid_grant"})
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": f.Issuer, "sub": g.user.Subject, "aud": f.ClientID,
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
		"nonce": g.nonce, "email": g.user.Email, "email_verified": g.user.EmailVerified,
		"name": g.user.Name,
	}
	if g.user.Groups != nil {
		claims["groups"] = g.user.Groups
	}
	f.mu.Lock()
	if f.entraTenant != "" {
		// Entra ID: no email_verified at all, the tenant, and the sign-in name.
		delete(claims, "email_verified")
		upn := g.user.UPN
		if upn == "" {
			upn = g.user.Email
		}
		claims["tid"], claims["upn"], claims["preferred_username"] = f.entraTenant, upn, upn
	}
	if f.tamper != nil {
		f.tamper(claims)
	}
	signer, kid, alg := f.key, f.kid, f.alg
	if f.signer != nil {
		signer, alg = f.signer, "RS256"
	}
	f.mu.Unlock()
	writeJSON(w, map[string]any{"access_token": "at-" + randomString(), "token_type": "Bearer",
		"id_token": Sign(signer, alg, kid, claims)})
}

// Sign builds a compact JWS over claims.
func Sign(key crypto.Signer, alg, kid string, claims map[string]any) string {
	b64 := base64.RawURLEncoding.EncodeToString
	head, _ := json.Marshal(map[string]any{"alg": alg, "kid": kid, "typ": "JWT"})
	body, _ := json.Marshal(claims)
	input := b64(head) + "." + b64(body)
	sum := sha256.Sum256([]byte(input))
	var sig []byte
	switch k := key.(type) {
	case *rsa.PrivateKey:
		sig, _ = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	case *ecdsa.PrivateKey:
		r, s, _ := ecdsa.Sign(rand.Reader, k, sum[:])
		sig = make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
	}
	return input + "." + b64(sig)
}

func pkceOK(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	return verifier != "" && base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

func randomString() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// GitHub
// ---------------------------------------------------------------------------

// GitHubUser is the account the fake GitHub signs in as.
type GitHubUser struct {
	ID    int64
	Login string
	// Emails lists the account's addresses; the first verified primary one is used.
	Emails []GitHubEmail
	// Orgs maps an organization to the membership state ("active", "pending").
	Orgs map[string]string
	// Teams maps an organization to the slugs of the account's teams in it.
	Teams map[string][]string
}

// GitHubEmail is one entry of /user/emails.
type GitHubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// GitHub is a fake GitHub serving the OAuth web flow and the user API on one server.
type GitHub struct {
	Server       *httptest.Server
	ClientID     string
	ClientSecret string

	mu     sync.Mutex
	user   GitHubUser
	codes  map[string]grant
	tokens map[string]GitHubUser
}

// NewGitHub starts a fake GitHub.
func NewGitHub(t testing.TB) *GitHub {
	t.Helper()
	g := &GitHub{ClientID: "gh-client", ClientSecret: "gh-secret",
		codes: map[string]grant{}, tokens: map[string]GitHubUser{}}
	g.user = GitHubUser{ID: 4242, Login: "octo",
		Emails: []GitHubEmail{{Email: "octo@example.com", Primary: true, Verified: true}},
		Orgs:   map[string]string{"acme": "active"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", g.authorize)
	mux.HandleFunc("POST /login/oauth/access_token", g.accessToken)
	mux.HandleFunc("GET /user", g.withUser(func(w http.ResponseWriter, _ *http.Request, u GitHubUser) {
		writeJSON(w, map[string]any{"id": u.ID, "login": u.Login, "name": u.Login})
	}))
	mux.HandleFunc("GET /user/emails", g.withUser(func(w http.ResponseWriter, _ *http.Request, u GitHubUser) {
		writeJSON(w, u.Emails)
	}))
	mux.HandleFunc("GET /user/memberships/orgs/{org}", g.withUser(func(w http.ResponseWriter, r *http.Request, u GitHubUser) {
		state, ok := u.Orgs[r.PathValue("org")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"message": "Not Found"})
			return
		}
		writeJSON(w, map[string]any{"state": state})
	}))
	mux.HandleFunc("GET /user/memberships/orgs", g.withUser(func(w http.ResponseWriter, _ *http.Request, u GitHubUser) {
		out := []map[string]any{}
		for org, state := range u.Orgs {
			if state == "active" {
				out = append(out, map[string]any{"state": state, "organization": map[string]any{"login": org}})
			}
		}
		writeJSON(w, out)
	}))
	mux.HandleFunc("GET /user/teams", g.withUser(func(w http.ResponseWriter, _ *http.Request, u GitHubUser) {
		out := []map[string]any{}
		for org, slugs := range u.Teams {
			for _, slug := range slugs {
				out = append(out, map[string]any{"slug": slug, "organization": map[string]any{"login": org}})
			}
		}
		writeJSON(w, out)
	}))
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Server.Close)
	return g
}

// SetUser chooses who the next sign-in is.
func (g *GitHub) SetUser(u GitHubUser) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.user = u
}

func (g *GitHub) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != g.ClientID || q.Get("redirect_uri") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	code := randomString()
	g.codes[code] = grant{redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), user: User{}}
	g.tokens["pending:"+code] = g.user
	g.mu.Unlock()
	redirectWithCode(w, r, q.Get("redirect_uri"), code, q.Get("state"))
}

func (g *GitHub) accessToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	code := r.PostForm.Get("code")
	g.mu.Lock()
	defer g.mu.Unlock()
	gr, ok := g.codes[code]
	delete(g.codes, code)
	user := g.tokens["pending:"+code]
	delete(g.tokens, "pending:"+code)
	// GitHub reports a bad code as 200 with an error field.
	if !ok || r.PostForm.Get("client_id") != g.ClientID || r.PostForm.Get("client_secret") != g.ClientSecret ||
		gr.redirectURI != r.PostForm.Get("redirect_uri") ||
		(gr.challenge != "" && !pkceOK(r.PostForm.Get("code_verifier"), gr.challenge)) {
		writeJSON(w, map[string]any{"error": "bad_verification_code"})
		return
	}
	tok := "gho_" + randomString()
	g.tokens[tok] = user
	writeJSON(w, map[string]any{"access_token": tok, "token_type": "bearer"})
}

func (g *GitHub) withUser(fn func(http.ResponseWriter, *http.Request, GitHubUser)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		g.mu.Lock()
		u, ok := g.tokens[tok]
		g.mu.Unlock()
		if !ok || !strings.HasPrefix(tok, "gho_") {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"message": "Bad credentials"})
			return
		}
		fn(w, r, u)
	}
}
