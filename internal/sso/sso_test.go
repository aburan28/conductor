package sso_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/sso"
	"github.com/adamburan/conductor/internal/sso/ssotest"
)

const redirectURI = "https://conductor.example.com/v1/sso/test/callback"

func oidcProvider(t *testing.T, f *ssotest.OIDC, mutate func(*sso.Config)) sso.Provider {
	t.Helper()
	cfg := sso.Config{Name: "test", Kind: sso.KindOIDC, Issuer: f.Issuer, ClientID: f.ClientID, ClientSecret: f.ClientSecret}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := sso.New(cfg, sso.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// signIn runs the authorization request against the fake provider and returns the code it
// redirects back with.
func signIn(t *testing.T, p sso.Provider, nonce, verifier string) string {
	t.Helper()
	u, err := p.AuthCodeURL(context.Background(), sso.AuthRequest{
		State: "st-1", Nonce: nonce, Challenge: sso.Challenge(verifier), RedirectURI: redirectURI})
	if err != nil {
		t.Fatal(err)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if loc.Query().Get("state") != "st-1" {
		t.Fatalf("state not returned: %s", loc)
	}
	return loc.Query().Get("code")
}

func exchange(t *testing.T, p sso.Provider) (sso.Identity, error) {
	t.Helper()
	verifier := sso.NewVerifier()
	code := signIn(t, p, "nonce-1", verifier)
	return p.Exchange(context.Background(), code, verifier, "nonce-1", redirectURI)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("sign-in succeeded, want %s", code)
	}
	t.Logf("refused as expected: %v", err)
	if got := sso.ErrorCode(err); got != code {
		t.Fatalf("error code = %s, want %s (%v)", got, code, err)
	}
}

func TestOIDCHappyPath(t *testing.T) {
	f := ssotest.NewOIDC(t)
	id, err := exchange(t, oidcProvider(t, f, nil))
	if err != nil {
		t.Fatal(err)
	}
	if id.Issuer != f.Issuer || id.Subject != "sub-1" || id.Email != "user@example.com" || id.Provider != "test" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestOIDCAcceptsES256(t *testing.T) {
	f := ssotest.NewOIDC(t)
	f.UseES256(t)
	if _, err := exchange(t, oidcProvider(t, f, nil)); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCRejectsBadTokens(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(map[string]any)
		forge  bool
		code   string
	}{
		{name: "bad signature", forge: true, code: sso.CodeInvalidToken},
		{name: "wrong audience", tamper: func(c map[string]any) { c["aud"] = "someone-else" }, code: sso.CodeInvalidToken},
		{name: "another client's azp", tamper: func(c map[string]any) {
			c["aud"] = []string{"conductor-test", "other"}
			c["azp"] = "other"
		}, code: sso.CodeInvalidToken},
		{name: "wrong issuer", tamper: func(c map[string]any) { c["iss"] = "https://evil.example.com" }, code: sso.CodeInvalidToken},
		{name: "expired", tamper: func(c map[string]any) { c["exp"] = time.Now().Add(-10 * time.Minute).Unix() }, code: sso.CodeInvalidToken},
		{name: "issued in the future", tamper: func(c map[string]any) { c["iat"] = time.Now().Add(time.Hour).Unix() }, code: sso.CodeInvalidToken},
		{name: "no expiry", tamper: func(c map[string]any) { delete(c, "exp") }, code: sso.CodeInvalidToken},
		{name: "nonce mismatch", tamper: func(c map[string]any) { c["nonce"] = "another-sign-in" }, code: sso.CodeInvalidToken},
		{name: "no nonce", tamper: func(c map[string]any) { delete(c, "nonce") }, code: sso.CodeInvalidToken},
		{name: "unverified email", tamper: func(c map[string]any) { c["email_verified"] = false }, code: sso.CodeEmailUnverified},
		{name: "email_verified absent", tamper: func(c map[string]any) { delete(c, "email_verified") }, code: sso.CodeEmailUnverified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ssotest.NewOIDC(t)
			if tc.forge {
				f.ForgeWith(t)
			}
			f.Tamper(tc.tamper)
			_, err := exchange(t, oidcProvider(t, f, nil))
			wantCode(t, err, tc.code)
		})
	}
}

func TestOIDCAcceptsStringEmailVerified(t *testing.T) {
	f := ssotest.NewOIDC(t)
	f.Tamper(func(c map[string]any) { c["email_verified"] = "true" })
	if _, err := exchange(t, oidcProvider(t, f, nil)); err != nil {
		t.Fatal(err)
	}
}

// Unsigned and HMAC-signed tokens are refused before a key is consulted: "none" needs no
// key at all, and HS256 would be keyed with the client secret, which is not the issuer's.
func TestOIDCRefusesUnsignedAndSymmetricTokens(t *testing.T) {
	f := ssotest.NewOIDC(t)
	p := oidcProvider(t, f, nil)
	b64 := base64.RawURLEncoding.EncodeToString
	claims := b64([]byte(`{"iss":"` + f.Issuer + `","sub":"s","aud":"conductor-test","exp":9999999999,"iat":1,"nonce":"n","email":"a@b.c","email_verified":true}`))
	for _, alg := range []string{"none", "HS256"} {
		raw := b64([]byte(`{"alg":"`+alg+`","kid":"k1"}`)) + "." + claims + "." + b64([]byte("sig"))
		_, err := sso.VerifyForTest(p, raw, "n")
		wantCode(t, err, sso.CodeInvalidToken)
	}
}

func TestOIDCDomainRestriction(t *testing.T) {
	f := ssotest.NewOIDC(t)
	allowed := oidcProvider(t, f, func(c *sso.Config) { c.Domains = []string{"example.com"} })
	if _, err := exchange(t, allowed); err != nil {
		t.Fatal(err)
	}
	other := oidcProvider(t, f, func(c *sso.Config) { c.Domains = []string{"acme.com"} })
	_, err := exchange(t, other)
	wantCode(t, err, sso.CodeDomainNotAllowed)
	// A suffix is not the domain.
	f.SetUser(ssotest.User{Subject: "s2", Email: "x@evil-example.com", EmailVerified: true})
	_, err = exchange(t, allowed)
	wantCode(t, err, sso.CodeDomainNotAllowed)
}

func TestOIDCKeyRotationRefetchesTheKeySet(t *testing.T) {
	f := ssotest.NewOIDC(t)
	now := time.Now()
	clock := func() time.Time { return now }
	p, err := sso.New(sso.Config{Name: "test", Issuer: f.Issuer, ClientID: f.ClientID, ClientSecret: f.ClientSecret, Kind: sso.KindOIDC},
		sso.Options{Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exchange(t, p); err != nil {
		t.Fatal(err)
	}
	f.Rotate(t)
	// Within the refetch bound an unknown kid does not hit the issuer again.
	_, err = exchange(t, p)
	wantCode(t, err, sso.CodeInvalidToken)
	now = now.Add(time.Minute)
	if _, err := exchange(t, p); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if f.JWKSFetches != 2 {
		t.Fatalf("JWKS fetched %d times, want 2", f.JWKSFetches)
	}
}

func TestOIDCRefusesAMismatchedDiscoveryIssuer(t *testing.T) {
	f := ssotest.NewOIDC(t)
	p := oidcProvider(t, f, func(c *sso.Config) { c.Issuer = f.Issuer + "/" })
	_, err := p.AuthCodeURL(context.Background(), sso.AuthRequest{State: "s", Nonce: "n", Challenge: "c", RedirectURI: redirectURI})
	wantCode(t, err, sso.CodeUpstream)
}

func TestOIDCCodeIsBoundToItsVerifier(t *testing.T) {
	f := ssotest.NewOIDC(t)
	p := oidcProvider(t, f, nil)
	code := signIn(t, p, "n", sso.NewVerifier())
	_, err := p.Exchange(context.Background(), code, sso.NewVerifier(), "n", redirectURI)
	wantCode(t, err, sso.CodeUpstream)
}

func githubProvider(t *testing.T, g *ssotest.GitHub, orgs ...string) sso.Provider {
	t.Helper()
	p, err := sso.New(sso.Config{Name: "github", Kind: sso.KindGitHub, ClientID: g.ClientID, ClientSecret: g.ClientSecret,
		APIURL: g.Server.URL, WebURL: g.Server.URL, Orgs: orgs}, sso.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGitHubSignIn(t *testing.T) {
	g := ssotest.NewGitHub(t)
	id, err := exchange(t, githubProvider(t, g, "acme"))
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "4242" || id.Email != "octo@example.com" || id.Username != "octo" || id.Issuer != g.Server.URL {
		t.Fatalf("identity = %+v", id)
	}
}

func TestGitHubOrgMembership(t *testing.T) {
	g := ssotest.NewGitHub(t)
	_, err := exchange(t, githubProvider(t, g, "other-org"))
	wantCode(t, err, sso.CodeOrgNotAllowed)

	g.SetUser(ssotest.GitHubUser{ID: 7, Login: "invitee",
		Emails: []ssotest.GitHubEmail{{Email: "i@example.com", Primary: true, Verified: true}},
		Orgs:   map[string]string{"acme": "pending"}})
	_, err = exchange(t, githubProvider(t, g, "acme"))
	wantCode(t, err, sso.CodeOrgNotAllowed)

	// Any one allowed organization is enough.
	g.SetUser(ssotest.GitHubUser{ID: 8, Login: "member",
		Emails: []ssotest.GitHubEmail{{Email: "m@example.com", Primary: true, Verified: true}},
		Orgs:   map[string]string{"beta": "active"}})
	if _, err := exchange(t, githubProvider(t, g, "acme", "beta")); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubNeedsAVerifiedPrimaryEmail(t *testing.T) {
	g := ssotest.NewGitHub(t)
	g.SetUser(ssotest.GitHubUser{ID: 9, Login: "x", Emails: []ssotest.GitHubEmail{
		{Email: "primary@example.com", Primary: true, Verified: false},
		{Email: "secondary@example.com", Primary: false, Verified: true},
	}})
	_, err := exchange(t, githubProvider(t, g))
	wantCode(t, err, sso.CodeEmailUnverified)
}

func TestParseSpec(t *testing.T) {
	env := map[string]string{"CONDUCTOR_SSO_GOOGLE_CLIENT_SECRET": "from-env", "MY_SECRET": "named"}
	getenv := func(k string) string { return env[k] }

	c, err := sso.ParseSpec("name=google,issuer=https://accounts.google.com,client-id=abc,domain=@Example.com,domain=b.org", getenv)
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientSecret != "from-env" || c.Kind != sso.KindOIDC || c.Label != "Google" ||
		strings.Join(c.Domains, ",") != "example.com,b.org" {
		t.Fatalf("config = %+v", c)
	}

	if _, err := sso.ParseSpec("name=google,issuer=https://accounts.google.com,client-id=abc,client-secret=x", getenv); err == nil ||
		!strings.Contains(err.Error(), "never accepted on the command line") {
		t.Fatalf("an inline secret was accepted: %v", err)
	}

	c, err = sso.ParseSpec("name=github,client-id=gh,client-secret-env=MY_SECRET,org=acme", getenv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != sso.KindGitHub || c.ClientSecret != "named" || c.APIURL != "https://api.github.com" {
		t.Fatalf("config = %+v", c)
	}

	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = sso.ParseSpec("name=okta,issuer=https://acme.okta.com,client-id=o,client-secret-file="+file, getenv)
	if err != nil || c.ClientSecret != "from-file" || c.Label != "Okta" {
		t.Fatalf("config = %+v, %v", c, err)
	}

	for _, bad := range []string{
		"name=Google,issuer=https://a.example.com,client-id=x,client-secret-env=MY_SECRET",    // name case
		"name=plain,issuer=http://idp.example.com,client-id=x,client-secret-env=MY_SECRET",    // not https
		"name=nosecret,issuer=https://idp.example.com,client-id=x",                            // no secret
		"name=o,issuer=https://idp.example.com,client-id=x,client-secret-env=MY_SECRET,org=a", // org on OIDC
		"name=o,issuer=https://idp.example.com,client-id=x,client-secret-env=MY_SECRET,bogus=1",
	} {
		if _, err := sso.ParseSpec(bad, getenv); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := sso.ParseSpecs([]string{
		"name=github,client-id=a,client-secret-env=MY_SECRET",
		"name=github,client-id=b,client-secret-env=MY_SECRET",
	}, getenv); err == nil {
		t.Error("a duplicate provider name was accepted")
	}
}

func TestErrorCodeDefaultsToUpstream(t *testing.T) {
	if sso.ErrorCode(errors.New("x")) != sso.CodeUpstream {
		t.Fatal("a plain error is not upstream")
	}
}
