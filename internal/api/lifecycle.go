package api

import (
	"net/http"

	"github.com/adamburan/conductor/internal/domain"
)

// lifecycleRoutes are the session-side halves of the claim loop: a wrapped session adopting
// a claim made before it started, and a session reserving territory under the claim it
// carries. Both act through the caller's own session, never by naming a lease.
func (s *Server) lifecycleRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("POST /v1/sessions/{session}/adopt", auth(s.adoptClaims))
	m.HandleFunc("POST /v1/sessions/{session}/scopes", auth(s.sessionScopes))
	m.HandleFunc("POST /v1/attempts/{attempt}/evidence", auth(s.publishEvidence))
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
