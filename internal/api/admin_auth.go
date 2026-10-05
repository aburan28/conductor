package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/adamburan/conductor/internal/admin"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// Organization policy where it touches authentication: require-SSO, token lifetimes, and
// the lineage of tokens minted from a single sign-on (DESIGN.md §25.8).

// AdminOptions configures the admin area.
type AdminOptions struct {
	// Locks are the organization settings the server's config file decides.
	Locks admin.Locks
	// Config is the server's effective configuration, secrets redacted, for
	// GET /v1/admin/config.
	Config []ConfigEntry
	// ConfigFile is the path of the config file in force, if any.
	ConfigFile string
}

// ConfigEntry is one setting of the effective server configuration.
type ConfigEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Source is where the value came from: flag, env, file or default.
	Source string `json:"source"`
	Secret bool   `json:"secret,omitempty"`
}

type adminState struct {
	opts AdminOptions

	mu    sync.Mutex
	cache map[domain.ID]cachedPolicy
}

type cachedPolicy struct {
	policy admin.Policy
	at     time.Time
}

// policyCacheTTL bounds how stale a replica's view of an organization's policy may be. A
// change made through this replica is visible at once; one made through another replica
// within this long. Authentication consults the policy on every request, so it is cached.
const policyCacheTTL = 5 * time.Second

func newAdminState(opts AdminOptions) *adminState {
	return &adminState{opts: opts, cache: map[domain.ID]cachedPolicy{}}
}

// policyFor returns an organization's effective policy: what is stored, with the config
// file's locked settings applied over it.
func (s *Server) policyFor(ctx context.Context, orgID domain.ID) (admin.Policy, error) {
	st := s.admin
	st.mu.Lock()
	if c, ok := st.cache[orgID]; ok && time.Since(c.at) < policyCacheTTL {
		st.mu.Unlock()
		return c.policy, nil
	}
	st.mu.Unlock()
	stored, err := s.store.GetOrgPolicy(ctx, orgID)
	if err != nil {
		return admin.Policy{}, err
	}
	p := st.opts.Locks.Effective(stored.Policy)
	st.mu.Lock()
	st.cache[orgID] = cachedPolicy{policy: p, at: time.Now()}
	st.mu.Unlock()
	return p, nil
}

func (s *Server) forgetPolicy(orgID domain.ID) {
	s.admin.mu.Lock()
	delete(s.admin.cache, orgID)
	s.admin.mu.Unlock()
}

type adminCtxKey int

// tokenExpiresKey carries the expiry of the token that authenticated the request.
const tokenExpiresKey adminCtxKey = 0

func tokenExpires(r *http.Request) *time.Time {
	t, _ := r.Context().Value(tokenExpiresKey).(*time.Time)
	return t
}

// isHuman reports whether a principal is a person, to whom human token rules apply.
func isHuman(p domain.Principal) bool { return p.Kind == domain.PrincipalHuman || p.Kind == "" }

// ssoTokenProvider returns the provider a token named sso:<provider> or
// sso:<provider>/<name> came from, or "".
func ssoTokenProvider(name string) string {
	rest, ok := strings.CutPrefix(name, db.SSOTokenPrefix)
	if !ok {
		return ""
	}
	provider, _, _ := strings.Cut(rest, "/")
	return provider
}

// errSSORequired is how a refused non-SSO token is reported.
const ssoRequiredMessage = "this organization requires single sign-on: sign in with `conductor login --sso` or the dashboard's sign-in button"

// enforceOrgPolicy applies the organization's policy to an authenticated request. It
// answers the request itself (and returns false) when the token may not be used.
//
// Require-SSO refuses, rather than revokes, a person's tokens that single sign-on did not
// mint: the rule then holds for every token whatever minted it — an invite, a bootstrap,
// one from before the policy was turned on — on every replica, from the moment the policy
// is read, and turning the policy off (say, when the identity provider is down) restores
// them without a round of re-issuing. Tokens from local sign-in follow the security mode
// instead, and service principals (runners, bots) are not people and keep their tokens.
func (s *Server) enforceOrgPolicy(w http.ResponseWriter, r *http.Request, p domain.Principal, name string) bool {
	if !isHuman(p) || ssoTokenProvider(name) != "" || strings.HasPrefix(name, db.LocalTokenPrefix) {
		return true
	}
	pol, err := s.policyFor(r.Context(), p.OrganizationID)
	if err != nil {
		s.fail(w, r, err)
		return false
	}
	if pol.RequireSSO {
		s.ok(w, r, http.StatusUnauthorized, ErrorBody{Code: "sso_required", Error: ssoRequiredMessage})
		return false
	}
	return true
}

// mintPolicy decides the name and lifetime of a token a principal mints for itself
// (POST /v1/tokens, /v1/tokens/reset). A token minted with a single sign-on token belongs
// to that sign-in: it is named sso:<provider>/<name>, so require-SSO accepts it and
// unlinking the identity revokes it, and it expires no later than its parent, so it cannot
// outlive the sign-in it came from. The name prefix sso: is reserved for that lineage. The
// organization's token lifetime cap applies to every token.
func (s *Server) mintPolicy(r *http.Request, p domain.Principal, name string, ttl time.Duration) (string, time.Duration, error) {
	if strings.HasPrefix(name, db.SSOTokenPrefix) {
		return "", 0, fmt.Errorf("%w: token names starting with %q are reserved for single sign-on", domain.ErrInvalidArgument, db.SSOTokenPrefix)
	}
	if provider := ssoTokenProvider(tokenName(r)); provider != "" {
		name = db.SSOTokenPrefix + provider + "/" + name
		if exp := tokenExpires(r); exp != nil {
			remaining := time.Until(*exp)
			if remaining <= 0 {
				return "", 0, domain.ErrUnauthenticated
			}
			if ttl == 0 || ttl > remaining {
				ttl = remaining
			}
		}
	}
	pol, err := s.policyFor(r.Context(), p.OrganizationID)
	if err != nil {
		return "", 0, err
	}
	return name, pol.CapTokenTTL(p.Kind, ttl), nil
}

// capInviteTTL applies the organization's token cap to a token an invite mints.
func (s *Server) capInviteTTL(r *http.Request, orgID domain.ID, kind domain.PrincipalKind, ttl time.Duration) (time.Duration, error) {
	pol, err := s.policyFor(r.Context(), orgID)
	if err != nil {
		return 0, err
	}
	return pol.CapTokenTTL(kind, ttl), nil
}

// requiresSSO reports whether an organization requires single sign-on for people.
func (s *Server) requiresSSO(r *http.Request, orgID domain.ID) (bool, error) {
	pol, err := s.policyFor(r.Context(), orgID)
	return pol.RequireSSO, err
}
