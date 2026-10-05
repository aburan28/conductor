package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/admin"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// SCIM 2.0 provisioning (RFC 7643, RFC 7644; DESIGN.md §25.8), so an organization's
// identity provider — Okta, Microsoft Entra ID, any SCIM client — creates, updates,
// deactivates and deletes its people here, and pushes the groups that group → role mapping
// reads.
//
// It is authenticated by an organization's SCIM token (minted by an org_admin, stored
// hashed), never by a person's token, and reaches only that organization. It grants no
// role: a new user joins the organization policy's default project with its default role
// (contributor, reviewer or observer at most), and every other role comes from a person or
// from group mapping, which can never reach org_admin. Deactivating a user (active=false)
// revokes every token it holds and refuses it everywhere at once, keeping its history;
// deleting one also removes its memberships and identities.

func (s *Server) scimRoutes(m *http.ServeMux) {
	a := s.scimAuth
	m.HandleFunc("GET /scim/v2/ServiceProviderConfig", a(s.scimServiceProviderConfig))
	m.HandleFunc("GET /scim/v2/ResourceTypes", a(s.scimResourceTypes))
	m.HandleFunc("GET /scim/v2/Schemas", a(s.scimSchemas))
	m.HandleFunc("GET /scim/v2/Users", a(s.scimListUsers))
	m.HandleFunc("POST /scim/v2/Users", a(s.scimCreateUser))
	m.HandleFunc("GET /scim/v2/Users/{id}", a(s.scimGetUser))
	m.HandleFunc("PUT /scim/v2/Users/{id}", a(s.scimReplaceUser))
	m.HandleFunc("PATCH /scim/v2/Users/{id}", a(s.scimPatchUser))
	m.HandleFunc("DELETE /scim/v2/Users/{id}", a(s.scimDeleteUser))
	m.HandleFunc("GET /scim/v2/Groups", a(s.scimListGroups))
	m.HandleFunc("POST /scim/v2/Groups", a(s.scimCreateGroup))
	m.HandleFunc("GET /scim/v2/Groups/{id}", a(s.scimGetGroup))
	m.HandleFunc("PUT /scim/v2/Groups/{id}", a(s.scimReplaceGroup))
	m.HandleFunc("PATCH /scim/v2/Groups/{id}", a(s.scimPatchGroup))
	m.HandleFunc("DELETE /scim/v2/Groups/{id}", a(s.scimDeleteGroup))
}

type scimHandler func(w http.ResponseWriter, r *http.Request, orgID domain.ID)

// scimAuth authenticates a SCIM token, with the same failure throttle as every other
// credential.
func (s *Server) scimAuth(next scimHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client := clientKey(r, s.behindProxy)
		allowed, retryAfter := s.limiter.allow(client)
		header := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		var org domain.ID
		var err error = domain.ErrUnauthenticated
		if ok && strings.TrimSpace(token) != "" {
			org, err = s.store.AuthenticateSCIMToken(r.Context(), strings.TrimSpace(token))
		}
		if err != nil {
			if !errors.Is(err, domain.ErrUnauthenticated) {
				s.scimFail(w, r, err)
				return
			}
			s.limiter.fail(client)
			if !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
				scimError(w, http.StatusTooManyRequests, "", "too many failed authentication attempts")
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="conductor-scim"`)
			scimError(w, http.StatusUnauthorized, "", "a valid SCIM token is required (an organization administrator mints one in Admin → Provisioning)")
			return
		}
		s.limiter.succeed(client)
		next(w, r, org)
	}
}

const scimContentType = "application/scim+json"

func scimJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", scimContentType)
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func scimError(w http.ResponseWriter, status int, scimType, detail string) {
	body := map[string]any{"schemas": []string{admin.SchemaError}, "status": strconv.Itoa(status), "detail": detail}
	if scimType != "" {
		body["scimType"] = scimType
	}
	scimJSON(w, status, body)
}

// scimFail maps an error onto a SCIM error response.
func (s *Server) scimFail(w http.ResponseWriter, r *http.Request, err error) {
	var se *admin.SCIMError
	switch {
	case errors.As(err, &se):
		scimError(w, se.Status, se.SCIMType, se.Detail)
	case errors.Is(err, domain.ErrNotFound):
		scimError(w, http.StatusNotFound, "", "no such resource")
	case errors.Is(err, domain.ErrDuplicate):
		scimError(w, http.StatusConflict, "uniqueness", err.Error())
	case errors.Is(err, domain.ErrNotPermitted):
		scimError(w, http.StatusForbidden, "", err.Error())
	case errors.Is(err, domain.ErrInvalidArgument):
		scimError(w, http.StatusBadRequest, "invalidValue", err.Error())
	default:
		id := requestID(r)
		s.logger.Error("scim request failed", "request_id", id, "method", r.Method, "path", r.URL.Path, "error", err)
		scimError(w, http.StatusInternalServerError, "", "internal error (request "+id+")")
	}
}

func (s *Server) scimBase() string { return s.sso.publicURL + "/scim/v2" }

func scimBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// paging reads startIndex and count (RFC 7644 §3.4.2.4).
func paging(r *http.Request) (int, int) {
	start := max(intParam(r, "startIndex", 1), 1)
	count := intParam(r, "count", 100)
	if count < 0 {
		count = 0
	}
	return start, min(count, 200)
}

func listResponse(resources []any, total, start int) map[string]any {
	if resources == nil {
		resources = []any{}
	}
	return map[string]any{"schemas": []string{admin.SchemaList}, "totalResults": total,
		"startIndex": start, "itemsPerPage": len(resources), "Resources": resources}
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

func (s *Server) scimServiceProviderConfig(w http.ResponseWriter, _ *http.Request, _ domain.ID) {
	no := map[string]any{"supported": false}
	scimJSON(w, http.StatusOK, map[string]any{
		"schemas":          []string{admin.SchemaSPConfig},
		"documentationUri": "",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": 200},
		"changePassword":   no, "sort": no, "etag": no,
		"authenticationSchemes": []map[string]any{{"type": "oauthbearertoken", "name": "Bearer token", "primary": true,
			"description": "An organization SCIM token from Conductor's Admin → Provisioning"}},
		"meta": map[string]any{"resourceType": "ServiceProviderConfig", "location": s.scimBase() + "/ServiceProviderConfig"},
	})
}

func (s *Server) scimResourceTypes(w http.ResponseWriter, _ *http.Request, _ domain.ID) {
	types := []any{
		map[string]any{"schemas": []string{admin.SchemaResType}, "id": "User", "name": "User", "endpoint": "/Users",
			"schema": admin.SchemaUser, "meta": map[string]any{"resourceType": "ResourceType", "location": s.scimBase() + "/ResourceTypes/User"}},
		map[string]any{"schemas": []string{admin.SchemaResType}, "id": "Group", "name": "Group", "endpoint": "/Groups",
			"schema": admin.SchemaGroup, "meta": map[string]any{"resourceType": "ResourceType", "location": s.scimBase() + "/ResourceTypes/Group"}},
	}
	scimJSON(w, http.StatusOK, listResponse(types, len(types), 1))
}

func scimAttr(name, typ string, required bool, extra ...map[string]any) map[string]any {
	a := map[string]any{"name": name, "type": typ, "multiValued": false, "required": required, "caseExact": false,
		"mutability": "readWrite", "returned": "default", "uniqueness": "none"}
	for _, e := range extra {
		for k, v := range e {
			a[k] = v
		}
	}
	return a
}

func (s *Server) scimSchemas(w http.ResponseWriter, _ *http.Request, _ domain.ID) {
	user := map[string]any{"schemas": []string{admin.SchemaSchema}, "id": admin.SchemaUser, "name": "User",
		"description": "A person", "attributes": []any{
			scimAttr("userName", "string", true, map[string]any{"uniqueness": "server"}),
			scimAttr("externalId", "string", false),
			scimAttr("displayName", "string", false),
			scimAttr("name", "complex", false, map[string]any{"subAttributes": []any{
				scimAttr("formatted", "string", false), scimAttr("givenName", "string", false), scimAttr("familyName", "string", false)}}),
			scimAttr("emails", "complex", false, map[string]any{"multiValued": true, "subAttributes": []any{
				scimAttr("value", "string", false), scimAttr("type", "string", false), scimAttr("primary", "boolean", false)}}),
			scimAttr("active", "boolean", false),
		}, "meta": map[string]any{"resourceType": "Schema", "location": s.scimBase() + "/Schemas/" + admin.SchemaUser}}
	group := map[string]any{"schemas": []string{admin.SchemaSchema}, "id": admin.SchemaGroup, "name": "Group",
		"description": "A group, used for group → role mapping", "attributes": []any{
			scimAttr("displayName", "string", true, map[string]any{"uniqueness": "server"}),
			scimAttr("externalId", "string", false),
			scimAttr("members", "complex", false, map[string]any{"multiValued": true, "subAttributes": []any{
				scimAttr("value", "string", false, map[string]any{"mutability": "immutable"}), scimAttr("display", "string", false)}}),
		}, "meta": map[string]any{"resourceType": "Schema", "location": s.scimBase() + "/Schemas/" + admin.SchemaGroup}}
	scimJSON(w, http.StatusOK, listResponse([]any{user, group}, 2, 1))
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

func (s *Server) scimUserResource(u db.SCIMUser) map[string]any {
	out := map[string]any{
		"schemas": []string{admin.SchemaUser}, "id": u.ID, "userName": u.UserName,
		"displayName": u.DisplayName, "name": map[string]any{"formatted": u.DisplayName},
		"active": u.Active, "emails": []any{},
		"meta": map[string]any{"resourceType": "User", "created": u.CreatedAt.UTC().Format(time.RFC3339),
			"lastModified": u.CreatedAt.UTC().Format(time.RFC3339), "location": s.scimBase() + "/Users/" + string(u.ID)},
	}
	if u.ExternalID != "" {
		out["externalId"] = u.ExternalID
	}
	if u.Email != "" {
		out["emails"] = []any{map[string]any{"value": u.Email, "type": "work", "primary": true}}
	}
	return out
}

func (s *Server) scimListUsers(w http.ResponseWriter, r *http.Request, org domain.ID) {
	f, err := admin.ParseSCIMFilter(r.URL.Query().Get("filter"), "userName", "externalId", "id", "emails.value")
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	var filter db.SCIMUserFilter
	if f != nil {
		filter = db.SCIMUserFilter{Attribute: f.Attribute, Value: f.Value}
	}
	start, count := paging(r)
	users, total, err := s.store.ListSCIMUsers(r.Context(), org, filter, start, count)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	resources := make([]any, 0, len(users))
	for _, u := range users {
		resources = append(resources, s.scimUserResource(u))
	}
	scimJSON(w, http.StatusOK, listResponse(resources, total, start))
}

func (s *Server) scimGetUser(w http.ResponseWriter, r *http.Request, org domain.ID) {
	u, err := s.store.GetSCIMUser(r.Context(), org, domain.ID(r.PathValue("id")))
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	scimJSON(w, http.StatusOK, s.scimUserResource(u))
}

// provisionRolesSCIM are the roles a SCIM-created account may start with. The policy's
// validation already bounds default_role to these; the check is repeated where the role
// is granted, because this is the line SCIM must never cross.
var provisionRolesSCIM = []domain.Role{domain.RoleContributor, domain.RoleReviewer, domain.RoleObserver}

func (s *Server) scimCreateUser(w http.ResponseWriter, r *http.Request, org domain.ID) {
	body, err := scimBody(r)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	in, err := admin.ParseSCIMUser(body)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	params := db.SCIMUserParams{UserName: in.UserName, ExternalID: strings.TrimSpace(in.ExternalID),
		DisplayName: in.FullName(), Email: in.PrimaryEmail(), Active: in.Active == nil || bool(*in.Active)}
	pol, err := s.policyFor(r.Context(), org)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	if pol.DefaultProject != "" && slices.Contains(provisionRolesSCIM, pol.DefaultRole) {
		project, err := s.store.GetProjectBySlug(r.Context(), org, pol.DefaultProject)
		switch {
		case err == nil:
			params.ProjectID, params.Role = project.ID, pol.DefaultRole
		case !errors.Is(err, domain.ErrNotFound):
			s.scimFail(w, r, err)
			return
		}
	}
	u, err := s.store.CreateSCIMUser(r.Context(), org, params)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	detail := map[string]any{"principal": u.Handle, "kind": "scim"}
	if params.Role != "" {
		detail["role"] = string(params.Role)
	}
	s.store.Audit(r.Context(), org, params.ProjectID, "", "scim.user_created", "principal", string(u.ID), detail)
	w.Header().Set("Location", s.scimBase()+"/Users/"+string(u.ID))
	scimJSON(w, http.StatusCreated, s.scimUserResource(u))
}

// scimSetActive applies an active flag that changed, auditing it.
func (s *Server) scimSetActive(r *http.Request, org domain.ID, u db.SCIMUser, active bool) error {
	if u.Active == active {
		return nil
	}
	revoked, err := s.store.SetPrincipalActive(r.Context(), org, u.ID, active)
	if err != nil {
		return err
	}
	action := map[bool]string{true: "scim.user_reactivated", false: "scim.user_deactivated"}[active]
	s.store.Audit(r.Context(), org, "", "", action, "principal", string(u.ID),
		map[string]any{"principal": u.Handle, "kind": "scim", "count": revoked})
	return nil
}

func (s *Server) scimReplaceUser(w http.ResponseWriter, r *http.Request, org domain.ID) {
	id := domain.ID(r.PathValue("id"))
	u, err := s.store.GetSCIMUser(r.Context(), org, id)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	body, err := scimBody(r)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	in, err := admin.ParseSCIMUser(body)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	name, ext, email := in.FullName(), strings.TrimSpace(in.ExternalID), in.PrimaryEmail()
	if err := s.store.UpdateSCIMUser(r.Context(), org, id, db.SCIMUserUpdate{UserName: &in.UserName, ExternalID: &ext,
		DisplayName: &name, Email: &email}); err != nil {
		s.scimFail(w, r, err)
		return
	}
	if err := s.scimSetActive(r, org, u, in.Active == nil || bool(*in.Active)); err != nil {
		s.scimFail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), org, "", "", "scim.user_updated", "principal", string(id), map[string]any{"principal": u.Handle, "kind": "scim"})
	s.scimGetUser(w, r, org)
}

func (s *Server) scimPatchUser(w http.ResponseWriter, r *http.Request, org domain.ID) {
	id := domain.ID(r.PathValue("id"))
	u, err := s.store.GetSCIMUser(r.Context(), org, id)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	body, err := scimBody(r)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	patch, err := admin.ParseUserPatch(body)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	update := db.SCIMUserUpdate{UserName: patch.UserName, ExternalID: patch.ExternalID, DisplayName: patch.DisplayName, Email: patch.Email}
	if update.DisplayName == nil && (patch.GivenName != nil || patch.FamilyName != nil) {
		name := strings.TrimSpace(deref(patch.GivenName) + " " + deref(patch.FamilyName))
		update.DisplayName = &name
	}
	if update != (db.SCIMUserUpdate{}) {
		if err := s.store.UpdateSCIMUser(r.Context(), org, id, update); err != nil {
			s.scimFail(w, r, err)
			return
		}
		s.store.Audit(r.Context(), org, "", "", "scim.user_updated", "principal", string(id), map[string]any{"principal": u.Handle, "kind": "scim"})
	}
	if patch.Active != nil {
		if err := s.scimSetActive(r, org, u, *patch.Active); err != nil {
			s.scimFail(w, r, err)
			return
		}
	}
	s.scimGetUser(w, r, org)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Server) scimDeleteUser(w http.ResponseWriter, r *http.Request, org domain.ID) {
	id := domain.ID(r.PathValue("id"))
	u, err := s.store.GetSCIMUser(r.Context(), org, id)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	revoked, err := s.store.DeleteSCIMUser(r.Context(), org, id)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), org, "", "", "scim.user_deleted", "principal", string(id),
		map[string]any{"principal": u.Handle, "kind": "scim", "count": revoked})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Groups
// ---------------------------------------------------------------------------

func (s *Server) scimGroupResource(g db.SCIMGroup, withMembers bool) map[string]any {
	out := map[string]any{
		"schemas": []string{admin.SchemaGroup}, "id": g.ID, "displayName": g.DisplayName,
		"meta": map[string]any{"resourceType": "Group", "created": g.CreatedAt.UTC().Format(time.RFC3339),
			"lastModified": g.UpdatedAt.UTC().Format(time.RFC3339), "location": s.scimBase() + "/Groups/" + string(g.ID)},
	}
	if g.ExternalID != "" {
		out["externalId"] = g.ExternalID
	}
	if withMembers {
		members := []any{}
		for _, m := range g.Members {
			members = append(members, map[string]any{"value": m.ID, "display": m.Display, "$ref": s.scimBase() + "/Users/" + string(m.ID)})
		}
		out["members"] = members
	}
	return out
}

// wantMembers honours excludedAttributes=members, which Entra sends to keep large groups
// out of a listing.
func wantMembers(r *http.Request) bool {
	for _, a := range strings.Split(r.URL.Query().Get("excludedAttributes"), ",") {
		if strings.EqualFold(strings.TrimSpace(a), "members") {
			return false
		}
	}
	return true
}

func (s *Server) scimListGroups(w http.ResponseWriter, r *http.Request, org domain.ID) {
	f, err := admin.ParseSCIMFilter(r.URL.Query().Get("filter"), "displayName", "externalId", "id")
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	attr, value := "", ""
	if f != nil {
		attr, value = f.Attribute, f.Value
	}
	start, count := paging(r)
	groups, total, err := s.store.ListSCIMGroups(r.Context(), org, attr, value, start, count)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	resources := make([]any, 0, len(groups))
	for _, g := range groups {
		resources = append(resources, s.scimGroupResource(g, wantMembers(r)))
	}
	scimJSON(w, http.StatusOK, listResponse(resources, total, start))
}

func (s *Server) scimGetGroup(w http.ResponseWriter, r *http.Request, org domain.ID) {
	g, err := s.store.GetSCIMGroup(r.Context(), org, domain.ID(r.PathValue("id")))
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	scimJSON(w, http.StatusOK, s.scimGroupResource(g, wantMembers(r)))
}

func ids(in []string) []domain.ID {
	out := make([]domain.ID, len(in))
	for i, s := range in {
		out[i] = domain.ID(s)
	}
	return out
}

func (s *Server) scimCreateGroup(w http.ResponseWriter, r *http.Request, org domain.ID) {
	body, err := scimBody(r)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	in, members, err := admin.ParseSCIMGroup(body)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	g, err := s.store.CreateSCIMGroup(r.Context(), org, in.DisplayName, strings.TrimSpace(in.ExternalID), ids(members))
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), org, "", "", "scim.group_created", "scim_group", string(g.ID),
		map[string]any{"title": g.DisplayName, "count": len(g.Members), "kind": "scim"})
	w.Header().Set("Location", s.scimBase()+"/Groups/"+string(g.ID))
	scimJSON(w, http.StatusCreated, s.scimGroupResource(g, true))
}

func (s *Server) scimReplaceGroup(w http.ResponseWriter, r *http.Request, org domain.ID) {
	body, err := scimBody(r)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	in, members, err := admin.ParseSCIMGroup(body)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	ext := strings.TrimSpace(in.ExternalID)
	replace := ids(members)
	s.scimChangeGroup(w, r, org, db.SCIMGroupChange{DisplayName: &in.DisplayName, ExternalID: &ext, Replace: &replace})
}

func (s *Server) scimPatchGroup(w http.ResponseWriter, r *http.Request, org domain.ID) {
	body, err := scimBody(r)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	patch, err := admin.ParseGroupPatch(body)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	change := db.SCIMGroupChange{DisplayName: patch.DisplayName, ExternalID: patch.ExternalID,
		Add: ids(patch.Add), Remove: ids(patch.Remove)}
	if patch.ReplaceMembers != nil {
		replace := ids(*patch.ReplaceMembers)
		change.Replace = &replace
	}
	if change.DisplayName != nil && strings.TrimSpace(*change.DisplayName) == "" {
		s.scimFail(w, r, &admin.SCIMError{Status: http.StatusBadRequest, SCIMType: "invalidValue", Detail: "displayName cannot be empty"})
		return
	}
	s.scimChangeGroup(w, r, org, change)
}

func (s *Server) scimChangeGroup(w http.ResponseWriter, r *http.Request, org domain.ID, c db.SCIMGroupChange) {
	id := domain.ID(r.PathValue("id"))
	if err := s.store.ChangeSCIMGroup(r.Context(), org, id, c); err != nil {
		s.scimFail(w, r, err)
		return
	}
	g, err := s.store.GetSCIMGroup(r.Context(), org, id)
	if err != nil {
		s.scimFail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), org, "", "", "scim.group_updated", "scim_group", string(id),
		map[string]any{"title": g.DisplayName, "count": len(g.Members), "kind": "scim"})
	scimJSON(w, http.StatusOK, s.scimGroupResource(g, wantMembers(r)))
}

func (s *Server) scimDeleteGroup(w http.ResponseWriter, r *http.Request, org domain.ID) {
	id := domain.ID(r.PathValue("id"))
	if err := s.store.DeleteSCIMGroup(r.Context(), org, id); err != nil {
		s.scimFail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), org, "", "", "scim.group_deleted", "scim_group", string(id), map[string]any{"kind": "scim"})
	w.WriteHeader(http.StatusNoContent)
}
