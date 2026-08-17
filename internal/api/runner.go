package api

import (
	"net/http"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/domain"
)

// Endpoints a remote runner needs, so a machine that executes work never requires database
// credentials (DESIGN.md §28.2: runners connect outbound; a laptop exposes no inbound port
// and holds no database password).

func (s *Server) runnerSnapshot(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	snap, err := s.svc.Snapshot(r.Context(), caller, project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, snap)
}

func (s *Server) attemptBrief(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	attempt, err := s.store.GetAttempt(r.Context(), r.PathValue("attempt"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	caller, err := s.svc.Authorize(r.Context(), p, attempt.ProjectID, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	brief, err := s.svc.Brief(r.Context(), caller, attempt.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, brief)
}

func (s *Server) setAttemptRoute(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	attempt, err := s.store.GetAttempt(r.Context(), r.PathValue("attempt"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	caller, err := s.svc.Authorize(r.Context(), p, attempt.ProjectID, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body coord.RouteRecord
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.SetRoute(r.Context(), caller, attempt.ID, body); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"status": "recorded"})
}

type attemptStateBody struct {
	State        domain.AttemptState `json:"state"`
	Branch       string              `json:"branch"`
	WorktreePath string              `json:"worktree_path"`
	// MarkTaskRunning advances the task alongside the attempt, which the runner does once
	// the harness is actually executing.
	MarkTaskRunning bool `json:"mark_task_running"`
}

func (s *Server) setAttemptState(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	attempt, err := s.store.GetAttempt(r.Context(), r.PathValue("attempt"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	caller, err := s.svc.Authorize(r.Context(), p, attempt.ProjectID, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body attemptStateBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.SetAttemptState(r.Context(), caller, attempt.ID,
		body.State, body.Branch, body.WorktreePath); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.MarkTaskRunning {
		if err := s.svc.MarkTaskRunning(r.Context(), attempt.TaskID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"state": body.State})
}
