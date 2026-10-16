package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/modelsrc"
)

// Inference endpoints (internal/modelsrc): the APIs a project routes to, and the models each
// reports serving. Discovery reads model names only; keys stay in the project secret vault and
// are referenced by name.
//
// Maintainers add, change, refresh and remove endpoints. Every member may read them and the
// models they serve, which is what the dashboard's model dropdown is filled from.

func (s *Server) inferenceRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("GET /v1/projects/{project}/inference/endpoints", auth(s.listInferenceEndpoints))
	m.HandleFunc("POST /v1/projects/{project}/inference/endpoints", auth(s.createInferenceEndpoint))
	m.HandleFunc("PUT /v1/projects/{project}/inference/endpoints/{endpoint}", auth(s.updateInferenceEndpoint))
	m.HandleFunc("DELETE /v1/projects/{project}/inference/endpoints/{endpoint}", auth(s.deleteInferenceEndpoint))
	m.HandleFunc("POST /v1/projects/{project}/inference/endpoints/{endpoint}/refresh", auth(s.refreshInferenceEndpoint))
	m.HandleFunc("GET /v1/projects/{project}/inference/models", auth(s.listInferenceModels))
	m.HandleFunc("PUT /v1/projects/{project}/models/profiles/{alias}/{harness}/pin", auth(s.pinProfile))
	m.HandleFunc("DELETE /v1/projects/{project}/models/profiles/{alias}/{harness}/pin", auth(s.unpinProfile))
}

var endpointNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// minRefreshGap is how soon an endpoint may be refreshed again by request; sooner than this
// the stored answer is returned. It keeps a stuck button from becoming a request flood
// against someone's provider account.
const minRefreshGap = 5 * time.Second

func (s *Server) inferenceOff(w http.ResponseWriter, r *http.Request) bool {
	if s.models != nil {
		return false
	}
	s.ok(w, r, http.StatusServiceUnavailable, ErrorBody{
		Error: "model discovery is not enabled on this server", Code: "not_configured"})
	return true
}

type endpointList struct {
	Endpoints []domain.InferenceEndpoint `json:"endpoints"`
	Presets   []modelsrc.Preset          `json:"presets"`
}

func (s *Server) listInferenceEndpoints(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	eps, err := s.store.ListInferenceEndpoints(r.Context(), project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, endpointList{Endpoints: eps, Presets: modelsrc.Presets})
}

type endpointRequest struct {
	Name         string `json:"name"`
	Preset       string `json:"preset,omitempty"`
	Kind         string `json:"kind,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	APIKeySecret string `json:"api_key_secret,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
}

type endpointResponse struct {
	Endpoint domain.InferenceEndpoint `json:"endpoint"`
	Refresh  *modelsrc.Result         `json:"refresh,omitempty"`
	// RefreshError is why the first discovery failed. The endpoint is saved regardless, so a
	// typo in the key name can be fixed without retyping the rest.
	RefreshError string `json:"refresh_error,omitempty"`
}

func (s *Server) createInferenceEndpoint(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.inferenceOff(w, r) {
		return
	}
	var body endpointRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	presetKey := false
	ep := domain.InferenceEndpoint{
		OrganizationID: project.OrganizationID, ProjectID: project.ID,
		Name: strings.TrimSpace(body.Name), Kind: domain.InferenceKind(body.Kind),
		BaseURL: strings.TrimSpace(body.BaseURL), APIKeySecret: strings.TrimSpace(body.APIKeySecret),
		Enabled: body.Enabled == nil || *body.Enabled,
	}
	if body.Preset != "" {
		preset, ok := modelsrc.PresetByName(body.Preset)
		if !ok {
			s.fail(w, r, domain.ErrInvalidArgument)
			return
		}
		if ep.Name == "" {
			ep.Name = preset.Name
		}
		if ep.Kind == "" {
			ep.Kind = preset.Kind
		}
		if ep.BaseURL == "" {
			ep.BaseURL = preset.BaseURL
		}
		// A hosted preset names the secret its provider's key usually lives in. It is attached
		// even if the secret does not exist yet, so adding the key afterwards is all that is
		// left to do; until then the endpoint reports the missing secret by name.
		if ep.APIKeySecret == "" {
			ep.APIKeySecret = preset.KeyHint
			presetKey = true
		}
	}
	if !endpointNameRe.MatchString(ep.Name) || !ep.Kind.Valid() {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	if err := modelsrc.ValidateBaseURL(ep.BaseURL); err != nil {
		s.fail(w, r, err)
		return
	}
	if !presetKey {
		if err := s.checkKeySecret(r.Context(), project.ID, ep.APIKeySecret); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	created, err := s.store.CreateInferenceEndpoint(r.Context(), ep, p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID,
		"inference.create", "inference_endpoint", created.Name, map[string]any{"kind": string(created.Kind)})

	resp := endpointResponse{Endpoint: created}
	if created.Enabled {
		res, err := s.models.Refresh(r.Context(), created)
		resp.Endpoint = res.Endpoint
		if err != nil {
			resp.RefreshError = err.Error()
		} else {
			resp.Refresh = &res
		}
	}
	s.ok(w, r, http.StatusCreated, resp)
}

// checkKeySecret rejects a key reference the vault cannot satisfy, so a typo is an error
// now rather than a failed refresh later.
func (s *Server) checkKeySecret(ctx context.Context, projectID domain.ID, name string) error {
	if name == "" {
		return nil
	}
	if s.secrets == nil {
		return errors.Join(domain.ErrInvalidArgument, errors.New("the secret vault is not enabled, so api_key_secret cannot be used"))
	}
	views, err := s.secrets.List(ctx, projectID)
	if err != nil {
		return err
	}
	for _, v := range views {
		if v.Name == name {
			return nil
		}
	}
	return errors.Join(domain.ErrInvalidArgument, errors.New("no secret named "+name+" in this project"))
}

func (s *Server) updateInferenceEndpoint(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.inferenceOff(w, r) {
		return
	}
	cur, err := s.store.GetInferenceEndpoint(r.Context(), project.ID, r.PathValue("endpoint"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Pointers tell "not sent" from "sent empty": an empty api_key_secret drops the key.
	var body struct {
		BaseURL      *string `json:"base_url"`
		APIKeySecret *string `json:"api_key_secret"`
		Enabled      *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	baseURL, keySecret, enabled := cur.BaseURL, cur.APIKeySecret, cur.Enabled
	if body.BaseURL != nil {
		baseURL = strings.TrimSpace(*body.BaseURL)
	}
	if body.APIKeySecret != nil {
		keySecret = strings.TrimSpace(*body.APIKeySecret)
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if err := modelsrc.ValidateBaseURL(baseURL); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.checkKeySecret(r.Context(), project.ID, keySecret); err != nil {
		s.fail(w, r, err)
		return
	}
	updated, err := s.store.UpdateInferenceEndpoint(r.Context(), project.ID, cur.ID, baseURL, keySecret, enabled)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID,
		"inference.update", "inference_endpoint", updated.Name, map[string]any{"kind": string(updated.Kind)})
	s.ok(w, r, http.StatusOK, endpointResponse{Endpoint: updated})
}

func (s *Server) deleteInferenceEndpoint(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cur, err := s.store.GetInferenceEndpoint(r.Context(), project.ID, r.PathValue("endpoint"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.DeleteInferenceEndpoint(r.Context(), project.ID, cur.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID,
		"inference.delete", "inference_endpoint", cur.Name, map[string]any{"kind": string(cur.Kind)})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) refreshInferenceEndpoint(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.inferenceOff(w, r) {
		return
	}
	ep, err := s.store.GetInferenceEndpoint(r.Context(), project.ID, r.PathValue("endpoint"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if ep.LastRefreshAt != nil && time.Since(*ep.LastRefreshAt) < minRefreshGap {
		s.ok(w, r, http.StatusOK, endpointResponse{Endpoint: ep})
		return
	}
	res, err := s.models.Refresh(r.Context(), ep)
	resp := endpointResponse{Endpoint: res.Endpoint, Refresh: &res}
	if err != nil {
		// The endpoint was reached or not; either way the refresh itself worked as a request.
		resp.Refresh, resp.RefreshError = nil, err.Error()
	}
	s.ok(w, r, http.StatusOK, resp)
}

// ModelChoice is one row of the model dropdown: a model some endpoint serves, and whether the
// catalog already uses it.
type ModelChoice struct {
	Endpoint    string               `json:"endpoint"`
	Kind        domain.InferenceKind `json:"kind"`
	Model       string               `json:"model"`
	DisplayName string               `json:"display_name,omitempty"`
	Released    *time.Time           `json:"released,omitempty"`
	FirstSeenAt time.Time            `json:"first_seen_at"`
	// New marks a model first seen in the last week, after the endpoint's first listing, so
	// that listing does not mark everything new.
	New bool `json:"new"`
	// UsedBy lists "alias/harness" catalog profiles currently resolved to this model.
	UsedBy []string `json:"used_by"`
	// Missing is set when the endpoint no longer lists the model.
	Missing bool `json:"missing,omitempty"`
}

func (s *Server) listInferenceModels(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	endpoints, err := s.store.ListInferenceEndpoints(ctx, project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	profiles, err := s.store.ListModelProfiles(ctx, project.OrganizationID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	onlyEndpoint := r.URL.Query().Get("endpoint")
	includeMissing := r.URL.Query().Get("missing") == "1"
	out := []ModelChoice{}
	for _, ep := range endpoints {
		if onlyEndpoint != "" && ep.Name != onlyEndpoint {
			continue
		}
		all, err := s.store.ListDiscoveredModels(ctx, ep.ID, true)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		// A model is "new" if it appeared after the endpoint's first listing, which is when
		// everything it served was discovered at once; that moment is the earliest first-seen.
		baseline := time.Time{}
		for _, m := range all {
			if baseline.IsZero() || m.FirstSeenAt.Before(baseline) {
				baseline = m.FirstSeenAt
			}
		}
		models := all[:0:0]
		for _, m := range all {
			if includeMissing || m.MissingSince == nil {
				models = append(models, m)
			}
		}
		// Profiles that select from this endpoint may namespace the id ("vllm/").
		prefix := ""
		for _, pr := range profiles {
			if pr.Select != nil && pr.Select.Endpoint == ep.Name && pr.Select.ProjectID == project.ID {
				prefix = pr.Select.Prefix
			}
		}
		for _, m := range models {
			choice := ModelChoice{
				Endpoint: ep.Name, Kind: ep.Kind, Model: m.Model, DisplayName: m.DisplayName,
				Released: m.CreatedUpstream, FirstSeenAt: m.FirstSeenAt, UsedBy: []string{},
				Missing: m.MissingSince != nil,
			}
			choice.New = time.Since(m.FirstSeenAt) < 7*24*time.Hour && m.FirstSeenAt.After(baseline.Add(time.Minute))
			for _, pr := range profiles {
				if pr.Model == m.Model || (prefix != "" && pr.Model == prefix+m.Model) {
					choice.UsedBy = append(choice.UsedBy, pr.Alias+"/"+pr.Harness)
				}
			}
			out = append(out, choice)
		}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"models": out})
}

// pinProfile is the dropdown's action: a person chooses which model an alias runs on a
// harness, from what the endpoints serve or any id they type. Discovery and bootstrap then
// leave that profile alone until it is unpinned.
func (s *Server) pinProfile(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" || len(model) > 200 || strings.ContainsAny(model, " \t\r\n") {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	before, _ := s.profile(r.Context(), project.OrganizationID, r.PathValue("alias"), r.PathValue("harness"))
	profile, err := s.store.PinProfileModel(r.Context(), project.OrganizationID, r.PathValue("alias"), r.PathValue("harness"), model)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID, "model.pinned", "model_profile",
		profile.Alias+"/"+profile.Harness, map[string]any{"model_alias": profile.Alias, "harness": profile.Harness,
			"from": before.Model, "to": profile.Model})
	s.ok(w, r, http.StatusOK, map[string]any{"profile": profile})
}

func (s *Server) unpinProfile(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	profile, err := s.store.UnpinProfile(r.Context(), project.OrganizationID, r.PathValue("alias"), r.PathValue("harness"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID, "model.unpinned", "model_profile",
		profile.Alias+"/"+profile.Harness, map[string]any{"model_alias": profile.Alias, "harness": profile.Harness})
	// A profile that selects its model goes back to following discovery straight away.
	if profile.Select != nil {
		if _, err := modelsrc.ResolveProject(r.Context(), s.store, project); err != nil {
			s.logger.Warn("resolving after unpin", "error", err)
		}
		if again, err := s.profile(r.Context(), project.OrganizationID, profile.Alias, profile.Harness); err == nil {
			profile = again
		}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"profile": profile})
}

func (s *Server) profile(ctx context.Context, orgID domain.ID, alias, harness string) (domain.ModelProfile, error) {
	profiles, err := s.store.ListModelProfiles(ctx, orgID)
	if err != nil {
		return domain.ModelProfile{}, err
	}
	for _, p := range profiles {
		if p.Alias == alias && p.Harness == harness {
			return p, nil
		}
	}
	return domain.ModelProfile{}, domain.ErrNotFound
}
