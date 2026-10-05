package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/aburan28/conductor/internal/admin"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/sso"
)

// Organization policy where it touches single sign-on: admission, provisioning, and group
// → role mapping (DESIGN.md §25.8).

// ssoAdmits applies an organization's admission rules to an identity: its address must be
// in an allowed domain when the organization lists any, and a GitHub sign-in must come from
// an allowed GitHub organization when it lists any.
func ssoAdmits(pol admin.Policy, cfg sso.Config, ident sso.Identity) error {
	if !pol.EmailAllowed(ident.Email) {
		return refusal(sso.CodeDomainNotAllowed, "%s is not in a domain your organization allows to sign in (%s).",
			ident.Email, strings.Join(pol.AllowedDomains, ", "))
	}
	if cfg.Kind == sso.KindGitHub && !pol.GitHubOrgAllowed(ident.Orgs) {
		return refusal(sso.CodeOrgNotAllowed, "This GitHub account is not an active member of %s, which your organization requires "+
			"(if it is, an organization owner may need to approve this OAuth App).", strings.Join(pol.AllowedGitHubOrgs, " or "))
	}
	return nil
}

// provisionMatches reports whether an organization's policy provisions an account for
// this identity: provisioning is on, the policy is restricted and valid, and the identity
// positively matches a restriction (not merely passes an empty one).
func provisionMatches(pol admin.Policy, cfg sso.Config, ident sso.Identity) bool {
	if !pol.AutoProvision || !pol.Restricted() || pol.Validate() != nil || ssoAdmits(pol, cfg, ident) != nil {
		return false
	}
	byDomain := len(pol.AllowedDomains) > 0 && pol.EmailAllowed(ident.Email)
	byOrg := len(pol.AllowedGitHubOrgs) > 0 && cfg.Kind == sso.KindGitHub && pol.GitHubOrgAllowed(ident.Orgs)
	return byDomain || byOrg
}

// ssoProvisionByPolicy creates an account for a first sign-in that matches no registered
// address, in the one organization whose policy provisions it. ok is false when no
// organization does; several matching is a refusal, since the server cannot know which
// organization the person belongs to.
func (s *Server) ssoProvisionByPolicy(r *http.Request, cfg sso.Config, ident sso.Identity, acct db.ExternalAccount) (domain.Principal, bool, error) {
	ctx := r.Context()
	var orgs []domain.ID
	var err error
	if s.admin.opts.Locks.Patch.AutoProvision != nil && *s.admin.opts.Locks.Patch.AutoProvision {
		orgs, err = s.store.ListOrganizationIDs(ctx)
	} else {
		orgs, err = s.store.OrgsAutoProvisioning(ctx)
	}
	if err != nil {
		return domain.Principal{}, false, err
	}
	var matches []admin.Policy
	var matchOrgs []domain.ID
	for _, org := range orgs {
		pol, err := s.policyFor(ctx, org)
		if err != nil {
			return domain.Principal{}, false, err
		}
		if provisionMatches(pol, cfg, ident) {
			matches, matchOrgs = append(matches, pol), append(matchOrgs, org)
		}
	}
	switch len(matches) {
	case 0:
		return domain.Principal{}, false, nil
	case 1:
	default:
		return domain.Principal{}, false, refusal("ambiguous_org",
			"More than one organization on this server provisions accounts for %s. Ask an administrator to add you.", ident.Email)
	}
	pol, orgID := matches[0], matchOrgs[0]
	project, err := s.store.GetProjectBySlug(ctx, orgID, pol.DefaultProject)
	if errors.Is(err, domain.ErrNotFound) {
		s.logger.Warn("sso provisioning: the policy's default project does not exist", "organization", orgID, "project", pol.DefaultProject)
		return domain.Principal{}, false, refusal("not_registered", "No Conductor account is registered for %s, and your organization's "+
			"default project for new accounts does not exist. Ask an administrator.", ident.Email)
	}
	if err != nil {
		return domain.Principal{}, false, err
	}
	base := ident.Username
	if cfg.Kind != sso.KindGitHub || base == "" {
		base, _, _ = strings.Cut(ident.Email, "@")
	}
	principal, err := s.store.ProvisionSSOPrincipal(ctx, project, db.SanitizeHandle(base), ident.Name, pol.DefaultRole, acct)
	if errors.Is(err, db.ErrIdentityTaken) {
		return domain.Principal{}, false, refusal("identity_taken", "This %s account was just linked to a different Conductor account.", cfg.Label)
	}
	if err != nil {
		return domain.Principal{}, false, err
	}
	s.store.Audit(ctx, orgID, project.ID, principal.ID, "sso.provisioned", "principal", principal.ID,
		map[string]any{"provider": cfg.Name, "principal": principal.Handle, "role": string(pol.DefaultRole), "kind": "policy"})
	return principal, true, nil
}

// applyGroupMapping gives a signing-in principal the project roles its groups map to: the
// provider's (an ID token's groups claim, GitHub teams) together with the SCIM groups the
// organization's identity provider pushed. Mapping adds memberships and moves roles up or
// down within the policy's ceiling; it never touches a role above that ceiling or a
// runner, never demotes a project's last administrator, and never removes a membership —
// taking access away is deprovisioning (SCIM, or removing the member), not a sign-in.
func (s *Server) applyGroupMapping(r *http.Request, pol admin.Policy, principal domain.Principal, cfg sso.Config, groups []string) error {
	if len(pol.GroupRules) == 0 || !isHuman(principal) {
		return nil
	}
	ctx := r.Context()
	scimGroups, err := s.store.GroupsOf(ctx, principal.ID)
	if err != nil {
		return err
	}
	mapped := pol.MappedRoles(append(append([]string{}, groups...), scimGroups...))
	for _, slug := range admin.SortedProjects(mapped) {
		role := mapped[slug]
		project, err := s.store.GetProjectBySlug(ctx, principal.OrganizationID, slug)
		if errors.Is(err, domain.ErrNotFound) {
			s.logger.Warn("group mapping names a project that does not exist", "organization", principal.OrganizationID, "project", slug)
			continue
		}
		if err != nil {
			return err
		}
		current, err := s.store.RoleIn(ctx, project.ID, principal.ID)
		member := err == nil
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		switch pol.PlanRole(current, member, role) {
		case admin.RoleKeep:
			continue
		case admin.RoleAdd:
			if err := s.store.AddMember(ctx, project.ID, principal.ID, role); err != nil && !errors.Is(err, domain.ErrDuplicate) {
				return err
			}
		case admin.RoleSet:
			if current.Can(domain.RoleProjectAdmin) && !role.Can(domain.RoleProjectAdmin) {
				admins, err := s.store.CountProjectAdmins(ctx, project.ID)
				if err != nil {
					return err
				}
				if admins <= 1 {
					continue
				}
			}
			if err := s.store.SetMemberRole(ctx, project.ID, principal.ID, role); err != nil {
				return err
			}
		}
		s.store.Audit(ctx, principal.OrganizationID, project.ID, principal.ID, "member.role_mapped", "principal", principal.ID,
			map[string]any{"principal": principal.Handle, "from": string(current), "to": string(role), "provider": cfg.Name})
	}
	return nil
}
