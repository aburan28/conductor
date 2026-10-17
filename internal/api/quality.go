package api

import (
	"net/http"
	"os"
	"strings"

	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/quality"
)

// quality.lint is advisory only (docs/QUALITY_LINT.md). Heuristics always run;
// an OpenAI-compatible endpoint may add findings when configured.

func (s *Server) qualityRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("POST /v1/projects/{project}/quality/lint", auth(s.qualityLint))
}

type qualityLintBody struct {
	Summary            string                       `json:"summary"`
	Objective          string                       `json:"objective,omitempty"`
	AcceptanceCriteria []domain.AcceptanceCriterion `json:"acceptance_criteria,omitempty"`
	Scopes             []domain.ScopeRequest        `json:"scopes,omitempty"`
	Labels             []string                     `json:"labels,omitempty"`
	// Task optionally loads a stored task; private tasks are redacted for non-owners.
	Task string `json:"task,omitempty"`
}

func (s *Server) qualityLint(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body qualityLintBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}

	env := quality.Envelope{
		Summary:            body.Summary,
		Objective:          body.Objective,
		AcceptanceCriteria: body.AcceptanceCriteria,
		Scopes:             body.Scopes,
		Labels:             body.Labels,
	}

	if ref := strings.TrimSpace(body.Task); ref != "" {
		task, err := s.store.GetTaskByRef(r.Context(), project.ID, ref)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		owner := task.CreatedBy
		if lease, lerr := s.store.ActiveLeaseForTask(r.Context(), task.ID); lerr == nil && lease.HolderPrincipal != "" {
			owner = lease.HolderPrincipal
		}
		private := task.Visibility == domain.VisibilityPrivate && owner != p.ID
		ress, _ := s.store.ReservationsForTask(r.Context(), task.ID)
		scopes := make([]domain.ScopeRequest, 0, len(ress))
		for _, res := range ress {
			scopes = append(scopes, domain.ScopeRequest{Resource: res.Resource(), Mode: res.Mode})
		}
		if private {
			env = quality.Envelope{
				Visibility:    domain.VisibilityPrivate,
				RedactPrivate: true,
				Scopes:        scopes,
			}
		} else {
			if env.Summary == "" {
				env.Summary = task.Title
			}
			if env.Objective == "" {
				env.Objective = task.Objective
			}
			if len(env.AcceptanceCriteria) == 0 {
				env.AcceptanceCriteria = task.AcceptanceCriteria
			}
			if len(env.Labels) == 0 {
				env.Labels = task.Labels
			}
			env.Visibility = task.Visibility
			if len(env.Scopes) == 0 {
				env.Scopes = scopes
			}
		}
	}

	opts := quality.Options{Enhancer: s.qualityEnhancer()}
	result := quality.Lint(r.Context(), env, opts)
	// Never audit the envelope body — it may contain intent the ledger should not store twice.
	s.ok(w, r, http.StatusOK, result)
}

func (s *Server) qualityEnhancer() quality.Enhancer {
	if s.qualityEnhance != nil {
		return s.qualityEnhance
	}
	base := strings.TrimSpace(os.Getenv("CONDUCTOR_QUALITY_LINT_URL"))
	if base == "" {
		return nil
	}
	return &quality.OpenAIEnhancer{
		BaseURL: base,
		APIKey:  os.Getenv("CONDUCTOR_QUALITY_LINT_KEY"),
		Model:   firstNonEmpty(os.Getenv("CONDUCTOR_QUALITY_LINT_MODEL"), "verifier.cheap"),
	}
}
