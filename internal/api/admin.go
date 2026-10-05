package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/admin"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// The admin area (DESIGN.md §25.8): an organization's policy, branding, members, SCIM
// tokens, audit log and the server's effective configuration. Everything under /v1/admin is
// for the organization's administrators — principals holding org_admin in one of its
// projects — and is refused to a project-scoped token like every other route that names no
// project. What every member may read about their organization (its branding and which
// areas are on) is GET /v1/org; what a sign-in page shows before anyone signs in is
// GET /v1/branding.

func (s *Server) adminRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("GET /v1/org", auth(s.orgInfo))
	m.HandleFunc("GET /v1/org/logo", auth(s.orgLogo))
	m.HandleFunc("GET /v1/branding", s.publicBranding)
	m.HandleFunc("GET /v1/branding/logo", s.publicLogo)
	m.HandleFunc("GET /v1/admin/policy", auth(s.adminGetPolicy))
	m.HandleFunc("PATCH /v1/admin/policy", auth(s.adminPatchPolicy))
	m.HandleFunc("PUT /v1/admin/branding/logo", auth(s.adminPutLogo))
	m.HandleFunc("DELETE /v1/admin/branding/logo", auth(s.adminDeleteLogo))
	m.HandleFunc("GET /v1/admin/config", auth(s.adminConfig))
	m.HandleFunc("GET /v1/admin/members", auth(s.adminMembers))
	m.HandleFunc("POST /v1/admin/members/{handle}/deactivate", auth(s.adminDeactivate))
	m.HandleFunc("POST /v1/admin/members/{handle}/reactivate", auth(s.adminReactivate))
	m.HandleFunc("GET /v1/admin/audit", auth(s.adminAudit))
	m.HandleFunc("GET /v1/admin/scim/tokens", auth(s.adminListSCIMTokens))
	m.HandleFunc("POST /v1/admin/scim/tokens", auth(s.adminCreateSCIMToken))
	m.HandleFunc("DELETE /v1/admin/scim/tokens/{id}", auth(s.adminRevokeSCIMToken))
}

// errNotOrgAdmin refuses the admin area to everyone but an organization's administrators.
var errNotOrgAdmin = fmt.Errorf("%w: only an organization administrator (org_admin) can do this", domain.ErrNotPermitted)

// requireOrgAdmin refuses unless p administers its organization.
func (s *Server) requireOrgAdmin(r *http.Request, p domain.Principal) error {
	ok, err := s.store.IsOrgAdmin(r.Context(), p.ID, p.OrganizationID)
	if err != nil {
		return err
	}
	if !ok {
		return errNotOrgAdmin
	}
	return nil
}

// brandingView is branding as clients render it.
type brandingView struct {
	admin.Branding
	// OnAccent is the text color for the accent background.
	OnAccent string `json:"on_accent,omitempty"`
	HasLogo  bool   `json:"has_logo"`
	LogoSHA  string `json:"logo_sha256,omitempty"`
}

func (s *Server) brandingFor(r *http.Request, orgID domain.ID, pol admin.Policy) (brandingView, error) {
	v := brandingView{Branding: pol.Branding}
	if v.AccentColor != "" {
		v.OnAccent = admin.OnAccent(v.AccentColor)
	}
	logo, ok, err := s.logoFor(r, orgID)
	if err != nil {
		return v, err
	}
	v.HasLogo, v.LogoSHA = ok, logo.SHA256
	return v, nil
}

// logoFor is the logo in force: the config file's, else the organization's.
func (s *Server) logoFor(r *http.Request, orgID domain.ID) (admin.Logo, bool, error) {
	if l := s.admin.opts.Locks.Logo; l != nil {
		return *l, true, nil
	}
	if orgID == "" {
		return admin.Logo{}, false, nil
	}
	return s.store.GetOrgLogo(r.Context(), orgID)
}

type featureView struct {
	admin.Feature
	Enabled bool `json:"enabled"`
	Locked  bool `json:"locked"`
}

func (s *Server) featureViews(pol admin.Policy) []featureView {
	out := make([]featureView, 0, len(admin.Features))
	for _, f := range admin.Features {
		out = append(out, featureView{Feature: f, Enabled: pol.FeatureOn(f.Key),
			Locked: s.admin.opts.Locks.Locked(admin.FeatureKey(f.Key))})
	}
	return out
}

// orgInfo is what every member may know about their organization: its name, branding,
// which advanced areas are on, whether it requires single sign-on, and whether the caller
// administers it.
func (s *Server) orgInfo(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	org, err := s.store.GetOrganization(r.Context(), p.OrganizationID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pol, err := s.policyFor(r.Context(), org.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	branding, err := s.brandingFor(r, org.ID, pol)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	isAdmin, err := s.store.IsOrgAdmin(r.Context(), p.ID, org.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	features := map[string]bool{}
	for _, f := range admin.Features {
		features[f.Key] = pol.FeatureOn(f.Key)
	}
	s.ok(w, r, http.StatusOK, map[string]any{
		"organization": map[string]any{"id": org.ID, "slug": org.Slug, "name": org.Name},
		"branding":     branding, "features": features, "require_sso": pol.RequireSSO, "is_org_admin": isAdmin,
	})
}

// serveLogo writes a logo as the image it is, and as nothing else: its type was sniffed at
// upload and is one of PNG, JPEG or GIF; nosniff (set for every response) keeps a browser
// from reconsidering, and the sandboxing CSP keeps the response inert if opened directly.
func (s *Server) serveLogo(w http.ResponseWriter, r *http.Request, logo admin.Logo, ok bool) {
	if !ok {
		s.fail(w, r, fmt.Errorf("%w: no logo is set", domain.ErrNotFound))
		return
	}
	etag := `"` + logo.SHA256 + `"`
	h := w.Header()
	h.Set("Content-Type", logo.ContentType)
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Content-Disposition", "inline")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logo.Data)
}

func (s *Server) orgLogo(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	logo, ok, err := s.logoFor(r, p.OrganizationID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.serveLogo(w, r, logo, ok)
}

// publicBranding is the sign-in page's branding, before anyone has signed in: the primary
// organization's (the machine owner's, or the only one), with the config file's applied.
// Branding is public by design — it is what the sign-in page shows — so it says nothing
// else about the organization.
func (s *Server) publicBranding(w http.ResponseWriter, r *http.Request) {
	org, found, err := s.store.PrimaryOrganization(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pol := s.admin.opts.Locks.Effective(admin.Defaults())
	if found {
		if pol, err = s.policyFor(r.Context(), org.ID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	v, err := s.brandingFor(r, org.ID, pol)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, v)
}

func (s *Server) publicLogo(w http.ResponseWriter, r *http.Request) {
	org, _, err := s.store.PrimaryOrganization(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	logo, ok, err := s.logoFor(r, org.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.serveLogo(w, r, logo, ok)
}

// ---------------------------------------------------------------------------
// Policy
// ---------------------------------------------------------------------------

type ssoProviderAdminView struct {
	ssoProviderInfo
	Restricted          bool     `json:"restricted"`
	Domains             []string `json:"domains,omitempty"`
	Orgs                []string `json:"orgs,omitempty"`
	GroupsClaim         string   `json:"groups_claim,omitempty"`
	TrustedEmailDomains []string `json:"trusted_email_domains,omitempty"`
	RedirectURI         string   `json:"redirect_uri"`
}

func (s *Server) adminGetPolicy(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if err := s.requireOrgAdmin(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	stored, err := s.store.GetOrgPolicy(r.Context(), p.OrganizationID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	effective := s.admin.opts.Locks.Effective(stored.Policy)
	providers := []ssoProviderAdminView{}
	for _, name := range s.sso.order {
		cfg := s.sso.providers[name].Config()
		providers = append(providers, ssoProviderAdminView{
			ssoProviderInfo: ssoProviderInfo{Name: cfg.Name, Label: cfg.Label, Type: cfg.Kind},
			Restricted:      cfg.Restricted(), Domains: cfg.Domains, Orgs: cfg.Orgs, GroupsClaim: cfg.GroupsClaim,
			TrustedEmailDomains: cfg.TrustedEmailDomains, RedirectURI: s.sso.redirectURI(name)})
	}
	branding, err := s.brandingFor(r, p.OrganizationID, effective)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	problems := []string{}
	var verr *admin.ValidationError
	if err := effective.Validate(); errors.As(err, &verr) {
		problems = verr.Problems
	}
	out := map[string]any{
		"policy": effective, "locked": nonNil(s.admin.opts.Locks.Keys()), "features": s.featureViews(effective),
		"branding": branding, "providers": providers, "problems": problems,
		"server_sso": map[string]any{"token_ttl": s.sso.tokenTTL().String(),
			"auto_provision_role": s.sso.opts.AutoProvisionRole, "default_project": s.sso.opts.DefaultProject},
		"config_file": s.admin.opts.ConfigFile,
	}
	if stored.UpdatedAt != nil {
		out["updated_at"] = stored.UpdatedAt
		if stored.UpdatedBy != "" {
			if by, err := s.store.GetPrincipal(r.Context(), stored.UpdatedBy); err == nil {
				out["updated_by"] = by.Handle
			}
		}
	}
	s.ok(w, r, http.StatusOK, out)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// adminPatchPolicy changes some of an organization's settings. A setting the config file
// locks is refused, the resulting policy must validate as a whole, and the change is
// audited with the names of the settings it touched (never their values: a login banner
// or a domain list is the organization's business, not the audit log's).
func (s *Server) adminPatchPolicy(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if err := s.requireOrgAdmin(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	var patch admin.Patch
	if err := decodeStrict(r, &patch); err != nil {
		s.fail(w, r, err)
		return
	}
	if patch.Empty() {
		s.fail(w, r, fmt.Errorf("%w: the request changes no setting", domain.ErrInvalidArgument))
		return
	}
	locks := s.admin.opts.Locks
	if err := locks.CheckUnlocked(patch); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	stored, err := s.store.GetOrgPolicy(ctx, p.OrganizationID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	next := patch.Apply(stored.Policy)
	effective := locks.Effective(next)
	if err := effective.Validate(); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.checkPolicyAgainstServer(r, p.OrganizationID, effective); err != nil {
		s.fail(w, r, err)
		return
	}
	// Requiring single sign-on from a session that did not come from it is how an
	// administrator locks everyone out, themselves first. Doing it from an SSO session
	// proves at least one administrator can still get in.
	if patch.RequireSSO != nil && *patch.RequireSSO && ssoTokenProvider(tokenName(r)) == "" &&
		!strings.HasPrefix(tokenName(r), db.LocalTokenPrefix) {
		s.fail(w, r, fmt.Errorf("%w: sign in with single sign-on before requiring it, so you are not locked out "+
			"(your current session came from a token)", domain.ErrInvalidArgument))
		return
	}
	if err := s.store.PutOrgPolicy(ctx, p.OrganizationID, next, p.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.forgetPolicy(p.OrganizationID)
	// The detail names settings, not values. "resources" is the audit allowlist's word for
	// the things an action touched.
	s.store.Audit(ctx, p.OrganizationID, "", p.ID, "org.policy_updated", "organization", string(p.OrganizationID),
		map[string]any{"resources": patch.Keys()})
	s.adminGetPolicy(w, r, p)
}

// checkPolicyAgainstServer checks what only the server can: that single sign-on can be
// required (a provider is configured), and that the projects the policy names exist.
func (s *Server) checkPolicyAgainstServer(r *http.Request, orgID domain.ID, pol admin.Policy) error {
	var problems []string
	if pol.RequireSSO && len(s.sso.order) == 0 {
		problems = append(problems, "require_sso: no single sign-on provider is configured on this server, "+
			"so nobody could sign in; configure one (sso.providers in the config file, or --sso-provider) first")
	}
	if pol.AutoProvision && len(s.sso.order) == 0 {
		problems = append(problems, "auto_provision: no single sign-on provider is configured on this server")
	}
	projects := []string{}
	if pol.DefaultProject != "" {
		projects = append(projects, pol.DefaultProject)
	}
	for _, rule := range pol.GroupRules {
		if rule.Project != "" && !slices.Contains(projects, rule.Project) {
			projects = append(projects, rule.Project)
		}
	}
	for _, slug := range projects {
		if _, err := s.store.GetProjectBySlug(r.Context(), orgID, slug); errors.Is(err, domain.ErrNotFound) {
			problems = append(problems, fmt.Sprintf("no project %q in this organization", slug))
		} else if err != nil {
			return err
		}
	}
	if len(problems) > 0 {
		return &admin.ValidationError{Problems: problems}
	}
	return nil
}

type logoBody struct {
	ContentType string `json:"content_type"`
	// Data is the image, base64-encoded.
	Data string `json:"data"`
}

func (s *Server) adminPutLogo(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if err := s.requireOrgAdmin(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.admin.opts.Locks.Logo != nil {
		s.fail(w, r, &admin.ErrLocked{Keys: []string{admin.KeyLogo}})
		return
	}
	var body logoBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil {
		s.fail(w, r, fmt.Errorf("%w: data must be base64", domain.ErrInvalidArgument))
		return
	}
	logo, err := admin.CheckLogo(data, body.ContentType)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.PutOrgLogo(r.Context(), p.OrganizationID, logo); err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), p.OrganizationID, "", p.ID, "org.logo_set", "organization", string(p.OrganizationID),
		map[string]any{"kind": logo.ContentType})
	s.ok(w, r, http.StatusOK, map[string]any{"content_type": logo.ContentType, "sha256": logo.SHA256, "bytes": len(logo.Data)})
}

func (s *Server) adminDeleteLogo(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if err := s.requireOrgAdmin(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.admin.opts.Locks.Logo != nil {
		s.fail(w, r, &admin.ErrLocked{Keys: []string{admin.KeyLogo}})
		return
	}
	if err := s.store.DeleteOrgLogo(r.Context(), p.OrganizationID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), p.OrganizationID, "", p.ID, "org.logo_removed", "organization", string(p.OrganizationID), nil)
	s.ok(w, r, http.StatusNoContent, nil)
}

// adminConfig shows the server's effective configuration, secrets redacted, to the
// administrators of the primary organization (the machine owner's, or the only one): the
// configuration is the server's, and on a server hosting several organizations it is not
// every tenant's business.
func (s *Server) adminConfig(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if err := s.requireOrgAdmin(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	if org, found, err := s.store.PrimaryOrganization(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	} else if found && org.ID != p.OrganizationID {
		s.fail(w, r, fmt.Errorf("%w: the server's configuration is shown to the administrators of the organization that runs it", domain.ErrNotPermitted))
		return
	}
	entries := s.admin.opts.Config
	if entries == nil {
		entries = []ConfigEntry{}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"config_file": s.admin.opts.ConfigFile, "settings": entries,
		"locked": nonNil(s.admin.opts.Locks.Keys())})
}

// ---------------------------------------------------------------------------
// Members
// ---------------------------------------------------------------------------

// adminMembers lists every account of the organization with its project roles and linked
// identities. Changing a role goes through PATCH /v1/projects/{project}/members/{handle},
// and unlinking an identity through DELETE …/identities/{provider}, so their rules (no
// grant above the caller, the last administrator) apply unchanged.
func (s *Server) adminMembers(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if err := s.requireOrgAdmin(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	members, err := s.store.ListOrgMembers(r.Context(), p.OrganizationID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"members": members})
}

func (s *Server) adminDeactivate(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	s.adminSetActive(w, r, p, false)
}

func (s *Server) adminReactivate(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	s.adminSetActive(w, r, p, true)
}

// adminSetActive deactivates or reactivates an account: an organization-wide switch, so an
// org_admin's. Deactivating yourself is refused (it would end the request's own session
// with no way back), as is deactivating a project's last administrator.
func (s *Server) adminSetActive(w http.ResponseWriter, r *http.Request, p domain.Principal, active bool) {
	if err := s.requireOrgAdmin(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	target, err := s.store.GetPrincipalByHandle(r.Context(), p.OrganizationID, r.PathValue("handle"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if target.ID == p.ID && !active {
		s.fail(w, r, fmt.Errorf("%w: you cannot deactivate yourself", domain.ErrNotPermitted))
		return
	}
	revoked, err := s.store.SetPrincipalActive(r.Context(), p.OrganizationID, target.ID, active)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	action := map[bool]string{true: "member.reactivated", false: "member.deactivated"}[active]
	s.store.Audit(r.Context(), p.OrganizationID, "", p.ID, action, "principal", target.ID,
		map[string]any{"principal": target.Handle, "count": revoked})
	s.ok(w, r, http.StatusOK, map[string]any{"handle": target.Handle, "active": active, "revoked_tokens": revoked})
}

// ---------------------------------------------------------------------------
// SCIM tokens
// ---------------------------------------------------------------------------

func (s *Server) adminListSCIMTokens(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if err := s.requireOrgAdmin(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	tokens, err := s.store.ListSCIMTokens(r.Context(), p.OrganizationID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"tokens": tokens, "base_url": s.sso.publicURL + "/scim/v2"})
}

func (s *Server) adminCreateSCIMToken(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if err := s.requireOrgAdmin(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		body.Name = "scim"
	}
	if len(body.Name) > 64 || !admin.PlainLine(body.Name) {
		s.fail(w, r, fmt.Errorf("%w: name must be one line of at most 64 characters", domain.ErrInvalidArgument))
		return
	}
	tok, secret, err := s.store.CreateSCIMToken(r.Context(), p.OrganizationID, p.ID, body.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), p.OrganizationID, "", p.ID, "scim.token_created", "scim_token", string(tok.ID),
		map[string]any{"title": tok.Name})
	s.ok(w, r, http.StatusCreated, map[string]any{"id": tok.ID, "name": tok.Name, "token": secret,
		"base_url": s.sso.publicURL + "/scim/v2", "created_at": tok.CreatedAt})
}

func (s *Server) adminRevokeSCIMToken(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if err := s.requireOrgAdmin(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	id := domain.ID(r.PathValue("id"))
	if err := s.store.RevokeSCIMToken(r.Context(), p.OrganizationID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), p.OrganizationID, "", p.ID, "scim.token_revoked", "scim_token", string(id), nil)
	s.ok(w, r, http.StatusNoContent, nil)
}

// parseTimeParam reads an RFC 3339 time or a date (YYYY-MM-DD, UTC midnight).
func parseTimeParam(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%w: %q is not a time (RFC 3339, or YYYY-MM-DD)", domain.ErrInvalidArgument, v)
}

// decodeStrict is decode that refuses unknown fields: a misspelled setting in a policy
// change must fail, not be silently ignored while the request reports success.
func decodeStrict[T any](r *http.Request, dst *T) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.Join(domain.ErrInvalidArgument, err)
	}
	return nil
}
