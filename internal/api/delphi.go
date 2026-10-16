package api

import (
	"encoding/json"
	"net/http"

	"github.com/aburan28/conductor/internal/delphi/hosted"
	"github.com/aburan28/conductor/internal/domain"
)

// Delphi: delegate prompts to frontier models through provider batch APIs (docs/DELPHI.md).
// Request bodies pass through to the provider and answers are fetched from it on request;
// neither is stored (DESIGN.md §12.4), so these handlers never log or audit their content.

func (s *Server) delphiRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("POST /v1/projects/{project}/delphi", auth(s.submitDelphi))
	m.HandleFunc("GET /v1/projects/{project}/delphi", auth(s.listDelphi))
	m.HandleFunc("GET /v1/projects/{project}/delphi/{ref}", auth(s.getDelphi))
	m.HandleFunc("GET /v1/projects/{project}/delphi/{ref}/answer", auth(s.answerDelphi))
	m.HandleFunc("POST /v1/projects/{project}/delphi/{ref}/cancel", auth(s.cancelDelphi))
	m.HandleFunc("POST /v1/projects/{project}/delphi/{ref}/reconcile", auth(s.reconcileDelphi))
}

func (s *Server) delphiOff(w http.ResponseWriter, r *http.Request) bool {
	if s.delphi != nil {
		return false
	}
	s.ok(w, r, http.StatusServiceUnavailable, ErrorBody{
		Error: "delphi is not enabled on this server", Code: "not_configured"})
	return true
}

// maxDelphiBody bounds one request: long documents fit, a runaway upload does not.
const maxDelphiBody = 16 << 20

func (s *Server) submitDelphi(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.delphiOff(w, r) {
		return
	}
	var body hosted.SubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDelphiBody)).Decode(&body); err != nil {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	d, created, err := s.delphi.Submit(r.Context(), project, p.ID, body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	switch {
	case created && d.State == domain.DelphiSubmitting:
		status = http.StatusAccepted // provider outcome unknown; see docs/DELPHI.md
	case created:
		status = http.StatusCreated
		s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID, "delphi.submit", "delphi_delegation", d.ID,
			map[string]any{"ref": d.Ref, "provider": d.Provider, "model": d.Model})
	}
	s.ok(w, r, status, d)
}

type delphiList struct {
	Delegations []domain.DelphiDelegation `json:"delegations"`
}

func (s *Server) listDelphi(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.delphiOff(w, r) {
		return
	}
	ds, err := s.store.ListDelphiDelegations(r.Context(), project.ID, r.URL.Query().Get("state"), intParam(r, "limit", 100))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, delphiList{Delegations: ds})
}

func (s *Server) getDelphi(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.delphiOff(w, r) {
		return
	}
	d, err := s.store.GetDelphiDelegation(r.Context(), project.ID, r.PathValue("ref"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, d)
}

// answerDelphi fetches the answer from the provider. Contributors only: an answer can carry
// anything the request asked about.
func (s *Server) answerDelphi(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.delphiOff(w, r) {
		return
	}
	a, err := s.delphi.Answer(r.Context(), project, r.PathValue("ref"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, a)
}

func (s *Server) cancelDelphi(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.delphiOff(w, r) {
		return
	}
	d, err := s.delphi.Cancel(r.Context(), project, r.PathValue("ref"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID, "delphi.cancel", "delphi_delegation", d.ID,
		map[string]any{"ref": d.Ref})
	s.ok(w, r, http.StatusAccepted, d)
}

type delphiReconcileBody struct {
	BatchID string `json:"batch_id,omitempty"`
	Abandon bool   `json:"abandon,omitempty"`
}

// reconcileDelphi resolves a submit whose outcome was lost. Maintainers only: adopting the
// wrong batch would attach someone else's answer to this delegation.
func (s *Server) reconcileDelphi(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.delphiOff(w, r) {
		return
	}
	var body delphiReconcileBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	d, err := s.delphi.Reconcile(r.Context(), project, r.PathValue("ref"), body.BatchID, body.Abandon)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID, "delphi.reconcile", "delphi_delegation", d.ID,
		map[string]any{"ref": d.Ref, "state": d.State})
	s.ok(w, r, http.StatusOK, d)
}
