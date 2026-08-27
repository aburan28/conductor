package api

import (
	"net/http"

	"github.com/adamburan/conductor/internal/domain"
)

// Read-only endpoints the dashboard uses to explain what the ledger already knows: which
// machines can execute, which models the catalog resolves, what a task's checks reported,
// and why it routed the way it did. Nothing here carries prompt or transcript text; every
// row is coordination metadata that the corresponding CLI command already prints.
func (s *Server) inspectRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("GET /v1/projects/{project}/runners", auth(s.listRunners))
	m.HandleFunc("GET /v1/projects/{project}/models", auth(s.listModels))
	m.HandleFunc("GET /v1/tasks/{task}/validation", auth(s.taskValidation))
	m.HandleFunc("GET /v1/tasks/{task}/decisions", auth(s.taskDecisions))
	m.HandleFunc("GET /v1/tasks/{task}/route/explain", auth(s.routeExplain))
	m.HandleFunc("POST /v1/tasks/{task}/route/explain", auth(s.routeExplain))
}

// routeExplain previews what the dispatch policy and tier router would decide for a task,
// without dispatching anything. It is the API behind `conductor route <ref>` and the task
// detail page's "route preview".
func (s *Server) routeExplain(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, _, err := s.taskFor(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	role := domain.RoleImplementer
	if v := r.URL.Query().Get("role"); v != "" {
		role = domain.AgentRole(v)
	}
	full, err := s.store.GetTask(r.Context(), task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	result, err := s.svc.ResolveRoute(r.Context(), task.ProjectID, full, role, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{
		"task_ref": task.Ref, "decision": result.Decision,
		"dispatch": result.Dispatch, "features": result.Features,
	})
}

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	runners, err := s.store.ListRunners(r.Context(), project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"runners": runners})
}

// listModels returns the organization's model catalog. Model names here are policy, not
// anyone's session identity, so publishModelIdentity does not apply.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	profiles, err := s.store.ListModelProfiles(r.Context(), project.OrganizationID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if profiles == nil {
		profiles = []domain.ModelProfile{}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"profiles": profiles})
}

func (s *Server) taskValidation(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, _, err := s.taskFor(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	results, err := s.store.ListValidation(r.Context(), task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if results == nil {
		results = []domain.ValidationResult{}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) taskDecisions(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, _, err := s.taskFor(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	decisions, err := s.store.ListDecisions(r.Context(), task.ID, intParam(r, "limit", 50))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if decisions == nil {
		decisions = []domain.PolicyDecision{}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"decisions": decisions})
}
