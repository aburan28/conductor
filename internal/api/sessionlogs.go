package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/localstate"
)

// sessionLogs streams a live session's harness output over SSE, mirroring taskLogs:
// existing content first, then appends as the sidecar's tee writes them, then a done
// frame once the session is closed.
//
// The log is the wrapped harness's combined output, teed by the sidecar into the
// localstate sessions directory (~/.conductor/sessions/<id>/harness.log) — headless
// wraps always tee; interactive wraps tee when CONDUCTOR_HARNESS_LOG=1 so a terminal
// session is never downgraded to a pipe. Like attempt logs, the file is readable where
// the control plane shares a machine with the wrap (DESIGN.md §28.1); a session wrapped
// elsewhere streams "no log on this host" until it closes.
func (s *Server) sessionLogs(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	session, err := s.sessionFor(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.fail(w, r, errors.New("streaming unsupported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	send := func(payload any) {
		body, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
		flusher.Flush()
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	var offset int64
	waiting := "" // last waiting reason sent; "" once frames are flowing
	setWaiting := func(reason string) {
		if waiting != reason {
			waiting = reason
			send(map[string]any{"type": "waiting", "reason": reason})
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return

		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()

		case <-ticker.C:
			fresh, err := s.store.GetSession(r.Context(), session.ID)
			if err == nil {
				session = fresh
			}
			closed := session.ClosedAt != nil || session.State == domain.SessionClosed

			path, err := sessionHarnessLogPath(session.ID)
			if err != nil {
				setWaiting("no log on this host")
				if closed {
					send(map[string]any{"type": "done", "state": "closed"})
					return
				}
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				setWaiting("no harness log yet")
				if closed {
					send(map[string]any{"type": "done", "state": "closed"})
					return
				}
				continue
			}
			waiting = ""
			if info.Size() < offset {
				// Truncated or rotated: start over rather than miss the new run.
				offset = 0
			}
			if info.Size() > offset {
				if offset == 0 && info.Size() > logTailBytes {
					offset = info.Size() - logTailBytes
					send(map[string]any{"type": "log", "text": "… earlier output truncated …\n"})
				}
				remaining := info.Size() - offset
				if remaining > logReadMax {
					remaining = logReadMax
				}
				buf := make([]byte, remaining)
				n, _ := sessionLogReadAt(path, buf, offset)
				// Only whole lines are sent: a chunk ending mid-rune would be corrupted
				// by the JSON encoding.
				if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
					send(map[string]any{"type": "log", "text": string(buf[:i+1])})
					offset += int64(i) + 1
				}
			}
			if closed {
				send(map[string]any{"type": "done", "state": "closed"})
				return
			}
		}
	}
}

// sessionFor resolves the {id} path value at the Observer floor — the same read the
// snapshot export performs, since a log is that session's output, not a way around its
// visibility switches. A non-UUID id is a lookup miss, not a server fault.
func (s *Server) sessionFor(r *http.Request, p domain.Principal) (domain.Session, error) {
	session, err := s.store.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		if isBadUUID(err) {
			err = domain.ErrNotFound
		}
		return domain.Session{}, err
	}
	if _, err := s.svc.Authorize(r.Context(), p, session.ProjectID, domain.RoleObserver); err != nil {
		return domain.Session{}, err
	}
	return session, nil
}

// sessionHarnessLogPath is where the wrap sidecar tees the harness output, keyed by
// session id so the dashboard can derive it without the session carrying a path.
func sessionHarnessLogPath(id string) (string, error) {
	dir, err := localstate.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id, "harness.log"), nil
}

// sessionLogReadAt reads from the sidecar's tee file; the file can be rotated under
// us, so a failed read simply yields no bytes for this poll.
func sessionLogReadAt(path string, buf []byte, offset int64) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.ReadAt(buf, offset)
}
