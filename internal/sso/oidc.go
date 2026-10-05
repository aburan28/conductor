package sso

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// OpenID Connect: discovery, the authorization code flow with PKCE, and ID token
// verification (OpenID Connect Core 1.0 §3.1).

// clockSkew is how far the issuer's clock may disagree with this one.
const clockSkew = 2 * time.Minute

// discoveryMaxAge is how long a discovery document is used before it is fetched again.
const discoveryMaxAge = time.Hour

// maxResponse bounds every response read from a provider.
const maxResponse = 1 << 20

type metadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

type oidcProvider struct {
	cfg  Config
	opts Options

	mu       sync.Mutex
	meta     *metadata
	metaAt   time.Time
	keys     *keySet
	keysFrom string
}

func newOIDC(cfg Config, opts Options) *oidcProvider {
	return &oidcProvider{cfg: cfg, opts: opts}
}

func (p *oidcProvider) Config() Config { return p.cfg }

// discover returns the issuer's metadata, fetching it when absent or stale.
func (p *oidcProvider) discover(ctx context.Context) (*metadata, *keySet, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meta != nil && p.opts.Now().Sub(p.metaAt) < discoveryMaxAge {
		return p.meta, p.keys, nil
	}
	var m metadata
	wellKnown := strings.TrimRight(p.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	if err := getJSON(ctx, p.opts.HTTPClient, wellKnown, "", &m); err != nil {
		if p.meta != nil {
			// A stale document beats an outage: endpoints and the key set URI almost never
			// move, and the keys themselves are refreshed separately.
			return p.meta, p.keys, nil
		}
		return nil, nil, fail(CodeUpstream, "the identity provider could not be reached", err)
	}
	// The issuer in the document must be the configured issuer, character for character
	// (OpenID Connect Discovery §4.3); otherwise one issuer could vouch for another.
	if m.Issuer != p.cfg.Issuer {
		return nil, nil, fail(CodeUpstream, "the identity provider is misconfigured",
			fmt.Errorf("discovery document names issuer %q, configured %q", m.Issuer, p.cfg.Issuer))
	}
	for what, u := range map[string]string{"authorization_endpoint": m.AuthorizationEndpoint,
		"token_endpoint": m.TokenEndpoint, "jwks_uri": m.JWKSURI} {
		if err := checkEndpoint(what, u); err != nil {
			return nil, nil, fail(CodeUpstream, "the identity provider is misconfigured", err)
		}
	}
	if len(m.CodeChallengeMethods) > 0 && !slices.Contains(m.CodeChallengeMethods, "S256") {
		return nil, nil, fail(CodeUpstream, "the identity provider does not support PKCE (S256)", nil)
	}
	if p.keys == nil || p.keysFrom != m.JWKSURI {
		p.keys = &keySet{uri: m.JWKSURI, http: p.opts.HTTPClient, now: p.opts.Now}
		p.keysFrom = m.JWKSURI
	}
	p.meta, p.metaAt = &m, p.opts.Now()
	return p.meta, p.keys, nil
}

func (p *oidcProvider) AuthCodeURL(ctx context.Context, req AuthRequest) (string, error) {
	m, _, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(m.AuthorizationEndpoint)
	if err != nil {
		return "", fail(CodeUpstream, "the identity provider is misconfigured", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.cfg.ClientID)
	q.Set("redirect_uri", req.RedirectURI)
	q.Set("scope", "openid email profile")
	q.Set("state", req.State)
	q.Set("nonce", req.Nonce)
	q.Set("code_challenge", req.Challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (p *oidcProvider) Exchange(ctx context.Context, code, verifier, nonce, redirectURI string) (Identity, error) {
	m, keys, err := p.discover(ctx)
	if err != nil {
		return Identity{}, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	// client_secret_basic is the default every issuer must support (OpenID Connect Core
	// §9); client_secret_post only when the issuer says basic is not on offer.
	basic := len(m.TokenAuthMethods) == 0 || slices.Contains(m.TokenAuthMethods, "client_secret_basic")
	if !basic {
		form.Set("client_id", p.cfg.ClientID)
		form.Set("client_secret", p.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, fail(CodeUpstream, "the identity provider is misconfigured", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		// RFC 6749 §2.3.1: both halves are form-encoded before they are joined.
		req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	}
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := doJSON(p.opts.HTTPClient, req, &tok); err != nil {
		return Identity{}, fail(CodeUpstream, "the identity provider refused the sign-in", err)
	}
	if tok.IDToken == "" {
		return Identity{}, fail(CodeUpstream, "the identity provider returned no ID token", errors.New(tok.Error))
	}
	claims, err := p.verify(ctx, m, keys, tok.IDToken, nonce)
	if err != nil {
		return Identity{}, err
	}
	return p.identity(claims)
}

// idClaims are the ID token claims this package reads.
type idClaims struct {
	Iss               string          `json:"iss"`
	Sub               string          `json:"sub"`
	Aud               audience        `json:"aud"`
	Azp               string          `json:"azp"`
	Exp               *json.Number    `json:"exp"`
	Iat               *json.Number    `json:"iat"`
	Nbf               *json.Number    `json:"nbf"`
	Nonce             string          `json:"nonce"`
	Email             string          `json:"email"`
	EmailVerified     json.RawMessage `json:"email_verified"`
	Name              string          `json:"name"`
	PreferredUsername string          `json:"preferred_username"`
}

// audience is the aud claim, which is a string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func unixClaim(n *json.Number) (time.Time, bool) {
	if n == nil {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// verify checks an ID token's signature and claims (OpenID Connect Core §3.1.3.7).
func (p *oidcProvider) verify(ctx context.Context, m *metadata, keys *keySet, raw, nonce string) (idClaims, error) {
	var c idClaims
	h, payload, input, sig, err := parseJWS(raw)
	if err != nil {
		return c, invalidToken("%v", err)
	}
	if !supportedAlg(h.Alg) {
		return c, invalidToken("alg %q is not accepted", h.Alg)
	}
	if len(h.Crit) > 0 {
		return c, invalidToken("critical header parameters %v are not understood", h.Crit)
	}
	candidates, err := keys.find(ctx, h.Kid, h.Alg)
	if err != nil {
		return c, err
	}
	verified := false
	for _, key := range candidates {
		if verifySignature(h.Alg, key, input, sig) == nil {
			verified = true
			break
		}
	}
	if !verified {
		return c, invalidToken("signature does not verify under any of the issuer's keys")
	}

	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return c, invalidToken("claims: %v", err)
	}
	if c.Iss != m.Issuer {
		return c, invalidToken("issuer %q, want %q", c.Iss, m.Issuer)
	}
	if c.Sub == "" {
		return c, invalidToken("no subject")
	}
	if !slices.Contains([]string(c.Aud), p.cfg.ClientID) {
		return c, invalidToken("audience %v does not include this client", []string(c.Aud))
	}
	// With several audiences the token must say it was issued to this client (§3.1.3.7
	// rule 4-5); an azp naming anyone else means another client's token was replayed here.
	if (len(c.Aud) > 1 || c.Azp != "") && c.Azp != p.cfg.ClientID {
		return c, invalidToken("authorized party %q is not this client", c.Azp)
	}
	now := p.opts.Now()
	exp, ok := unixClaim(c.Exp)
	if !ok {
		return c, invalidToken("no expiry")
	}
	if !now.Before(exp.Add(clockSkew)) {
		return c, invalidToken("expired at %s", exp.UTC().Format(time.RFC3339))
	}
	iat, ok := unixClaim(c.Iat)
	if !ok {
		return c, invalidToken("no issue time")
	}
	if iat.After(now.Add(clockSkew)) {
		return c, invalidToken("issued in the future (%s)", iat.UTC().Format(time.RFC3339))
	}
	if nbf, ok := unixClaim(c.Nbf); ok && nbf.After(now.Add(clockSkew)) {
		return c, invalidToken("not valid before %s", nbf.UTC().Format(time.RFC3339))
	}
	// The nonce ties the token to the authorization request this server made; without it a
	// token captured from another sign-in could be replayed into this one.
	if nonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1 {
		return c, invalidToken("nonce does not match the sign-in request")
	}
	return c, nil
}

// identity applies the admission rules to verified claims.
func (p *oidcProvider) identity(c idClaims) (Identity, error) {
	email := strings.ToLower(strings.TrimSpace(c.Email))
	if email == "" || !emailVerified(c.EmailVerified) {
		return Identity{}, fail(CodeEmailUnverified,
			"the identity provider did not confirm a verified email address for this account", nil)
	}
	if err := p.cfg.checkDomain(email); err != nil {
		return Identity{}, err
	}
	return Identity{
		Provider: p.cfg.Name, Issuer: c.Iss, Subject: c.Sub, Email: email,
		Name: c.Name, Username: c.PreferredUsername,
	}, nil
}

// emailVerified reads email_verified, which is a boolean by the specification and the
// string "true" from some issuers. Anything else, including absence, is unverified.
func emailVerified(raw json.RawMessage) bool {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && strings.EqualFold(s, "true")
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

// getJSON fetches a JSON document, with a bearer token when one is given.
func getJSON(ctx context.Context, client *http.Client, u, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return doJSON(client, req, out)
}

// statusError is a non-2xx answer from a provider.
type statusError struct {
	Status int
	Body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// doJSON sends a request and decodes a bounded JSON answer.
func doJSON(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return &statusError{Status: resp.StatusCode, Body: snippet}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: %w", req.URL.Redacted(), err)
	}
	return nil
}
