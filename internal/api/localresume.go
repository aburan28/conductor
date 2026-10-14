package api

import (
	"net/http"

	"github.com/aburan28/conductor/internal/domain"
)

// The machine-local resume surface. Everything else in this package answers
// "what is the team's state"; this one endpoint answers "wake this laptop's
// sessions back up" — the reboot case. It is a hook rather than a direct call
// so a control plane running somewhere other than the laptop with the terminals
// (a swarm member, a CI box) can simply not offer it.

// localResumeAll is POST /v1/local/resume-all: switch off harness accounts
// marked out of usage, then wake every paused or saved Claude Code, Codex, and
// OpenCode session on this machine, reopening terminals a restart took away.
func (s *Server) localResumeAll(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if s.localResume == nil {
		http.Error(w, "local resume not available on this control plane", http.StatusNotImplemented)
		return
	}
	summary, err := s.localResume()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, summary)
}
