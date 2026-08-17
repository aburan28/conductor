package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// Member and token administration.
//
// Before this existed, the only way to give a coworker access was to SSH to the database host
// and run `conductord bootstrap`, and the only way to take it away was hand-written SQL. That
// is not an access-control system, it is a a shared password with extra steps.

type inviteMemberBody struct {
	Handle      string               `json:"handle"`
	DisplayName string               `json:"display_name"`
	Email       string               `json:"email"`
	Role        domain.Role          `json:"role"`
	Kind        domain.PrincipalKind `json:"kind"`
	// TokenTTL bounds the credential's life. Zero means no expiry, which is appropriate for
	// a runner's service identity and a poor choice for a human.
	TokenTTL domain.Duration `json:"token_ttl"`
	// NoToken adds the membership without minting a credential, for a principal that
	// already has one from another project.
	NoToken bool `json:"no_token"`
}

type inviteMemberResult struct {
	Handle string      `json:"handle"`
	Role   domain.Role `json:"role"`
	// Token is returned exactly once, here. It is stored only as a SHA-256 hash and cannot
	// be recovered afterwards.
	Token     string `json:"token,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Created   bool   `json:"created_principal"`
}

// inviteMember adds a principal to a project and mints a token for them.
func (s *Server) inviteMember(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleProjectAdmin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body inviteMemberBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Handle == "" {
		s.fail(w, r, fmt.Errorf("%w: handle is required", domain.ErrInvalidArgument))
		return
	}
	if body.Role == "" {
		body.Role = domain.RoleContributor
	}
	if err := domain.Validate(body.Role, []domain.Role{
		domain.RoleOrgAdmin, domain.RoleProjectAdmin, domain.RoleMaintainer,
		domain.RoleContributor, domain.RoleReviewer, domain.RoleObserver, domain.RoleRunner,
	}, "role"); err != nil {
		s.fail(w, r, err)
		return
	}

	// Privilege cannot be granted upward. Without this a project_admin could mint an
	// org_admin and escalate through a principal they control.
	if body.Role == domain.RoleOrgAdmin && caller.Role != domain.RoleOrgAdmin {
		s.fail(w, r, fmt.Errorf("%w: only an org_admin may grant org_admin", domain.ErrNotPermitted))
		return
	}

	kind := body.Kind
	if kind == "" {
		kind = domain.PrincipalHuman
	}

	principal, err := s.store.GetPrincipalByHandle(r.Context(), project.OrganizationID, body.Handle)
	created := false
	if errors.Is(err, domain.ErrNotFound) {
		principal, err = s.store.CreatePrincipal(r.Context(), project.OrganizationID,
			kind, body.Handle, body.DisplayName, body.Email)
		created = true
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	if err := s.store.AddMember(r.Context(), project.ID, principal.ID, body.Role); err != nil {
		s.fail(w, r, err)
		return
	}

	result := inviteMemberResult{Handle: principal.Handle, Role: body.Role, Created: created}
	if !body.NoToken {
		// A human's credential expires by default; a service identity's does not, because
		// a runner that silently stops authenticating at 3am is worse than a long-lived
		// token on a machine you control.
		ttl := body.TokenTTL.Std()
		if ttl == 0 && kind == domain.PrincipalHuman {
			ttl = tokenTTLDefault
		}
		token, err := s.store.CreateToken(r.Context(), principal.ID,
			"invite:"+caller.Principal.Handle, ttl)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		result.Token = token
		if ttl > 0 {
			result.ExpiresAt = time.Now().Add(ttl).UTC().Format(time.RFC3339)
		}
	}

	s.store.Audit(r.Context(), project.OrganizationID, project.ID, caller.Principal.ID,
		"member.invited", "principal", principal.ID,
		map[string]any{"principal": principal.Handle, "kind": string(body.Role)})

	s.ok(w, r, http.StatusCreated, result)
}

// removeMember drops a membership and, when it was the principal's last one, revokes their
// tokens.
func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleProjectAdmin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	handle := r.PathValue("handle")
	target, err := s.store.GetPrincipalByHandle(r.Context(), project.OrganizationID, handle)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// Removing the last administrator would leave the project with nobody able to manage
	// membership, recoverable only from the database host.
	targetRole, err := s.store.RoleIn(r.Context(), project.ID, target.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if targetRole.Can(domain.RoleProjectAdmin) {
		admins, err := s.store.CountProjectAdmins(r.Context(), project.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if admins <= 1 {
			s.fail(w, r, fmt.Errorf(
				"%w: %s is the only administrator; promote someone else first",
				domain.ErrNotPermitted, handle))
			return
		}
	}

	if err := s.store.RemoveMember(r.Context(), project.ID, target.ID); err != nil {
		s.fail(w, r, err)
		return
	}

	s.store.Audit(r.Context(), project.OrganizationID, project.ID, caller.Principal.ID,
		"member.removed", "principal", target.ID,
		map[string]any{"principal": target.Handle})

	s.ok(w, r, http.StatusNoContent, nil)
}

type createTokenBody struct {
	Name string          `json:"name"`
	TTL  domain.Duration `json:"ttl"`
}

// createToken mints an additional token for the caller, for rotation or for a second machine.
//
// A principal can always mint for themselves: they already hold a valid credential, so this
// grants no privilege they did not have. It exists so rotating a leaked token does not
// require an administrator.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body createTokenBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Name == "" {
		body.Name = "cli"
	}
	token, err := s.store.CreateToken(r.Context(), p.ID, body.Name, body.TTL.Std())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusCreated, map[string]any{"name": body.Name, "token": token})
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	tokens, err := s.store.ListTokens(r.Context(), p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if tokens == nil {
		tokens = []db.TokenInfo{}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"tokens": tokens})
}

// revokeToken revokes one of the caller's own tokens by name.
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	name := r.PathValue("name")
	if err := s.store.RevokeToken(r.Context(), p.ID, name); err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), p.OrganizationID, "", p.ID,
		"token.revoked", "token", name, map[string]any{})
	s.ok(w, r, http.StatusNoContent, nil)
}

// revokeAllTokens is the panic button: cut every credential this principal holds.
func (s *Server) revokeAllTokens(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	n, err := s.store.RevokeAllTokens(r.Context(), p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), p.OrganizationID, "", p.ID,
		"token.revoked_all", "principal", p.ID, map[string]any{"count": n})
	// The caller's own credential is now dead too; say so, because the next request failing
	// with 401 should not be a surprise.
	s.ok(w, r, http.StatusOK, map[string]any{
		"revoked": n,
		"note":    "every token for this principal is revoked, including the one used for this request",
	})
}

// tokenTTLDefault bounds a human's credential when none is specified by the caller.
const tokenTTLDefault = 90 * 24 * time.Hour
