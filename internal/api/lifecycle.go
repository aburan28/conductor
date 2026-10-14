package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/aburan28/conductor/internal/domain"
)

// lifecycleRoutes are the session-side halves of the claim loop: a wrapped session adopting
// a claim made before it started, and a session reserving territory under the claim it
// carries. Both act through the caller's own session, never by naming a lease.
func (s *Server) lifecycleRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("POST /v1/sessions/{session}/adopt", auth(s.adoptClaims))
	m.HandleFunc("POST /v1/sessions/{session}/scopes", auth(s.sessionScopes))
	m.HandleFunc("POST /v1/attempts/{attempt}/evidence", auth(s.publishEvidence))
	m.HandleFunc("POST /v1/tasks/{task}/complete", auth(s.completeTask))
}

// completeTask finishes work that is still claimed or running in one step: the person
// working it merged it (without the GitHub App to say so) and is saying so. The ordinary
// transition route cannot express that — running -> done skips edges, and the live lease and
// attempt have to end with it — so this walks the same path a merge does.
//
// Who may do this is the transition route's rule for done (coord.AuthorizeTransition): a
// reviewer or a maintainer, not the person who did the work alone. A task waiting to merge
// (verifying and on) has no lease and goes through the transition route itself.
func (s *Server) completeTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	lease, err := s.store.ActiveLeaseForTask(r.Context(), task.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.fail(w, r, fmt.Errorf("%w: %s holds no live claim; mark finished work done with the transition route",
				domain.ErrIllegalTransition, task.Ref))
			return
		}
		s.fail(w, r, err)
		return
	}
	// Completing is a move to done, so it answers to the same rule as the transition route:
	// accepting work takes a reviewer or a maintainer, never only the person who did it. A
	// merged pull request completes work through the control plane instead (github_merge.go).
	others, err := s.svc.AuthorizeTransition(r.Context(), caller, task, domain.TaskDone)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.store.CompleteWork(r.Context(), task.ID, "marked done by "+p.Handle)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if others || lease.HolderPrincipal != p.ID {
		s.auditOthersWork(r, caller, task, "task.completed_by_other", map[string]any{
			"from": string(out.From), "holder": string(lease.HolderPrincipal)})
	}
	view, err := s.svc.TaskView(r.Context(), caller, task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"task": view, "from": out.From})
}

type evidenceBody struct {
	fenceBody
	CommitSHA    string                    `json:"commit_sha"`
	BaseSHA      string                    `json:"base_sha"`
	ChangedPaths []string                  `json:"changed_paths"`
	Commands     []domain.ValidationResult `json:"commands"`
}

// publishEvidence records evidence against a live attempt without finishing it: the commit,
// the paths it touched, and each validation command with its exit code. It is what
// coord_publish_result sends, so an interactive session's results reach the task the same
// way a runner's do at finish.
func (s *Server) publishEvidence(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body evidenceBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	fence := fenceFrom(r, body)
	attempt, err := s.store.GetAttempt(r.Context(), fence.AttemptID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fence.TaskID = attempt.TaskID
	caller, err := s.svc.Authorize(r.Context(), p, attempt.ProjectID, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.PublishEvidence(r.Context(), caller, fence, domain.EvidenceManifest{
		CommitSHA: body.CommitSHA, BaseSHA: body.BaseSHA,
		ChangedPaths: body.ChangedPaths, Commands: body.Commands,
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusAccepted, map[string]any{"status": "recorded", "commands": len(body.Commands)})
}

func (s *Server) adoptClaims(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body struct {
		WorktreePath string `json:"worktree_path"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	adopted, err := s.svc.AdoptClaims(r.Context(), p, r.PathValue("session"), body.WorktreePath)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"adopted": adopted})
}

func (s *Server) sessionScopes(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body struct {
		Scopes []domain.ScopeRequest    `json:"scopes"`
		Source domain.ReservationSource `json:"source"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	result, err := s.svc.ReserveForSession(r.Context(), p, r.PathValue("session"), body.Scopes, body.Source)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if result.Outcome.Blocks() {
		status = http.StatusConflict
	}
	s.ok(w, r, status, result)
}
