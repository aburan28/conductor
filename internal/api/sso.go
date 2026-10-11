package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/sso"
)

// Single sign-on (DESIGN.md §25.7).
//
// A sign-in through an identity provider ends the way every other sign-in does: with an
// ordinary bearer token, named sso:<provider>. SSO is a token issuer and nothing more — it
// adds no second authentication path through the API, so roles, project scopes, enhanced
// security mode and revocation apply to these tokens exactly as to any other.
//
// The flow, for the dashboard:
//
//  1. GET /v1/sso/{provider}/start records the sign-in (state, nonce, PKCE verifier) in
//     Postgres, sets a cookie binding it to this browser, and redirects to the provider.
//  2. The provider redirects to GET /v1/sso/{provider}/callback, which consumes the state
//     once, checks the cookie, redeems the code, verifies the identity, maps it to a
//     principal, and redirects to the dashboard with a one-time ticket in the fragment.
//  3. The dashboard POSTs the ticket to /v1/sso/redeem, which — given the same cookie —
//     mints the token.
//
// The CLI is the same with the cookie replaced by PKCE between the CLI and this server: it
// starts with POST /v1/sso/{provider}/start naming a loopback redirect and a challenge, the
// callback sends the ticket to that loopback address, and redemption needs the verifier.
//
// Mapping an identity to a principal follows three rules. An external account is named by
// issuer and subject, never by email. A first sign-in attaches to an existing principal only
// through an address an administrator registered on it, and only while that principal has
// no linked identity at all — so a second account, at this provider or another, can never
// attach itself to a principal someone already signs in to. Further identities are added by
// the signed-in principal (POST /v1/sso/{provider}/link), which proves control of both.

// SSOOptions configures single sign-on.
type SSOOptions struct {
	Providers []sso.Provider
	// PublicURL is where browsers reach this server; every redirect URI registered with a
	// provider is built from it. Empty uses Options.SelfEndpoint.
	PublicURL string
	// TokenTTL is how long a token from a sign-in lives; default 12 hours, at most 90 days.
	TokenTTL time.Duration
	// AutoProvisionRole, when set, gives a first sign-in that matches no registered address
	// a new account in DefaultProject with this role. Contributor or lower; empty is off.
	AutoProvisionRole domain.Role
	// DefaultProject is "org/project", by slug.
	DefaultProject string
}

const (
	// ssoStateTTL is how long a person has to finish signing in at the provider.
	ssoStateTTL = 10 * time.Minute
	// ssoTicketTTL is how long the dashboard or CLI has to redeem its ticket, which it does
	// at once.
	ssoTicketTTL = 2 * time.Minute
	// ssoTokenTTLDefault is the life of a token from a sign-in: a working day, after which
	// signing in again re-checks the provider's verdict (membership, allowed domain).
	ssoTokenTTLDefault = 12 * time.Hour

	ssoClientDashboard = "dashboard"
	ssoClientCLI       = "cli"
)

// autoProvisionRoles are the roles a self-service account may start with. Anything that can
// administer a project, maintain it, or run its work is granted by a person.
var autoProvisionRoles = map[domain.Role]bool{
	domain.RoleContributor: true, domain.RoleReviewer: true, domain.RoleObserver: true,
}

// ValidateSSOOptions checks a configuration before the server starts.
func ValidateSSOOptions(o SSOOptions) error {
	if o.AutoProvisionRole != "" {
		if !autoProvisionRoles[o.AutoProvisionRole] {
			return fmt.Errorf("--sso-auto-provision must be contributor, reviewer or observer, not %q", o.AutoProvisionRole)
		}
		if _, _, ok := strings.Cut(o.DefaultProject, "/"); !ok {
			return errors.New("--sso-auto-provision needs --sso-default-project ORG/PROJECT: the project new accounts join")
		}
		for _, p := range o.Providers {
			if !p.Config().Restricted() {
				return fmt.Errorf("--sso-auto-provision would admit every account at %s; restrict it with domain= (or org= for GitHub)",
					p.Config().Name)
			}
		}
	}
	if len(o.Providers) > 0 {
		u, err := url.Parse(o.PublicURL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("single sign-on needs --public-url: providers redirect browsers back to it")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && sso.IsLoopbackHost(u.Hostname())) {
			return fmt.Errorf("single sign-on needs an https --public-url (got %s); only a loopback address may use http", o.PublicURL)
		}
	}
	if o.TokenTTL < 0 {
		return errors.New("--sso-token-ttl cannot be negative")
	}
	return nil
}

type ssoState struct {
	opts      SSOOptions
	providers map[string]sso.Provider
	order     []string
	publicURL string
	// secure is set when browsers reach this server over https: the binding cookie is then
	// Secure and __Host- prefixed, so no other origin can plant or read it.
	secure bool
}

func newSSOState(opts SSOOptions, self string) *ssoState {
	st := &ssoState{opts: opts, providers: map[string]sso.Provider{}}
	st.publicURL = strings.TrimRight(firstNonEmpty(opts.PublicURL, self), "/")
	if u, err := url.Parse(st.publicURL); err == nil {
		st.secure = u.Scheme == "https"
	}
	for _, p := range opts.Providers {
		name := p.Config().Name
		if _, dup := st.providers[name]; !dup {
			st.order = append(st.order, name)
		}
		st.providers[name] = p
	}
	return st
}

func (st *ssoState) redirectURI(name string) string {
	return st.publicURL + "/v1/sso/" + name + "/callback"
}

func (st *ssoState) tokenTTL() time.Duration {
	switch ttl := st.opts.TokenTTL; {
	case ttl <= 0:
		return ssoTokenTTLDefault
	case ttl > tokenTTLDefault:
		return tokenTTLDefault
	default:
		return ttl
	}
}

func (st *ssoState) cookieName() string {
	if st.secure {
		return "__Host-conductor_sso"
	}
	return "conductor_sso"
}

func (st *ssoState) cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: st.cookieName(), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: st.secure, SameSite: http.SameSiteLaxMode}
}

// binderHash is the hash of the request's binding cookie, or "".
func (st *ssoState) binderHash(r *http.Request) string {
	c, err := r.Cookie(st.cookieName())
	if err != nil || c.Value == "" {
		return ""
	}
	return ssoHash(c.Value)
}

// ssoHash is how a secret that travels in a URL or a cookie is stored.
func ssoHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *Server) ssoRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("GET /v1/sso/providers", s.ssoProviders)
	m.HandleFunc("GET /v1/sso/{provider}/start", s.ssoStartBrowser)
	m.HandleFunc("POST /v1/sso/{provider}/start", s.ssoStartCLI)
	m.HandleFunc("POST /v1/sso/{provider}/link", auth(s.ssoStartLink))
	m.HandleFunc("GET /v1/sso/{provider}/callback", s.ssoCallback)
	m.HandleFunc("POST /v1/sso/redeem", s.ssoRedeem)
	m.HandleFunc("GET /v1/sso/status", auth(s.ssoStatus))
	m.HandleFunc("PUT /v1/projects/{project}/members/{handle}/email", auth(s.setMemberEmail))
	m.HandleFunc("DELETE /v1/projects/{project}/members/{handle}/identities/{provider}", auth(s.unlinkMemberIdentity))
}

type ssoProviderInfo struct {
	Name  string   `json:"name"`
	Label string   `json:"label"`
	Type  sso.Kind `json:"type"`
}

func (s *Server) ssoProviderList() []ssoProviderInfo {
	out := make([]ssoProviderInfo, 0, len(s.sso.order))
	for _, name := range s.sso.order {
		cfg := s.sso.providers[name].Config()
		out = append(out, ssoProviderInfo{Name: cfg.Name, Label: cfg.Label, Type: cfg.Kind})
	}
	return out
}

// ssoProviders is public: the sign-in screen needs it before anyone is signed in. It names
// the providers and nothing about their configuration.
func (s *Server) ssoProviders(w http.ResponseWriter, r *http.Request) {
	s.ok(w, r, http.StatusOK, map[string]any{"providers": s.ssoProviderList()})
}

func (s *Server) ssoProvider(r *http.Request) (sso.Provider, bool) {
	p, ok := s.sso.providers[r.PathValue("provider")]
	return p, ok
}

var errNoSuchProvider = fmt.Errorf("%w: no single sign-on provider by that name is configured here", domain.ErrNotFound)

// ssoBegin records a sign-in and returns the provider URL to send the browser to.
func (s *Server) ssoBegin(r *http.Request, p sso.Provider, l *db.SSOLogin) (string, error) {
	state, nonce, verifier := sso.Random(32), sso.Random(32), sso.NewVerifier()
	name := p.Config().Name
	authURL, err := p.AuthCodeURL(r.Context(), sso.AuthRequest{
		State: state, Nonce: nonce, Challenge: sso.Challenge(verifier), RedirectURI: s.sso.redirectURI(name),
	})
	if err != nil {
		return "", err
	}
	l.StateHash, l.Provider, l.Nonce, l.Verifier = ssoHash(state), name, nonce, verifier
	l.ExpiresAt = s.store.Now().Add(ssoStateTTL)
	return authURL, s.store.CreateSSOLogin(r.Context(), *l)
}

// ssoStartBrowser begins a dashboard sign-in. It is a plain link, so it answers in pages and
// redirects rather than JSON.
func (s *Server) ssoStartBrowser(w http.ResponseWriter, r *http.Request) {
	p, ok := s.ssoProvider(r)
	if !ok {
		s.ssoPage(w, r, http.StatusNotFound, errNoSuchProvider.Error())
		return
	}
	binder := sso.Random(32)
	login := db.SSOLogin{Client: ssoClientDashboard, BinderHash: ssoHash(binder),
		ReturnTo: safeNext(r.URL.Query().Get("next"))}
	authURL, err := s.ssoBegin(r, p, &login)
	if err != nil {
		s.logger.Warn("sso sign-in could not start", "provider", p.Config().Name, "error", err)
		s.ssoToDashboard(w, r, "/", "sso_error", ssoMessage(err))
		return
	}
	http.SetCookie(w, s.sso.cookie(binder, int(ssoStateTTL.Seconds())))
	http.Redirect(w, r, authURL, http.StatusFound)
}

type ssoStartBody struct {
	RedirectURI         string `json:"redirect_uri"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
}

// ssoStartCLI begins a command-line sign-in.
func (s *Server) ssoStartCLI(w http.ResponseWriter, r *http.Request) {
	s.ssoStartNative(w, r, "")
}

// ssoStartLink begins adding a provider's account to the caller's principal.
func (s *Server) ssoStartLink(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	// A token from local sign-in is good only while local sign-in is. Linking through one
	// would leave behind a way in — the linked account — that outlives the switch to
	// enhanced mode, so it takes a credential that would survive the switch itself.
	if strings.HasPrefix(tokenName(r), db.LocalTokenPrefix) {
		s.fail(w, r, fmt.Errorf("%w: link an identity with a token, not with a local sign-in session", domain.ErrNotPermitted))
		return
	}
	if p.Kind != domain.PrincipalHuman {
		s.fail(w, r, fmt.Errorf("%w: single sign-on is for people; this is a %s", domain.ErrNotPermitted, p.Kind))
		return
	}
	s.ssoStartNative(w, r, p.ID)
}

func (s *Server) ssoStartNative(w http.ResponseWriter, r *http.Request, linkPrincipal domain.ID) {
	p, ok := s.ssoProvider(r)
	if !ok {
		s.fail(w, r, errNoSuchProvider)
		return
	}
	var body ssoStartBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect, ok := loopbackRedirect(body.RedirectURI)
	if !ok {
		s.fail(w, r, fmt.Errorf("%w: redirect_uri must be http://127.0.0.1:PORT/… or http://[::1]:PORT/… on the machine running the CLI",
			domain.ErrInvalidArgument))
		return
	}
	if body.CodeChallengeMethod != "S256" || !sso.ValidPKCE(body.CodeChallenge) {
		s.fail(w, r, fmt.Errorf("%w: a PKCE code_challenge with code_challenge_method S256 is required", domain.ErrInvalidArgument))
		return
	}
	login := db.SSOLogin{Client: ssoClientCLI, ClientChallenge: body.CodeChallenge, ReturnTo: redirect,
		LinkPrincipal: linkPrincipal}
	authURL, err := s.ssoBegin(r, p, &login)
	if err != nil {
		s.ssoFail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusCreated, map[string]any{
		"authorization_url": authURL, "expires_at": login.ExpiresAt.UTC(),
	})
}

// ssoCallback is where the provider sends the browser back.
func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request) {
	client := clientKey(r, s.behindProxy)
	if allowed, retryAfter := s.limiter.allow(client); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		s.ssoPage(w, r, http.StatusTooManyRequests, "Too many failed sign-in attempts from this address. Wait a minute and try again.")
		return
	}
	q := r.URL.Query()
	var login db.SSOLogin
	found := false
	if state := q.Get("state"); state != "" {
		var err error
		if login, found, err = s.store.ConsumeSSOState(r.Context(), ssoHash(state)); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if !found || login.Provider != r.PathValue("provider") {
		s.limiter.fail(client)
		s.ssoPage(w, r, http.StatusBadRequest,
			"This sign-in has expired or was already completed. Start again from the sign-in page.")
		return
	}
	// From here on the sign-in is known, so a refusal goes back to whoever started it.
	refuse := func(code, msg string, err error) {
		s.limiter.fail(client)
		s.logger.Info("sso sign-in refused", "provider", login.Provider, "client", login.Client,
			"code", code, "error", err)
		s.ssoReturn(w, r, login, "", code, msg)
	}
	p, ok := s.sso.providers[login.Provider]
	if !ok {
		refuse("not_configured", errNoSuchProvider.Error(), nil)
		return
	}
	if login.Client == ssoClientDashboard &&
		subtle.ConstantTimeCompare([]byte(s.sso.binderHash(r)), []byte(login.BinderHash)) != 1 {
		// Without this check, anyone could start a sign-in, stop at the provider's redirect,
		// and send the link to a victim, whose browser would then finish signing in as the
		// attacker (login CSRF).
		refuse("browser_mismatch", "This sign-in was started in a different browser, or at an address other than "+
			s.sso.publicURL+". Start it again from "+s.sso.publicURL+".", nil)
		return
	}
	if e := q.Get("error"); e != "" {
		refuse("provider_error", "The identity provider did not sign you in ("+printable(e, 64)+").", nil)
		return
	}
	code := q.Get("code")
	if code == "" {
		refuse("provider_error", "The identity provider returned no authorization code.", nil)
		return
	}
	ident, err := p.Exchange(r.Context(), code, login.Verifier, login.Nonce, s.sso.redirectURI(login.Provider))
	if err != nil {
		refuse(sso.ErrorCode(err), ssoMessage(err), err)
		return
	}
	principal, err := s.ssoResolve(r, p, ident, login.LinkPrincipal)
	var refusal *ssoRefusal
	switch {
	case errors.As(err, &refusal):
		refuse(refusal.code, refusal.msg, nil)
		return
	case err != nil:
		s.ssoServerError(w, r, login, err)
		return
	}
	ticket := sso.Random(32)
	if err := s.store.IssueSSOTicket(r.Context(), login.StateHash, ssoHash(ticket), principal.ID, ssoTicketTTL); err != nil {
		s.ssoServerError(w, r, login, err)
		return
	}
	s.limiter.succeed(client)
	s.ssoReturn(w, r, login, ticket, "", "")
}

// ssoReturn sends the browser back to whoever started the sign-in, with a ticket or a reason.
func (s *Server) ssoReturn(w http.ResponseWriter, r *http.Request, l db.SSOLogin, ticket, code, msg string) {
	if l.Client == ssoClientCLI {
		u, err := url.Parse(l.ReturnTo)
		if err != nil {
			s.ssoPage(w, r, http.StatusBadRequest, msg)
			return
		}
		q := url.Values{}
		if ticket != "" {
			q.Set("ticket", ticket)
		} else {
			q.Set("error", code)
			q.Set("error_description", msg)
		}
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
		return
	}
	if ticket != "" {
		s.ssoToDashboard(w, r, l.ReturnTo, "sso", ticket)
		return
	}
	s.ssoToDashboard(w, r, l.ReturnTo, "sso_error", msg)
}

// ssoToDashboard redirects to a same-origin dashboard path with one value in the fragment,
// which the browser never sends to a server, so the ticket stays out of every access log.
func (s *Server) ssoToDashboard(w http.ResponseWriter, r *http.Request, path, key, value string) {
	if key == "sso_error" {
		http.SetCookie(w, s.sso.cookie("", -1))
	}
	frag := url.Values{key: {value}}
	http.Redirect(w, r, safeNext(path)+"#"+frag.Encode(), http.StatusSeeOther)
}

func (s *Server) ssoServerError(w http.ResponseWriter, r *http.Request, l db.SSOLogin, err error) {
	id := requestID(r)
	s.logger.Error("sso sign-in failed", "request_id", id, "provider", l.Provider, "error", err)
	s.ssoReturn(w, r, l, "", "internal", "Signing in failed on the server (request "+id+").")
}

type ssoRedeemBody struct {
	Ticket       string `json:"ticket"`
	CodeVerifier string `json:"code_verifier"`
}

// ssoRedeem exchanges a ticket for a token.
func (s *Server) ssoRedeem(w http.ResponseWriter, r *http.Request) {
	client := clientKey(r, s.behindProxy)
	allowed, retryAfter := s.limiter.allow(client)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		s.ok(w, r, http.StatusTooManyRequests, ErrorBody{Error: "too many failed authentication attempts", Code: "rate_limited"})
		return
	}
	// A JSON body, like local sign-in: a cross-site form cannot send one without a CORS
	// preflight this server never answers.
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		s.ok(w, r, http.StatusUnsupportedMediaType, ErrorBody{Error: "send a JSON body", Code: "invalid_argument"})
		return
	}
	var body ssoRedeemBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	challenge := ""
	if sso.ValidPKCE(body.CodeVerifier) {
		challenge = sso.Challenge(body.CodeVerifier)
	}
	var login db.SSOLogin
	found := false
	if body.Ticket != "" {
		var err error
		if login, found, err = s.store.RedeemSSOTicket(r.Context(), ssoHash(body.Ticket), s.sso.binderHash(r), challenge); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if !found {
		s.limiter.fail(client)
		s.ok(w, r, http.StatusUnauthorized, ErrorBody{Code: "invalid_ticket",
			Error: "this sign-in ticket is unknown, expired, already used, or was not issued to this client; sign in again"})
		return
	}
	s.limiter.succeed(client)
	if login.Client == ssoClientDashboard {
		http.SetCookie(w, s.sso.cookie("", -1))
	}
	principal, err := s.store.GetPrincipal(r.Context(), login.PrincipalID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if login.LinkPrincipal != "" {
		s.ok(w, r, http.StatusOK, map[string]any{"linked": true, "provider": login.Provider, "handle": principal.Handle,
			"principal_id": principal.ID})
		return
	}
	ttl := s.sso.tokenTTL()
	pol, err := s.policyFor(r.Context(), principal.OrganizationID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if t := pol.SSOSessionTTL.Std(); t > 0 {
		ttl = t
	}
	ttl = min(ttl, pol.HumanTokenCap())
	token, err := s.store.CreateToken(r.Context(), principal.ID, db.SSOTokenPrefix+login.Provider, ttl)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), principal.OrganizationID, "", principal.ID, "sso.login", "principal", principal.ID,
		map[string]any{"provider": login.Provider, "kind": login.Client})
	s.ok(w, r, http.StatusCreated, map[string]any{
		"token": token, "handle": principal.Handle, "principal_id": principal.ID,
		"provider": login.Provider, "expires_at": time.Now().Add(ttl).UTC(),
	})
}

// ssoRefusal is a sign-in the policy refuses, with a message for the person signing in.
type ssoRefusal struct{ code, msg string }

func (e *ssoRefusal) Error() string { return e.msg }

func refusal(code, format string, args ...any) error {
	return &ssoRefusal{code: code, msg: fmt.Sprintf(format, args...)}
}

// ssoResolve maps a verified identity to the principal it signs in as, linking or
// provisioning where the rules allow.
func (s *Server) ssoResolve(r *http.Request, p sso.Provider, ident sso.Identity, linkPrincipal domain.ID) (domain.Principal, error) {
	ctx := r.Context()
	cfg := p.Config()
	acct := db.ExternalAccount{Provider: cfg.Name, Issuer: ident.Issuer, Subject: ident.Subject, Email: ident.Email}
	existing, found, err := s.store.IdentityByAccount(ctx, ident.Issuer, ident.Subject)
	if err != nil {
		return domain.Principal{}, err
	}

	var principalID domain.ID
	mode := ""
	switch {
	case found && linkPrincipal != "" && existing.PrincipalID != linkPrincipal:
		return domain.Principal{}, refusal("identity_taken",
			"This %s account is already linked to a different Conductor account.", cfg.Label)
	case found:
		principalID = existing.PrincipalID
		if err := s.store.TouchIdentity(ctx, ident.Issuer, ident.Subject, ident.Email); err != nil {
			return domain.Principal{}, err
		}
	case linkPrincipal != "":
		err := s.store.LinkIdentity(ctx, linkPrincipal, acct, false)
		switch {
		case errors.Is(err, db.ErrIssuerTaken):
			return domain.Principal{}, refusal("already_linked",
				"Your Conductor account already has a different %s account linked. Ask an administrator to unlink it first.", cfg.Label)
		case errors.Is(err, db.ErrIdentityTaken):
			return domain.Principal{}, refusal("identity_taken",
				"This %s account is already linked to a different Conductor account.", cfg.Label)
		case err != nil:
			return domain.Principal{}, err
		}
		principalID, mode = linkPrincipal, "link"
	default:
		principal, err := s.ssoFirstSignIn(r, cfg, ident, acct)
		if err != nil {
			return domain.Principal{}, err
		}
		principalID, mode = principal.ID, "first_sign_in"
	}

	principal, err := s.store.GetPrincipal(ctx, principalID)
	if err != nil {
		return domain.Principal{}, err
	}
	if mode != "" {
		s.store.Audit(ctx, principal.OrganizationID, "", principal.ID, "sso.linked", "principal", principal.ID,
			map[string]any{"provider": cfg.Name, "mode": mode, "principal": principal.Handle})
	}
	if deactivated, err := s.store.PrincipalDeactivated(ctx, principal.ID); err != nil {
		return domain.Principal{}, err
	} else if deactivated {
		return domain.Principal{}, refusal("deactivated",
			"Your Conductor account (%s) is deactivated. Ask an administrator of your organization.", principal.Handle)
	}
	// The organization's own admission rules, on top of the provider's (admin_sso.go).
	pol, err := s.policyFor(ctx, principal.OrganizationID)
	if err != nil {
		return domain.Principal{}, err
	}
	if err := ssoAdmits(pol, cfg, ident); err != nil {
		return domain.Principal{}, err
	}
	if linkPrincipal == "" {
		// Identity-provider groups decide project roles, within the policy's bounds, before
		// the membership check: a group can be how someone first gets access.
		if err := s.applyGroupMapping(r, pol, principal, cfg, ident.Groups); err != nil {
			return domain.Principal{}, err
		}
		// Removing someone from their last project revokes their tokens; a sign-in must not
		// hand them a fresh one.
		member, err := s.store.HasMembership(ctx, principal.ID)
		if err != nil {
			return domain.Principal{}, err
		}
		if !member {
			return domain.Principal{}, refusal("no_membership",
				"Your Conductor account (%s) is not a member of any project. Ask a project administrator to add you.", principal.Handle)
		}
	}
	return principal, nil
}

// ssoFirstSignIn handles an external account linked to no one: link it to the principal an
// administrator registered its address on, or provision one, or refuse.
func (s *Server) ssoFirstSignIn(r *http.Request, cfg sso.Config, ident sso.Identity, acct db.ExternalAccount) (domain.Principal, error) {
	ctx := r.Context()
	candidates, err := s.store.HumansByEmail(ctx, ident.Email)
	if err != nil {
		return domain.Principal{}, err
	}
	switch len(candidates) {
	case 0:
		// An organization whose policy provisions accounts for this identity comes first;
		// the server-wide --sso-auto-provision is the fallback.
		if p, ok, err := s.ssoProvisionByPolicy(r, cfg, ident, acct); err != nil || ok {
			return p, err
		}
		if s.sso.opts.AutoProvisionRole == "" {
			return domain.Principal{}, refusal("not_registered",
				"No Conductor account is registered for %s. Ask a project administrator to add you "+
					"(conductor member add <handle> --email %s), then sign in again.", ident.Email, ident.Email)
		}
		return s.ssoProvision(r, cfg, ident, acct)
	case 1:
		c := candidates[0]
		// An organization that does not admit this identity does not get it linked either.
		pol, err := s.policyFor(ctx, c.OrganizationID)
		if err != nil {
			return domain.Principal{}, err
		}
		if err := ssoAdmits(pol, cfg, ident); err != nil {
			return domain.Principal{}, err
		}
		err = s.store.LinkIdentity(ctx, c.ID, acct, true)
		switch {
		case errors.Is(err, db.ErrAlreadyLinked), errors.Is(err, db.ErrIssuerTaken):
			// The address matches, but the account already signs in some other way. Attaching
			// a second identity on the strength of an email address is how an account at one
			// provider takes over an account someone else holds at another.
			return domain.Principal{}, refusal("already_linked",
				"The Conductor account registered for %s already signs in with a different identity, so this %s account "+
					"cannot be attached to it. Sign in with that identity and run `conductor sso link %s`, "+
					"or ask an administrator to unlink the other one.", ident.Email, cfg.Label, cfg.Name)
		case errors.Is(err, db.ErrIdentityTaken):
			return domain.Principal{}, refusal("identity_taken",
				"This %s account was just linked to a different Conductor account.", cfg.Label)
		case err != nil:
			return domain.Principal{}, err
		}
		return c, nil
	default:
		return domain.Principal{}, refusal("ambiguous_email",
			"%s is registered on more than one Conductor account. Ask an administrator to keep it on one.", ident.Email)
	}
}

// ssoProvision creates an account for a first sign-in, when auto-provisioning is on.
func (s *Server) ssoProvision(r *http.Request, cfg sso.Config, ident sso.Identity, acct db.ExternalAccount) (domain.Principal, error) {
	role := s.sso.opts.AutoProvisionRole
	// Checked at startup too; a role that slipped past it still must not be granted.
	if !autoProvisionRoles[role] {
		return domain.Principal{}, refusal("not_registered", "No Conductor account is registered for %s.", ident.Email)
	}
	orgSlug, projectSlug, _ := strings.Cut(s.sso.opts.DefaultProject, "/")
	org, err := s.store.GetOrganizationBySlug(r.Context(), orgSlug)
	if err != nil {
		return domain.Principal{}, fmt.Errorf("sso default project %s: %w", s.sso.opts.DefaultProject, err)
	}
	project, err := s.store.GetProjectBySlug(r.Context(), org.ID, projectSlug)
	if err != nil {
		return domain.Principal{}, fmt.Errorf("sso default project %s: %w", s.sso.opts.DefaultProject, err)
	}
	base := ident.Username
	if cfg.Kind != sso.KindGitHub || base == "" {
		base, _, _ = strings.Cut(ident.Email, "@")
	}
	principal, err := s.store.ProvisionSSOPrincipal(r.Context(), project, db.SanitizeHandle(base), ident.Name, role, acct)
	if errors.Is(err, db.ErrIdentityTaken) {
		return domain.Principal{}, refusal("identity_taken", "This %s account was just linked to a different Conductor account.", cfg.Label)
	}
	if err != nil {
		return domain.Principal{}, err
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, principal.ID, "sso.provisioned", "principal", principal.ID,
		map[string]any{"provider": cfg.Name, "principal": principal.Handle, "role": string(role)})
	return principal, nil
}

// ssoStatus lists the providers and the caller's own linked identities.
func (s *Server) ssoStatus(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	ids, err := s.store.ListIdentities(r.Context(), p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{"providers": s.ssoProviderList(), "identities": ids, "token_ttl": s.sso.tokenTTL().String()}
	if role := s.sso.opts.AutoProvisionRole; role != "" && len(s.sso.order) > 0 {
		out["auto_provision"] = map[string]any{"role": role, "project": s.sso.opts.DefaultProject}
	}
	s.ok(w, r, http.StatusOK, out)
}

// administersEverywhere refuses unless the caller administers every project target belongs
// to, with a role that may manage target's role there. Setting the address a first sign-in
// links by, or cutting someone's sign-in, acts on the whole account, not on one membership:
// a project_admin of one project must not be able to point an org_admin's account elsewhere
// at a mailbox they control.
func (s *Server) administersEverywhere(r *http.Request, caller, target domain.Principal) error {
	projects, err := s.store.ListProjectsFor(r.Context(), target.ID)
	if err != nil {
		return err
	}
	for _, proj := range projects {
		targetRole, err := s.store.RoleIn(r.Context(), proj.ID, target.ID)
		if err != nil {
			return err
		}
		callerRole, err := s.store.RoleIn(r.Context(), proj.ID, caller.ID)
		if errors.Is(err, domain.ErrNotFound) || (err == nil && (!callerRole.Can(domain.RoleProjectAdmin) || !grantable(callerRole, targetRole))) {
			return fmt.Errorf("%w: %s belongs to a project you do not administer above their role; "+
				"only someone who administers all of their projects can change how they sign in", domain.ErrNotPermitted, target.Handle)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// memberForSignInAdmin resolves the member an SSO administration route acts on.
func (s *Server) memberForSignInAdmin(r *http.Request, p domain.Principal) (domain.Project, domain.Principal, error) {
	if tokenProjectScope(r) != "" {
		return domain.Project{}, domain.Principal{}, fmt.Errorf(
			"%w: a project-scoped token cannot change how a member signs in", domain.ErrNotPermitted)
	}
	project, caller, err := s.project(r, p, domain.RoleProjectAdmin)
	if err != nil {
		return domain.Project{}, domain.Principal{}, err
	}
	target, err := s.store.GetPrincipalByHandle(r.Context(), project.OrganizationID, r.PathValue("handle"))
	if err != nil {
		return domain.Project{}, domain.Principal{}, err
	}
	if _, err := s.store.RoleIn(r.Context(), project.ID, target.ID); err != nil {
		return domain.Project{}, domain.Principal{}, err
	}
	if err := s.administersEverywhere(r, caller.Principal, target); err != nil {
		return domain.Project{}, domain.Principal{}, err
	}
	return project, target, nil
}

// setMemberEmail registers the address a member's first single sign-on links by.
func (s *Server) setMemberEmail(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body struct {
		Email string `json:"email"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	email := strings.TrimSpace(body.Email)
	if email != "" && !plausibleEmail(email) {
		s.fail(w, r, fmt.Errorf("%w: %q is not an email address", domain.ErrInvalidArgument, email))
		return
	}
	project, target, err := s.memberForSignInAdmin(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if target.Kind != domain.PrincipalHuman {
		s.fail(w, r, fmt.Errorf("%w: single sign-on is for people; %s is a %s", domain.ErrInvalidArgument, target.Handle, target.Kind))
		return
	}
	if err := s.store.SetPrincipalEmail(r.Context(), target.ID, email); err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID, "member.email_set", "principal", target.ID,
		map[string]any{"principal": target.Handle})
	s.ok(w, r, http.StatusOK, map[string]any{"handle": target.Handle, "email": email})
}

// unlinkMemberIdentity removes a member's identity at one provider and ends the sessions it
// opened.
func (s *Server) unlinkMemberIdentity(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, target, err := s.memberForSignInAdmin(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	provider := r.PathValue("provider")
	revoked, err := s.store.UnlinkIdentity(r.Context(), target.ID, provider)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID, "sso.unlinked", "principal", target.ID,
		map[string]any{"principal": target.Handle, "provider": provider, "count": revoked})
	s.ok(w, r, http.StatusOK, map[string]any{"handle": target.Handle, "provider": provider, "revoked_tokens": revoked})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// safeNext admits only a same-origin path, so the sign-in cannot be turned into a redirect
// to anywhere else: "/tasks/T-1" yes; "//evil.example", "https://evil.example",
// "/\evil.example" (which some browsers read as "//") and anything unparseable become "/".
func safeNext(next string) string {
	if next == "" || len(next) > 512 || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	for _, c := range next {
		if c == '\\' || c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "/"
	}
	out := u.EscapedPath()
	if !strings.HasPrefix(out, "/") || strings.HasPrefix(out, "//") {
		return "/"
	}
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// loopbackRedirect admits only a plain-http loopback address with an explicit port: the one
// place a CLI on the user's own machine listens. Anything else would make the callback an
// open redirect for tickets.
func loopbackRedirect(raw string) (string, bool) {
	if raw == "" || len(raw) > 256 {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", false
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", false
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), true
}

func plausibleEmail(s string) bool {
	local, domain, ok := strings.Cut(s, "@")
	return ok && local != "" && domain != "" && !strings.Contains(domain, "@") && len(s) <= 254 &&
		!strings.ContainsAny(s, " \t\r\n<>,;\"") && strings.Contains(domain, ".")
}

// printable keeps a provider-supplied string short and inert before it is echoed back.
func printable(s string, max int) string {
	var b strings.Builder
	for _, c := range s {
		if b.Len() >= max {
			break
		}
		if c >= 0x20 && c < 0x7f {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// ssoMessage is what the person signing in is told about a provider failure.
func ssoMessage(err error) string {
	var e *sso.Error
	if errors.As(err, &e) {
		return e.Message
	}
	if errors.Is(err, domain.ErrCapacity) {
		return err.Error()
	}
	return "Signing in failed on the server."
}

// ssoFail answers a JSON request that failed at the provider.
func (s *Server) ssoFail(w http.ResponseWriter, r *http.Request, err error) {
	var e *sso.Error
	if errors.As(err, &e) {
		s.logger.Warn("sso provider failure", "request_id", requestID(r), "provider", r.PathValue("provider"), "error", err)
		s.ok(w, r, http.StatusBadGateway, ErrorBody{Error: e.Message, Code: "sso_" + e.Code})
		return
	}
	s.fail(w, r, err)
}

// ssoPage answers a browser with a short plain-text page.
func (s *Server) ssoPage(w http.ResponseWriter, _ *http.Request, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, msg)
}
