package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/domain"
)

// Per-session snapshots: the dashboard's Save and Export tabs. Both gather the same
// server-side record; save persists it under the project's .conductor/snapshots (the same
// per-repository state the CLI keeps) and export hands the identical JSON to the browser.

// savedSession wraps the snapshot in the envelope both endpoints write, so a file found on
// disk a month later says which moment and which principal it came from.
type savedSession struct {
	coord.SessionSnapshot
	SavedAt time.Time `json:"saved_at"`
	SavedBy string    `json:"saved_by"`
}

// sessionSnapshotFor resolves the session by id and gathers its snapshot for the caller.
// Reading is the Observer floor, the same one the sessions export reads at: a snapshot is
// that export for one session, not a way around its switches. A non-UUID id is a lookup
// miss, not a server fault — the same convention taskFor applies.
func (s *Server) sessionSnapshotFor(r *http.Request, p domain.Principal) (domain.Session, coord.SessionSnapshot, error) {
	session, err := s.store.GetSession(r.Context(), r.PathValue("session"))
	if err != nil {
		if isBadUUID(err) {
			err = domain.ErrNotFound
		}
		return domain.Session{}, coord.SessionSnapshot{}, err
	}
	caller, err := s.svc.Authorize(r.Context(), p, session.ProjectID, domain.RoleObserver)
	if err != nil {
		return domain.Session{}, coord.SessionSnapshot{}, err
	}
	snapshot, err := s.svc.SessionSnapshot(r.Context(), caller, session.ID)
	return session, snapshot, err
}

// saveSession writes a snapshot to <repo>/.conductor/snapshots/session-<id>-<time>.json
// and returns the path. The write needs the project's registered repository path, so it
// exists where the control plane and the repository share a machine (DESIGN.md §28.1).
func (s *Server) saveSession(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	session, snap, err := s.sessionSnapshotFor(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.store.GetProject(r.Context(), session.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if project.RepoPath == "" {
		s.fail(w, r, fmt.Errorf("%w: project has no repository path registered", domain.ErrInvalidArgument))
		return
	}
	body, err := json.MarshalIndent(savedSession{
		SessionSnapshot: snap, SavedAt: time.Now().UTC(), SavedBy: p.Handle,
	}, "", "  ")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	dir := filepath.Join(project.RepoPath, ".conductor", "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.fail(w, r, err)
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("session-%s-%d.json", session.ID, time.Now().Unix()))
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusCreated, map[string]any{"path": path})
}

// exportSession downloads the same JSON the save endpoint would have written, without
// persisting anything server-side. The token may ride the query string (GET only, like
// every SSE endpoint) because a browser download cannot set headers.
func (s *Server) exportSession(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	_, snap, err := s.sessionSnapshotFor(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	body, err := json.MarshalIndent(savedSession{
		SessionSnapshot: snap, SavedAt: time.Now().UTC(), SavedBy: p.Handle,
	}, "", "  ")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=session-%s.json", snap.Session.ID))
	_, _ = w.Write(append(body, '\n'))
}
