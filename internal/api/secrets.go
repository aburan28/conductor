package api

import (
	"encoding/json"
	"net/http"

	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/secrets"
)

// Project secrets (internal/secrets): paste credentials into Conductor; they are sealed at
// rest and retrieved by MCP agents, `conductor wrap`, or `conductor secrets env`/`inject`.
//
// Maintainers write and delete. Contributors (and above) may list metadata and open values
// for their own session. Plaintext never appears in list responses, SSE, or task metadata.

func (s *Server) secretRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("GET /v1/projects/{project}/secrets", auth(s.listSecrets))
	m.HandleFunc("POST /v1/projects/{project}/secrets", auth(s.setSecret))
	m.HandleFunc("GET /v1/projects/{project}/secrets/export", auth(s.exportSecrets))
	m.HandleFunc("GET /v1/projects/{project}/secrets/{name}", auth(s.getSecret))
	m.HandleFunc("PUT /v1/projects/{project}/secrets/{name}", auth(s.putSecret))
	m.HandleFunc("DELETE /v1/projects/{project}/secrets/{name}", auth(s.deleteSecret))
}

func (s *Server) secretsOff(w http.ResponseWriter, r *http.Request) bool {
	if s.secrets != nil {
		return false
	}
	s.ok(w, r, http.StatusServiceUnavailable, ErrorBody{
		Error: "the secret vault is not enabled on this server", Code: "not_configured"})
	return true
}

type secretList struct {
	Secrets []secrets.View `json:"secrets"`
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.secretsOff(w, r) {
		return
	}
	views, err := s.secrets.List(r.Context(), project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, secretList{Secrets: views})
}

func (s *Server) setSecret(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.secretsOff(w, r) {
		return
	}
	var body secrets.SetRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, secrets.MaxValueBytes+8<<10)).Decode(&body); err != nil {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	view, err := s.secrets.Set(r.Context(), project, p.ID, body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID,
		"secret.set", "project_secret", view.Name, map[string]any{"name": view.Name})
	s.ok(w, r, http.StatusCreated, view)
}

func (s *Server) putSecret(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.secretsOff(w, r) {
		return
	}
	var body struct {
		Value       string `json:"value"`
		Description string `json:"description,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, secrets.MaxValueBytes+8<<10)).Decode(&body); err != nil {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	view, err := s.secrets.Set(r.Context(), project, p.ID, secrets.SetRequest{
		Name: r.PathValue("name"), Value: body.Value, Description: body.Description,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID,
		"secret.set", "project_secret", view.Name, map[string]any{"name": view.Name})
	s.ok(w, r, http.StatusOK, view)
}

func (s *Server) getSecret(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.secretsOff(w, r) {
		return
	}
	opened, err := s.secrets.Open(r.Context(), project, r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID,
		"secret.opened", "project_secret", opened.Name, map[string]any{"name": opened.Name})
	s.ok(w, r, http.StatusOK, opened)
}

type secretExport struct {
	Env   map[string]string `json:"env"`
	Count int               `json:"count"`
}

func (s *Server) exportSecrets(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.secretsOff(w, r) {
		return
	}
	env, err := s.secrets.Export(r.Context(), project)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	names := make([]string, 0, len(env))
	for n := range env {
		names = append(names, n)
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID,
		"secret.exported", "project", string(project.ID),
		map[string]any{"count": len(env), "names": names})
	s.ok(w, r, http.StatusOK, secretExport{Env: env, Count: len(env)})
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.secretsOff(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := s.secrets.Delete(r.Context(), project.ID, name); err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID,
		"secret.deleted", "project_secret", name, map[string]any{"name": name})
	w.WriteHeader(http.StatusNoContent)
}
