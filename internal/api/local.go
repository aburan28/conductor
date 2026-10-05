package api

import (
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// Local sign-in: open the dashboard on your own laptop and you are in.
//
// The rest of the API is bearer-token only, and stays that way. This adds exactly one
// unauthenticated endpoint, POST /v1/local/session, which mints an ordinary token for the
// machine's owner (the person who bootstrapped it) when — and only when — the request
// demonstrably comes from this machine and not from a web page somewhere else:
//
//   - the TCP peer is loopback, and the daemon is not behind a proxy (behind a proxy every
//     peer is loopback, so the signal means nothing);
//   - the Host header names loopback, which defeats DNS rebinding: a hostile page that
//     rebinds evil.example to 127.0.0.1 still sends Host: evil.example;
//   - the body is JSON, which a cross-site form cannot send without a CORS preflight this
//     server never answers;
//   - a browser's Origin, when present, is this same loopback origin, and Sec-Fetch-Site,
//     when present, is same-origin or none.
//
// What this does not defend against is another OS user on the same machine, who can reach
// loopback too. That is what enhanced security mode is for: it turns this endpoint off and
// revokes every token it handed out. A loopback-only daemon defaults to local mode; one
// reachable from the network defaults to enhanced.

// LocalLoginOptions configures local sign-in.
type LocalLoginOptions struct {
	// DefaultMode applies when no mode has been chosen and saved. Empty means enhanced, so a
	// server constructed without thinking about it (tests, embedders) is the strict one.
	DefaultMode string
	// ForcedMode, when set (conductord --security-mode), wins over the saved choice and
	// cannot be changed through the API.
	ForcedMode string
	// TokenTTL is how long a locally minted token lives; default 30 days.
	TokenTTL time.Duration
}

const localTokenTTLDefault = 30 * 24 * time.Hour

func (s *Server) localRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("GET /v1/local/status", s.localStatus)
	m.HandleFunc("POST /v1/local/session", s.localSession)
	m.HandleFunc("GET /v1/security", auth(s.getSecurity))
	m.HandleFunc("POST /v1/security", auth(s.setSecurity))
}

// securityMode resolves the mode in force and where it came from.
func (s *Server) securityMode(r *http.Request) (mode, source string, settings db.ServerSettings, err error) {
	settings, err = s.store.GetServerSettings(r.Context())
	if err != nil {
		return "", "", settings, err
	}
	switch {
	case s.local.ForcedMode != "":
		return s.local.ForcedMode, "flag", settings, nil
	case settings.SecurityMode != "":
		return settings.SecurityMode, "setting", settings, nil
	case s.local.DefaultMode == db.SecurityLocal:
		return db.SecurityLocal, "default", settings, nil
	}
	return db.SecurityEnhanced, "default", settings, nil
}

// localRefusal explains why this request may not sign in locally, or returns "" when it
// may. It checks the request only; the mode and the owner are checked by the caller.
func (s *Server) localRefusal(r *http.Request) string {
	if s.behindProxy {
		return "local sign-in is unavailable behind a reverse proxy, where every connection looks local"
	}
	// A proxy nobody told us about — nginx, a tunnel, `tailscale serve` — in front of a
	// loopback-bound daemon makes a remote request arrive from loopback, sometimes with the
	// Host rewritten to loopback too. Proxies announce themselves: forwarding headers, or
	// (stock nginx) HTTP/1.0 to the upstream. A request carrying either is not local.
	for _, h := range proxyHeaders {
		if r.Header.Get(h) != "" {
			return "local sign-in is refused to requests that came through a proxy (" + h + ")"
		}
	}
	if r.ProtoMajor == 1 && r.ProtoMinor == 0 {
		return "local sign-in is refused to HTTP/1.0 requests, which is how a reverse proxy usually talks to its upstream"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "local sign-in only works from this machine"
	}
	if !loopbackHostname(hostOnly(r.Host)) {
		return "local sign-in needs the dashboard opened at localhost or 127.0.0.1"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !loopbackHostname(u.Hostname()) || !strings.EqualFold(u.Host, r.Host) {
			return "local sign-in is refused to pages from another origin"
		}
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return "local sign-in is refused to cross-site requests"
	}
	return ""
}

// proxyHeaders are the headers mainstream reverse proxies and tunnels add by default.
var proxyHeaders = []string{
	"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Via",
	"Cf-Connecting-Ip", "True-Client-Ip", "Tailscale-User-Login", "X-Original-Forwarded-For",
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func loopbackHostname(h string) bool {
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// localStatus is public and says only what a sign-in screen needs: which mode is in force
// and whether this particular request could sign in locally. It names no one.
func (s *Server) localStatus(w http.ResponseWriter, r *http.Request) {
	mode, source, settings, err := s.securityMode(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	available, reason := false, ""
	switch {
	case mode != db.SecurityLocal:
		reason = "this server is in enhanced security mode; sign in with a token"
	case settings.LocalOwnerID == "":
		reason = "no machine owner is set; run `conductord bootstrap` on this machine"
	default:
		reason = s.localRefusal(r)
		available = reason == ""
	}
	s.ok(w, r, http.StatusOK, map[string]any{
		"security_mode": mode, "mode_source": source,
		"local_login_available": available, "reason": reason,
	})
}

var localClients = map[string]bool{"dashboard": true, "cli": true, "mac-app": true}

// localSession mints a token for the machine's owner.
func (s *Server) localSession(w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, code, msg string) {
		s.ok(w, r, status, ErrorBody{Error: msg, Code: code})
	}
	mode, _, settings, err := s.securityMode(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if mode != db.SecurityLocal {
		refuse(http.StatusForbidden, "local_login_disabled",
			"this server is in enhanced security mode: sign in with a token (`conductor dashboard` prints a sign-in link)")
		return
	}
	if why := s.localRefusal(r); why != "" {
		refuse(http.StatusForbidden, "not_local", why)
		return
	}
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		refuse(http.StatusUnsupportedMediaType, "invalid_argument", "send a JSON body")
		return
	}
	if settings.LocalOwnerID == "" {
		refuse(http.StatusConflict, "no_owner", "no machine owner is set; run `conductord bootstrap` on this machine")
		return
	}
	var body struct {
		Client string `json:"client"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	// A fixed set of client kinds, one live token each: signing in again replaces the last
	// token of that kind, so a local caller cannot accumulate owner credentials by inventing
	// names.
	client := strings.ToLower(strings.TrimSpace(body.Client))
	if !localClients[client] {
		client = "other"
	}
	owner, err := s.store.GetPrincipal(r.Context(), settings.LocalOwnerID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ttl := s.local.TokenTTL
	if ttl <= 0 {
		ttl = localTokenTTLDefault
	}
	// One live token per client kind: signing in again from the dashboard replaces the last
	// dashboard token instead of piling up credentials nobody will ever revoke by hand.
	name := db.LocalTokenPrefix + client
	if err := s.store.RevokeToken(r.Context(), owner.ID, name); err != nil && !errors.Is(err, domain.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	token, err := s.store.CreateToken(r.Context(), owner.ID, name, ttl)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), owner.OrganizationID, "", owner.ID, "local.signin", "principal", owner.ID,
		map[string]any{"kind": client})
	s.ok(w, r, http.StatusCreated, map[string]any{
		"token": token, "handle": owner.Handle, "principal_id": owner.ID,
		"expires_at": time.Now().Add(ttl).UTC(),
	})
}

// isAdminSomewhere reports whether a principal administers at least one project on this
// control plane.
func (s *Server) isAdminSomewhere(r *http.Request, p domain.Principal) (bool, error) {
	projects, err := s.store.ListProjectsFor(r.Context(), p.ID)
	if err != nil {
		return false, err
	}
	for _, proj := range projects {
		role, err := s.store.RoleIn(r.Context(), proj.ID, p.ID)
		if err != nil {
			continue
		}
		if role == domain.RoleOrgAdmin || role == domain.RoleProjectAdmin {
			return true, nil
		}
	}
	return false, nil
}

// machineOwner returns the machine's owner, ok=false when none is recorded.
func (s *Server) machineOwner(r *http.Request) (domain.Principal, bool, error) {
	settings, err := s.store.GetServerSettings(r.Context())
	if err != nil || settings.LocalOwnerID == "" {
		return domain.Principal{}, false, err
	}
	owner, err := s.store.GetPrincipal(r.Context(), settings.LocalOwnerID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Principal{}, false, nil
	}
	return owner, err == nil, err
}

// inOwnersOrg reports whether p belongs to the same organization as the machine's owner. A
// control plane can host several organizations; machine-level settings and the machine's
// GitHub App are the owner's organization's business, not every tenant's.
func (s *Server) inOwnersOrg(r *http.Request, p domain.Principal) (bool, error) {
	owner, ok, err := s.machineOwner(r)
	if err != nil || !ok {
		return false, err
	}
	return owner.OrganizationID == p.OrganizationID, nil
}

func (s *Server) getSecurity(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	mode, source, settings, err := s.securityMode(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{"security_mode": mode, "mode_source": source, "behind_proxy": s.behindProxy}
	if owner, ok, err := s.machineOwner(r); err == nil && ok {
		out["you_are_owner"] = settings.LocalOwnerID == p.ID
		if owner.OrganizationID == p.OrganizationID {
			out["owner"] = owner.Handle
		}
	}
	s.ok(w, r, http.StatusOK, out)
}

// setSecurity changes the mode. Tightening (to enhanced) is open to the owner and to project
// administrators in the owner's organization — tightening revokes the owner's local sessions,
// so another tenant on a shared daemon must not be able to lock the owner out. Loosening
// (back to local) is the owner's call alone — it is their machine that becomes reachable
// without a token. Neither is possible when conductord was started with --security-mode.
func (s *Server) setSecurity(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body struct {
		Mode string `json:"security_mode"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Mode != db.SecurityLocal && body.Mode != db.SecurityEnhanced {
		s.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("security_mode must be local or enhanced")))
		return
	}
	if s.local.ForcedMode != "" {
		s.ok(w, r, http.StatusConflict, ErrorBody{Code: "locked",
			Error: "conductord was started with --security-mode " + s.local.ForcedMode + "; restart it without the flag to change modes here"})
		return
	}
	_, _, settings, err := s.securityMode(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	isOwner := settings.LocalOwnerID != "" && settings.LocalOwnerID == p.ID
	switch body.Mode {
	case db.SecurityLocal:
		if !isOwner {
			s.fail(w, r, errors.Join(domain.ErrNotPermitted, errors.New("only this machine's owner can turn local sign-in back on")))
			return
		}
	case db.SecurityEnhanced:
		if !isOwner {
			admin, err := s.isAdminSomewhere(r, p)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			sameOrg := true
			if settings.LocalOwnerID != "" {
				if sameOrg, err = s.inOwnersOrg(r, p); err != nil {
					s.fail(w, r, err)
					return
				}
			}
			if !admin || !sameOrg {
				s.fail(w, r, errors.Join(domain.ErrNotPermitted, errors.New("only the machine's owner or a project administrator in their organization can change the security mode")))
				return
			}
		}
	}
	if err := s.store.SetSecurityMode(r.Context(), body.Mode); err != nil {
		s.fail(w, r, err)
		return
	}
	var revoked int64
	if body.Mode == db.SecurityEnhanced {
		if revoked, err = s.store.RevokeLocalTokens(r.Context()); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.store.Audit(r.Context(), p.OrganizationID, "", p.ID, "security.mode", "server", "", map[string]any{
		"to": body.Mode, "count": revoked,
	})
	s.ok(w, r, http.StatusOK, map[string]any{"security_mode": body.Mode, "revoked_local_tokens": revoked})
}
