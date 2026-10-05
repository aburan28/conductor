package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	// TokenTTL bounds the credential's life. Zero means the default: 90 days for a human, no
	// expiry for a service identity (a runner that silently stops authenticating at 3am is
	// worse than a long-lived token on a machine you control).
	TokenTTL domain.Duration `json:"token_ttl"`
	// NoToken adds the membership without minting a credential.
	NoToken bool `json:"no_token"`
}

type inviteMemberResult struct {
	Handle string      `json:"handle"`
	Role   domain.Role `json:"role"`
	// Token is returned exactly once, here, and only for an account this invite created. It
	// is stored only as a SHA-256 hash and cannot be recovered afterwards.
	Token     string `json:"token,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Created   bool   `json:"created_principal"`
	// ExistingPrincipal reports that the handle already named an account in this
	// organization. That account was added to the project, and no token was minted: its owner
	// keeps signing in with their own credentials, which now reach this project too.
	ExistingPrincipal bool `json:"existing_principal,omitempty"`
	// Note says, in a sentence, what the inviter should tell the invitee.
	Note string `json:"note,omitempty"`
}

// grantable reports whether a caller holding role `have` may confer, change, or remove role
// `target`. Privilege never flows upward: a project_admin can make another project_admin but
// not an org_admin, and cannot touch an org_admin's membership. The runner role sits outside
// the seniority ladder, so it is treated as below every administrator.
func grantable(have, target domain.Role) bool {
	if target == domain.RoleRunner {
		return have.Can(domain.RoleProjectAdmin)
	}
	return have.Can(target)
}

// inviteMember adds a principal to a project.
//
// A new handle creates the account and mints its first token, returned once to the inviter
// to pass on. An existing account in the organization is only added to the project: the
// inviter gets no token for it. Minting one here used to hand any project_admin a working
// credential for any account in the organization — an org_admin's, the bootstrap owner's —
// because a token works in every project its principal belongs to. And an existing member is
// never re-invited into a different role: that is a role change, and goes through
// setMemberRole, which enforces the rules a role change needs.
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
	if err := validateRole(body.Role); err != nil {
		s.fail(w, r, err)
		return
	}
	// Privilege cannot be granted upward. Without this a project_admin could mint an
	// org_admin and escalate through a principal they control.
	if !grantable(caller.Role, body.Role) {
		s.fail(w, r, fmt.Errorf("%w: a %s cannot grant %s", domain.ErrNotPermitted, caller.Role, body.Role))
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
		if errors.Is(err, domain.ErrDuplicate) {
			current, _ := s.store.RoleIn(r.Context(), project.ID, principal.ID)
			s.fail(w, r, fmt.Errorf(
				"%w: %s is already a member of this project as %s; change a role with "+
					"`conductor member role %s <role>` (PATCH /v1/projects/{project}/members/{handle})",
				domain.ErrDuplicate, principal.Handle, current, principal.Handle))
			return
		}
		s.fail(w, r, err)
		return
	}

	result := inviteMemberResult{Handle: principal.Handle, Role: body.Role, Created: created}
	switch {
	case !created:
		result.ExistingPrincipal = true
		result.Note = principal.Handle + " already has an account in this organization; they " +
			"use their own credentials, which now reach this project. No token was minted."
	case !body.NoToken:
		ttl, err := tokenTTL(principal.Kind, body.TokenTTL.Std(), true)
		if err == nil {
			ttl, err = s.capInviteTTL(r, project.OrganizationID, principal.Kind, ttl)
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		// A person in an organization that requires single sign-on signs in through it; a
		// token minted here would be refused on first use.
		if sso, err := s.requiresSSO(r, project.OrganizationID); err != nil {
			s.fail(w, r, err)
			return
		} else if sso && principal.Kind == domain.PrincipalHuman {
			result.Note = principal.Handle + " signs in with single sign-on, which this organization requires; no token was minted."
			break
		}
		name := "invite:" + caller.Principal.Handle
		token, err := s.store.CreateToken(r.Context(), principal.ID, name, ttl)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		result.Token = token
		if ttl > 0 {
			result.ExpiresAt = time.Now().Add(ttl).UTC().Format(time.RFC3339)
		}
		s.store.Audit(r.Context(), project.OrganizationID, project.ID, caller.Principal.ID,
			"token.minted_by_invite", "principal", principal.ID,
			map[string]any{"principal": principal.Handle, "expires_at": result.ExpiresAt})
	}

	accountState := "existing"
	if created {
		accountState = "created"
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, caller.Principal.ID,
		"member.invited", "principal", principal.ID,
		map[string]any{"principal": principal.Handle, "role": string(body.Role),
			"kind": string(principal.Kind), "state": accountState})

	s.ok(w, r, http.StatusCreated, result)
}

// validateRole rejects anything that is not a project role.
func validateRole(role domain.Role) error {
	return domain.Validate(role, []domain.Role{
		domain.RoleOrgAdmin, domain.RoleProjectAdmin, domain.RoleMaintainer,
		domain.RoleContributor, domain.RoleReviewer, domain.RoleObserver, domain.RoleRunner,
	}, "role")
}

type setMemberRoleBody struct {
	Role domain.Role `json:"role"`
}

// setMemberRole changes an existing member's role.
//
// It is the only way a role changes. The rules: the caller administers the project; neither
// the member's current role nor the new one may outrank the caller, so a project_admin can
// neither demote an org_admin nor create one; and the last administrator cannot be demoted,
// because a project nobody can administer is recoverable only from the database host.
func (s *Server) setMemberRole(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleProjectAdmin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body setMemberRoleBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := validateRole(body.Role); err != nil {
		s.fail(w, r, err)
		return
	}
	handle := r.PathValue("handle")
	target, err := s.store.GetPrincipalByHandle(r.Context(), project.OrganizationID, handle)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	current, err := s.store.RoleIn(r.Context(), project.ID, target.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !grantable(caller.Role, current) {
		s.fail(w, r, fmt.Errorf("%w: %s is a %s, which a %s cannot change",
			domain.ErrNotPermitted, handle, current, caller.Role))
		return
	}
	if !grantable(caller.Role, body.Role) {
		s.fail(w, r, fmt.Errorf("%w: a %s cannot grant %s", domain.ErrNotPermitted, caller.Role, body.Role))
		return
	}
	if current == body.Role {
		s.ok(w, r, http.StatusOK, map[string]any{"handle": target.Handle, "role": current})
		return
	}
	if current.Can(domain.RoleProjectAdmin) && !body.Role.Can(domain.RoleProjectAdmin) {
		if err := s.requireAnotherAdmin(r, project.ID, handle); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if err := s.store.SetMemberRole(r.Context(), project.ID, target.ID, body.Role); err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, caller.Principal.ID,
		"member.role_changed", "principal", target.ID,
		map[string]any{"principal": target.Handle, "from": string(current), "to": string(body.Role)})
	s.ok(w, r, http.StatusOK, map[string]any{"handle": target.Handle, "role": body.Role, "previous_role": current})
}

// requireAnotherAdmin refuses to remove or demote the project's last administrator.
func (s *Server) requireAnotherAdmin(r *http.Request, projectID domain.ID, handle string) error {
	admins, err := s.store.CountProjectAdmins(r.Context(), projectID)
	if err != nil {
		return err
	}
	if admins <= 1 {
		return fmt.Errorf("%w: %s is the only administrator; promote someone else first",
			domain.ErrNotPermitted, handle)
	}
	return nil
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
	// The same ceiling as a role change: removal is the most drastic demotion there is.
	if !grantable(caller.Role, targetRole) {
		s.fail(w, r, fmt.Errorf("%w: %s is a %s, which a %s cannot remove",
			domain.ErrNotPermitted, handle, targetRole, caller.Role))
		return
	}
	if targetRole.Can(domain.RoleProjectAdmin) {
		if err := s.requireAnotherAdmin(r, project.ID, handle); err != nil {
			s.fail(w, r, err)
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
	// Project, when set, confines the new token to that project (id or slug). The caller
	// must be a member of it. A runner mints one of these per attempt for the agent it
	// launches, so the credential an agent can read is good for one project, briefly.
	Project string `json:"project"`
}

// errScopedTokenCannotManage refuses token administration to a project-scoped token. Such a
// token is the one handed to an agent for a single attempt; letting it mint a principal-wide
// token would undo the scope, and letting it revoke tokens would let a leaked attempt token
// lock its operator out.
var errScopedTokenCannotManage = fmt.Errorf(
	"%w: a project-scoped token cannot create or revoke tokens; use your own login", domain.ErrNotPermitted)

// createToken mints an additional token for the caller, for rotation, a second machine, or a
// runner's per-attempt credential.
//
// A principal can always mint for themselves: they already hold a valid credential, so this
// grants no privilege they did not have. It exists so rotating a leaked token does not
// require an administrator.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if tokenProjectScope(r) != "" {
		s.fail(w, r, errScopedTokenCannotManage)
		return
	}
	var body createTokenBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Name == "" {
		body.Name = "cli"
	}
	body.Name = localLineage(r, body.Name)
	ttl, err := tokenTTL(p.Kind, body.TTL.Std(), false)
	if err == nil {
		body.Name, ttl, err = s.mintPolicy(r, p, body.Name, ttl)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var projectID domain.ID
	var projectSlug string
	if body.Project != "" {
		project, err := s.resolveProject(r.Context(), p, body.Project)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if _, err := s.svc.Authorize(r.Context(), p, project.ID, domain.RoleObserver); err != nil {
			s.fail(w, r, err)
			return
		}
		projectID, projectSlug = project.ID, project.Slug
	}
	token, err := s.store.CreateScopedToken(r.Context(), p.ID, projectID, body.Name, ttl)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	expires := time.Now().Add(ttl).UTC().Format(time.RFC3339)
	s.store.Audit(r.Context(), p.OrganizationID, projectID, p.ID,
		"token.created", "token", body.Name, map[string]any{"expires_at": expires})
	out := map[string]any{"name": body.Name, "token": token, "expires_at": expires}
	if projectSlug != "" {
		out["project"] = projectSlug
	}
	s.ok(w, r, http.StatusCreated, out)
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
	if tokenProjectScope(r) != "" {
		s.fail(w, r, errScopedTokenCannotManage)
		return
	}
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
	if tokenProjectScope(r) != "" {
		s.fail(w, r, errScopedTokenCannotManage)
		return
	}
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

// resetToken rotates the caller's credentials: one fresh token is minted and every other
// live token they hold is revoked, atomically. Unlike create-then-revoke by hand, there
// is no window where two credentials are valid and no ambiguity when several tokens
// share a name. The credential used for this request is among the revoked: the caller
// must use the returned token (or --save it) from here on.
func (s *Server) resetToken(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if tokenProjectScope(r) != "" {
		s.fail(w, r, errScopedTokenCannotManage)
		return
	}
	var body createTokenBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Name == "" {
		body.Name = "cli"
	}
	body.Name = localLineage(r, body.Name)
	ttl, err := tokenTTL(p.Kind, body.TTL.Std(), false)
	if err == nil {
		body.Name, ttl, err = s.mintPolicy(r, p, body.Name, ttl)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	token, revoked, err := s.store.ResetTokens(r.Context(), p.ID, body.Name, ttl)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	expires := time.Now().Add(ttl).UTC().Format(time.RFC3339)
	s.store.Audit(r.Context(), p.OrganizationID, "", p.ID,
		"token.reset", "principal", p.ID, map[string]any{"count": revoked, "expires_at": expires})
	s.ok(w, r, http.StatusCreated, map[string]any{
		"name": body.Name, "token": token, "revoked": revoked, "expires_at": expires,
	})
}

// tokenTTLDefault bounds a credential when the caller does not choose a lifetime, and is the
// most a human's credential may have.
const tokenTTLDefault = 90 * 24 * time.Hour

// tokenTTL decides a new token's lifetime. Zero or negative asks for the default, which is
// tokenTTLDefault — for every principal minting for itself, since a credential nobody chose
// to keep forever should not live forever. A human's token is capped at the default, which
// is what the CLI has always promised. The one exception is an administrator's invite of a
// service identity (a runner), where zero still means no expiry: that is a deliberate,
// audited act by someone who controls the machine it runs on.
func tokenTTL(kind domain.PrincipalKind, requested time.Duration, serviceInvite bool) (time.Duration, error) {
	human := kind == domain.PrincipalHuman || kind == ""
	switch {
	case requested < 0:
		return 0, fmt.Errorf("%w: a token lifetime cannot be negative", domain.ErrInvalidArgument)
	case requested == 0 && serviceInvite && !human:
		return 0, nil
	case requested == 0:
		return tokenTTLDefault, nil
	case human && requested > tokenTTLDefault:
		return tokenTTLDefault, nil
	}
	return requested, nil
}

// localLineage keeps a token minted by a local-sign-in token inside local sign-in: it is
// named "local:…" too, so enhanced mode disables and revokes it along with its parent. A
// local session must not be a way to mint a credential that outlives the decision to
// require tokens.
func localLineage(r *http.Request, name string) string {
	if strings.HasPrefix(tokenName(r), db.LocalTokenPrefix) && !strings.HasPrefix(name, db.LocalTokenPrefix) {
		return db.LocalTokenPrefix + name
	}
	return name
}
