// Package sso signs people in through an external identity provider: any OpenID Connect
// issuer (Google, Okta, Auth0, Keycloak, …) or GitHub's OAuth flow, which is not OIDC for
// user login.
//
// It is protocol only. It turns an authorization code into a verified Identity — issuer,
// subject, verified email — and enforces the provider's own admission rules (allowed email
// domains, GitHub organizations). It never decides who that identity is in Conductor and
// never mints a credential: internal/api maps the identity to a principal and issues an
// ordinary bearer token, so every authorization rule that applies to a token applies to a
// sign-in unchanged (DESIGN.md §25.7).
//
// ID tokens are verified with the standard library alone. The verification is small and
// fully specified — a JWS signature under a key from the issuer's JWKS, and a handful of
// claim checks — and a dependency would add more surface than it removes.
package sso

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Kind is the protocol a provider speaks.
type Kind string

const (
	// KindOIDC is any OpenID Connect issuer with discovery.
	KindOIDC Kind = "oidc"
	// KindGitHub is GitHub's OAuth web flow (github.com or GitHub Enterprise Server).
	KindGitHub Kind = "github"
)

// Config describes one configured provider.
type Config struct {
	// Name identifies the provider in URLs, in token names (sso:<name>), and on the command
	// line. It is also what the redirect URI registered with the provider is built from.
	Name string
	// Label is the button text: "Sign in with <Label>".
	Label string
	Kind  Kind
	// Issuer is the OIDC issuer URL, exactly as the issuer's discovery document states it.
	Issuer       string
	ClientID     string
	ClientSecret string
	// Domains, when set, admit only verified email addresses in these domains.
	Domains []string
	// Orgs, for GitHub, admits only active members of at least one of these organizations.
	Orgs []string
	// APIURL and WebURL are GitHub's endpoints; GitHub Enterprise Server sets both.
	APIURL, WebURL string
}

// Restricted reports whether the provider admits only some of its accounts. An
// unrestricted provider still signs in only people an administrator registered, unless
// auto-provisioning is on — which is why auto-provisioning requires every provider to be
// restricted.
func (c Config) Restricted() bool { return len(c.Domains) > 0 || len(c.Orgs) > 0 }

// Identity is what a provider vouches for after a successful sign-in.
type Identity struct {
	// Provider is the configured name the sign-in went through.
	Provider string
	// Issuer and Subject together name the account, permanently. An email address does
	// not: it can be reassigned, and some providers let a user change it at will.
	Issuer  string
	Subject string
	// Email is verified by the provider (sign-in is refused otherwise), lowercased.
	Email string
	// Name and Username are display hints only and are never used to link accounts.
	Name     string
	Username string
}

// AuthRequest is what an authorization redirect carries.
type AuthRequest struct {
	State       string
	Nonce       string
	Challenge   string // PKCE S256 challenge
	RedirectURI string
}

// Provider is one configured identity provider.
type Provider interface {
	Config() Config
	// AuthCodeURL is where to send the browser to sign in.
	AuthCodeURL(ctx context.Context, req AuthRequest) (string, error)
	// Exchange redeems an authorization code and returns the verified identity. nonce is
	// the value the authorization request carried; verifier is its PKCE verifier.
	Exchange(ctx context.Context, code, verifier, nonce, redirectURI string) (Identity, error)
}

// Options are shared by every provider.
type Options struct {
	// HTTPClient calls the provider. Nil uses a client with a 10-second timeout.
	HTTPClient *http.Client
	// Now is the clock ID token times are checked against. Nil is time.Now.
	Now func() time.Time
}

// New builds a provider from its configuration.
func New(cfg Config, opts Options) (Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	switch cfg.Kind {
	case KindGitHub:
		return newGitHub(cfg, opts), nil
	default:
		return newOIDC(cfg, opts), nil
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// Error codes a sign-in can fail with.
const (
	// CodeUpstream is a failure talking to the provider: unreachable, a malformed answer,
	// a code it refused to redeem.
	CodeUpstream = "upstream"
	// CodeInvalidToken is an ID token that failed verification: signature, issuer,
	// audience, lifetime or nonce.
	CodeInvalidToken = "invalid_token"
	// CodeEmailUnverified means the provider did not vouch for an email address.
	CodeEmailUnverified = "email_unverified"
	// CodeDomainNotAllowed is a verified email outside the allowed domains.
	CodeDomainNotAllowed = "domain_not_allowed"
	// CodeOrgNotAllowed is a GitHub account in none of the allowed organizations.
	CodeOrgNotAllowed = "org_not_allowed"
)

// Error is a failed sign-in. Message is safe to show the person signing in; Err carries the
// detail for the server log and is never shown, since it can quote the provider's answer.
type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func fail(code, msg string, err error) error { return &Error{Code: code, Message: msg, Err: err} }

func invalidToken(format string, args ...any) error {
	return fail(CodeInvalidToken, "the identity provider's token could not be verified", fmt.Errorf(format, args...))
}

// ErrorCode returns the code of a sign-in error, or CodeUpstream for any other error.
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeUpstream
}

// ---------------------------------------------------------------------------
// Admission rules
// ---------------------------------------------------------------------------

// checkDomain applies the allowed-domain rule to a verified address.
func (c Config) checkDomain(email string) error {
	if len(c.Domains) == 0 {
		return nil
	}
	_, domain, ok := strings.Cut(email, "@")
	if ok {
		for _, d := range c.Domains {
			// An exact match only. A suffix match would admit evil-example.com for
			// example.com, and a subdomain is a different administrative domain.
			if strings.EqualFold(domain, d) {
				return nil
			}
		}
	}
	return fail(CodeDomainNotAllowed,
		fmt.Sprintf("%s is not in a domain allowed to sign in here (%s)", email, strings.Join(c.Domains, ", ")), nil)
}

// ---------------------------------------------------------------------------
// Randomness and PKCE
// ---------------------------------------------------------------------------

// Random returns n random bytes, base64url-encoded: states, nonces, tickets, binders.
func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms; a zero state would be a
		// security bug, so do not carry on with one.
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewVerifier returns a PKCE code verifier: 32 random bytes, 43 characters (RFC 7636 §4.1).
func NewVerifier() string { return Random(32) }

// Challenge is the S256 challenge for a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

var pkcePattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// ValidPKCE reports whether s is shaped like a verifier or an S256 challenge.
func ValidPKCE(s string) bool { return pkcePattern.MatchString(s) }

// ---------------------------------------------------------------------------
// Configuration parsing
// ---------------------------------------------------------------------------

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// SecretEnv is the environment variable a provider's client secret is read from when its
// spec names no other source: CONDUCTOR_SSO_<NAME>_CLIENT_SECRET, with the name uppercased
// and dashes as underscores.
func SecretEnv(name string) string {
	return "CONDUCTOR_SSO_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_CLIENT_SECRET"
}

// ParseSpec parses one --sso-provider value: comma-separated key=value pairs.
//
//	name=google,issuer=https://accounts.google.com,client-id=…,domain=example.com
//	name=github,type=github,client-id=…,org=acme
//
// Keys: name, type (oidc|github), label, issuer, client-id, client-secret-file,
// client-secret-env, domain and org (both repeatable), api-url and web-url (GitHub
// Enterprise). The client secret is never accepted inline: a command line is visible to
// every user on the machine through ps and /proc. It comes from client-secret-file, from the
// variable client-secret-env names, or by default from SecretEnv(name).
func ParseSpec(spec string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var c Config
	var secretFile, secretEnv string
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return c, fmt.Errorf("sso provider %q: %q is not key=value", spec, part)
		}
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		switch key {
		case "name":
			c.Name = value
		case "type", "kind":
			c.Kind = Kind(strings.ToLower(value))
		case "label":
			c.Label = value
		case "issuer":
			c.Issuer = value
		case "client-id":
			c.ClientID = value
		case "client-secret":
			return c, errors.New("sso provider: a client secret is never accepted on the command line, " +
				"where every user on the machine can read it; use client-secret-file=PATH, " +
				"client-secret-env=VAR, or set " + SecretEnv(firstNonEmpty(c.Name, "NAME")))
		case "client-secret-file":
			secretFile = value
		case "client-secret-env":
			secretEnv = value
		case "domain":
			c.Domains = append(c.Domains, strings.ToLower(strings.TrimPrefix(value, "@")))
		case "org":
			c.Orgs = append(c.Orgs, value)
		case "api-url":
			c.APIURL = value
		case "web-url":
			c.WebURL = value
		default:
			return c, fmt.Errorf("sso provider %q: unknown key %q", spec, key)
		}
	}
	if c.Kind == "" {
		c.Kind = KindOIDC
		if c.Name == "github" && c.Issuer == "" {
			c.Kind = KindGitHub
		}
	}
	switch {
	case secretFile != "":
		body, err := os.ReadFile(secretFile)
		if err != nil {
			return c, fmt.Errorf("sso provider %s: client secret: %w", c.Name, err)
		}
		c.ClientSecret = strings.TrimSpace(string(body))
	case secretEnv != "":
		c.ClientSecret = getenv(secretEnv)
		if c.ClientSecret == "" {
			return c, fmt.Errorf("sso provider %s: %s is empty", c.Name, secretEnv)
		}
	default:
		c.ClientSecret = getenv(SecretEnv(c.Name))
	}
	return c, c.Validate()
}

// ParseSpecs parses several specs and refuses duplicate names.
func ParseSpecs(specs []string, getenv func(string) string) ([]Config, error) {
	var out []Config
	seen := map[string]bool{}
	for _, spec := range specs {
		if strings.TrimSpace(spec) == "" {
			continue
		}
		c, err := ParseSpec(spec, getenv)
		if err != nil {
			return nil, err
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("sso provider %s is configured twice", c.Name)
		}
		seen[c.Name] = true
		out = append(out, c)
	}
	return out, nil
}

// Validate fills defaults and checks a configuration.
func (c *Config) Validate() error {
	if !namePattern.MatchString(c.Name) {
		return fmt.Errorf("sso provider name %q must be lowercase letters, digits and dashes (at most 32)", c.Name)
	}
	if c.ClientID == "" {
		return fmt.Errorf("sso provider %s: client-id is required", c.Name)
	}
	if c.ClientSecret == "" {
		return fmt.Errorf("sso provider %s: no client secret; set %s, or pass client-secret-file=PATH", c.Name, SecretEnv(c.Name))
	}
	for _, d := range c.Domains {
		if d == "" || strings.ContainsAny(d, "@/ ") {
			return fmt.Errorf("sso provider %s: %q is not a domain", c.Name, d)
		}
	}
	switch c.Kind {
	case KindOIDC:
		if len(c.Orgs) > 0 {
			return fmt.Errorf("sso provider %s: org= applies to GitHub only; restrict an OIDC provider with domain=", c.Name)
		}
		if err := checkEndpoint("issuer", c.Issuer); err != nil {
			return fmt.Errorf("sso provider %s: %w", c.Name, err)
		}
		if c.Label == "" {
			c.Label = labelFor(c.Issuer, c.Name)
		}
	case KindGitHub:
		if c.APIURL == "" {
			c.APIURL = "https://api.github.com"
		}
		if c.WebURL == "" {
			c.WebURL = "https://github.com"
		}
		c.APIURL, c.WebURL = strings.TrimRight(c.APIURL, "/"), strings.TrimRight(c.WebURL, "/")
		for name, u := range map[string]string{"api-url": c.APIURL, "web-url": c.WebURL} {
			if err := checkEndpoint(name, u); err != nil {
				return fmt.Errorf("sso provider %s: %w", c.Name, err)
			}
		}
		if c.Label == "" {
			c.Label = "GitHub"
		}
	default:
		return fmt.Errorf("sso provider %s: type must be oidc or github, not %q", c.Name, c.Kind)
	}
	return nil
}

// checkEndpoint requires https, except on loopback where a development identity provider
// (and this package's tests) run in plaintext.
func checkEndpoint(what, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s is required", what)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s %q is not a URL", what, raw)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && IsLoopbackHost(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("%s %q must use https", what, raw)
}

// IsLoopbackHost reports whether a host name or address is this machine.
func IsLoopbackHost(h string) bool {
	h = strings.Trim(strings.ToLower(h), "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// labelFor names well-known issuers on the sign-in button.
func labelFor(issuer, name string) string {
	switch u, _ := url.Parse(issuer); {
	case u == nil:
	case u.Host == "accounts.google.com":
		return "Google"
	case strings.HasSuffix(u.Host, ".okta.com"):
		return "Okta"
	case u.Host == "login.microsoftonline.com":
		return "Microsoft"
	case strings.HasSuffix(u.Host, ".auth0.com"):
		return "Auth0"
	}
	return name
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
