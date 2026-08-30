package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// Username/password sign-in for the dashboard.
//
// Tokens are the credential agents use; passwords are the credential a human can remember.
// A successful login mints a regular bearer token and hands back the same payload as
// /v1/whoami, so the dashboard treats "signed in with a password" and "opened an invite
// link" identically from that point on. Passwords never authenticate the agent-facing
// routes directly — they mint a token, which is auditable, revocable, and expires.

type loginBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Org disambiguates a handle that exists in more than one organization. Optional:
	// most deployments have one org, and the single-match case resolves without it.
	Org string `json:"org"`
}

// loginTokenTTL bounds a credential minted by dashboard sign-in. Shorter than the invite
// default because it is the credential a browser holds, and a browser is the thing most
// likely to be left unlocked.
const loginTokenTTL = 30 * 24 * time.Hour

// projectRef is the whoami project projection. Both /v1/whoami and /v1/login serialize
// projects this way, because the connect screen consumes them interchangeably.
type projectRef struct {
	ID   domain.ID   `json:"id"`
	Slug string      `json:"slug"`
	Role domain.Role `json:"role"`
}

type loginResult struct {
	Token     string           `json:"token"`
	ExpiresAt string           `json:"expires_at,omitempty"`
	Principal domain.Principal `json:"principal"`
	Projects  []projectRef     `json:"projects"`
}

// login exchanges a handle and password for a bearer token.
//
// Every failure path — unknown user, unknown org, no password set, wrong password — returns
// the same 401 with the same body, so a probe cannot tell which part was wrong. Failures
// feed the same per-client throttle the token path uses.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body loginBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Username == "" || body.Password == "" {
		s.fail(w, r, fmt.Errorf("%w: username and password are required", domain.ErrInvalidArgument))
		return
	}

	client := clientKey(r, s.behindProxy)
	allowed, retryAfter := s.limiter.allow(client)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		s.ok(w, r, http.StatusTooManyRequests, ErrorBody{
			Error: "too many failed authentication attempts", Code: "rate_limited",
		})
		return
	}

	principal, err := s.resolveForLogin(r, body)
	if err != nil {
		s.limiter.fail(client)
		// The same 401 regardless of cause: no user or org enumeration.
		s.ok(w, r, http.StatusUnauthorized, ErrorBody{
			Error: "invalid credentials", Code: "unauthenticated",
		})
		return
	}

	token, err := s.store.CreateToken(r.Context(), principal.ID, "login", loginTokenTTL)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.limiter.succeed(client)

	projects, expiresAt, err := s.identityPayload(r, principal)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), principal.OrganizationID, "", principal.ID,
		"principal.login", "principal", principal.ID, map[string]any{"method": "password"})

	s.ok(w, r, http.StatusOK, loginResult{
		Token: token, ExpiresAt: expiresAt, Principal: principal, Projects: projects,
	})
}

// resolveForLogin finds the one principal a login attempt is about.
func (s *Server) resolveForLogin(r *http.Request, body loginBody) (domain.Principal, error) {
	if body.Org != "" {
		org, err := s.store.GetOrganizationBySlug(r.Context(), body.Org)
		if err != nil {
			return domain.Principal{}, domain.ErrUnauthenticated
		}
		return s.store.AuthenticatePassword(r.Context(), org.ID, body.Username, body.Password)
	}
	matches, err := s.store.PrincipalsByHandle(r.Context(), body.Username)
	if err != nil {
		return domain.Principal{}, err
	}
	switch len(matches) {
	case 1:
		return s.store.AuthenticatePassword(r.Context(), matches[0].OrganizationID, body.Username, body.Password)
	case 0:
		return domain.Principal{}, domain.ErrUnauthenticated
	default:
		return domain.Principal{}, fmt.Errorf(
			"%w: handle %q exists in more than one organization; pass org",
			domain.ErrInvalidArgument, body.Username)
	}
}

// identityPayload assembles the whoami projection: the caller's projects with their role in
// each. It returns the projects and the just-minted login token's expiry, formatted for the
// wire. Shared by /v1/whoami and /v1/login so the two cannot drift.
func (s *Server) identityPayload(r *http.Request, p domain.Principal) ([]projectRef, string, error) {
	projects, err := s.store.ListProjectsFor(r.Context(), p.ID)
	if err != nil {
		return nil, "", err
	}
	refs := make([]projectRef, 0, len(projects))
	for _, pr := range projects {
		role, _ := s.store.RoleIn(r.Context(), pr.ID, p.ID)
		refs = append(refs, projectRef{ID: pr.ID, Slug: pr.Slug, Role: role})
	}
	return refs, loginExpiry(), nil
}

// loginExpiry renders when a just-minted login token stops working.
func loginExpiry() string {
	return time.Now().Add(loginTokenTTL).UTC().Format(time.RFC3339)
}

type setPasswordBody struct {
	Password string `json:"password"`
}

// setPassword sets or replaces the caller's own password. A principal can always do this:
// they already hold a valid credential, so it grants nothing they did not have. It exists so
// a person who arrived by token — an invite link, a bootstrap printout — can give themselves
// a memorable password without touching the database host.
func (s *Server) setPassword(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body setPasswordBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.SetPassword(r.Context(), p.ID, body.Password); err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), p.OrganizationID, "", p.ID,
		"password.set", "principal", p.ID, map[string]any{})
	s.ok(w, r, http.StatusOK, map[string]any{"set": true})
}
