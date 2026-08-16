// Package api is the northbound HTTP interface (DESIGN.md §19).
//
// Two rules hold everywhere in this package:
//
//   - No handler reads project data before resolving the caller and checking membership.
//   - No handler serializes a domain object directly to a non-owner; everything goes through
//     internal/privacy's projections.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// Server holds the HTTP handlers.
type Server struct {
	store  *db.Store
	svc    *coord.Service
	logger *slog.Logger
	mux    *http.ServeMux
	// dashboard is served at / when non-empty.
	dashboard []byte
}

type Options struct {
	Logger    *slog.Logger
	Dashboard []byte
}

func New(store *db.Store, svc *coord.Service, opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{
		store:     store,
		svc:       svc,
		logger:    opts.Logger,
		mux:       http.NewServeMux(),
		dashboard: opts.Dashboard,
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Handler wraps the mux with logging and panic recovery.
func (s *Server) Handler() http.Handler {
	return s.recoverPanic(s.logRequests(s.mux))
}

// ---------------------------------------------------------------------------
// Context and authentication
// ---------------------------------------------------------------------------

type ctxKey int

const principalKey ctxKey = iota

// authenticate resolves the bearer token to a principal.
//
// Tokens are matched by SHA-256 hash, so a database dump contains no usable credential, and
// the plaintext is never logged (DESIGN.md §25.1).
func (s *Server) authenticate(next func(http.ResponseWriter, *http.Request, domain.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		if token == "" || token == header {
			// Also accept the token as a query parameter for SSE, where browsers cannot set
			// headers on EventSource. Restricted to GET so a token never rides on a mutation.
			if r.Method == http.MethodGet {
				token = r.URL.Query().Get("token")
			}
		}
		if token == "" {
			s.fail(w, r, domain.ErrUnauthenticated)
			return
		}
		principal, err := s.store.AuthenticateToken(r.Context(), token)
		if err != nil {
			s.fail(w, r, domain.ErrUnauthenticated)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey, principal)), principal)
	}
}

// project resolves the project from the path and authorizes the caller in one step, so no
// handler can accidentally skip the membership check.
func (s *Server) project(r *http.Request, principal domain.Principal, need domain.Role) (domain.Project, coord.Caller, error) {
	id := r.PathValue("project")
	project, err := s.resolveProject(r.Context(), principal, id)
	if err != nil {
		return domain.Project{}, coord.Caller{}, err
	}
	caller, err := s.svc.Authorize(r.Context(), principal, project.ID, need)
	if err != nil {
		return domain.Project{}, coord.Caller{}, err
	}
	return project, caller, nil
}

// resolveProject accepts either a project id or an org-scoped slug.
func (s *Server) resolveProject(ctx context.Context, principal domain.Principal, ref string) (domain.Project, error) {
	if ref == "" {
		return domain.Project{}, domain.ErrNotFound
	}
	if p, err := s.store.GetProject(ctx, ref); err == nil {
		return p, nil
	} else if !errors.Is(err, domain.ErrNotFound) && !isBadUUID(err) {
		return domain.Project{}, err
	}
	return s.store.GetProjectBySlug(ctx, principal.OrganizationID, ref)
}

// isBadUUID reports whether an error came from casting a non-UUID string, which happens when
// a slug is passed where an id was expected. That is a lookup miss, not a server fault.
func isBadUUID(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid")
}

// taskFor resolves a task by id or by project-scoped ref, then authorizes.
func (s *Server) taskFor(r *http.Request, principal domain.Principal, need domain.Role) (domain.Task, coord.Caller, error) {
	ref := r.PathValue("task")
	task, err := s.store.GetTask(r.Context(), ref)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) && !isBadUUID(err) {
			return domain.Task{}, coord.Caller{}, err
		}
		// Fall back to a ref lookup, which needs a project hint.
		projectRef := r.URL.Query().Get("project")
		if projectRef == "" {
			return domain.Task{}, coord.Caller{}, domain.ErrNotFound
		}
		project, perr := s.resolveProject(r.Context(), principal, projectRef)
		if perr != nil {
			return domain.Task{}, coord.Caller{}, perr
		}
		task, err = s.store.GetTaskByRef(r.Context(), project.ID, ref)
		if err != nil {
			return domain.Task{}, coord.Caller{}, err
		}
	}
	caller, err := s.svc.Authorize(r.Context(), principal, task.ProjectID, need)
	if err != nil {
		return domain.Task{}, coord.Caller{}, err
	}
	return task, caller, nil
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

func (s *Server) ok(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.logger.Warn("write response failed", "path", r.URL.Path, "error", err)
	}
}

// ErrorBody is the uniform error envelope.
type ErrorBody struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Details any    `json:"details,omitempty"`
}

// fail maps a domain error onto an HTTP status exactly once, here, so status decisions cannot
// drift between handlers.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code := http.StatusInternalServerError, "internal"

	switch {
	case errors.Is(err, domain.ErrUnauthenticated):
		status, code = http.StatusUnauthorized, "unauthenticated"
	case errors.Is(err, domain.ErrNotPermitted):
		status, code = http.StatusForbidden, "forbidden"
	case errors.Is(err, domain.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, domain.ErrInvalidEnum), errors.Is(err, domain.ErrInvalidArgument):
		status, code = http.StatusBadRequest, "invalid_argument"
	case errors.Is(err, domain.ErrIllegalTransition):
		status, code = http.StatusConflict, "illegal_transition"
	case errors.Is(err, domain.ErrStaleFencing):
		// 409 rather than 403: the caller was legitimate, its epoch is simply no longer
		// current. The correct client behaviour is to stop, not to re-authenticate.
		status, code = http.StatusConflict, "stale_fencing"
	case errors.Is(err, domain.ErrLeaseNotHeld):
		status, code = http.StatusConflict, "lease_not_held"
	case errors.Is(err, domain.ErrAlreadyClaimed):
		status, code = http.StatusConflict, "already_claimed"
	case errors.Is(err, domain.ErrConflict):
		status, code = http.StatusConflict, "scope_conflict"
	case errors.Is(err, domain.ErrDuplicate):
		status, code = http.StatusConflict, "duplicate"
	case errors.Is(err, domain.ErrDependencyCycle):
		status, code = http.StatusBadRequest, "dependency_cycle"
	case errors.Is(err, domain.ErrBudgetExhausted):
		status, code = http.StatusPaymentRequired, "budget_exhausted"
	case errors.Is(err, domain.ErrCapacity), errors.Is(err, domain.ErrConcurrency):
		status, code = http.StatusServiceUnavailable, "no_capacity"
	}

	if status >= 500 {
		s.logger.Error("request failed", "path", r.URL.Path, "error", err)
	}
	s.ok(w, r, status, ErrorBody{Error: err.Error(), Code: code})
}

// decode reads a JSON body with a size limit, so a malformed or hostile request cannot
// exhaust memory.
func decode[T any](r *http.Request, dst *T) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return errors.Join(domain.ErrInvalidArgument, err)
	}
	// Stash the hash for idempotency keying.
	sum := sha256.Sum256(body)
	r.Header.Set("X-Conductor-Body-Hash", hex.EncodeToString(sum[:]))
	return nil
}

// idempotent short-circuits a repeated mutation carrying the same Idempotency-Key.
func (s *Server) idempotent(w http.ResponseWriter, r *http.Request, principal domain.Principal) bool {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return false
	}
	status, body, found, err := s.store.RecallIdempotent(
		r.Context(), key, principal.ID, r.Header.Get("X-Conductor-Body-Hash"))
	if err != nil {
		s.fail(w, r, err)
		return true
	}
	if !found {
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Idempotent-Replay", "true")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return true
}

func (s *Server) remember(r *http.Request, principal domain.Principal, status int, body any) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return
	}
	_ = s.store.RememberIdempotent(r.Context(), key, principal.ID,
		r.Header.Get("X-Conductor-Body-Hash"), status, encoded)
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Log identifiers and outcomes only. Bodies routinely contain task titles, which are
		// project-visibility data and do not belong in a server log (DESIGN.md §26.3).
		s.logger.Debug("request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond).String())
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic recovered", "path", r.URL.Path, "panic", rec)
				s.ok(w, r, http.StatusInternalServerError,
					ErrorBody{Error: "internal error", Code: "panic"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Flush forwards to the underlying writer so SSE streaming keeps working through the
// recorder.
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func intParam(r *http.Request, name string, fallback int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func fenceFrom(r *http.Request, body fenceCarrier) domain.Fence {
	f := body.fence()
	if f.TaskID == "" {
		f.TaskID = r.PathValue("task")
	}
	if f.AttemptID == "" {
		f.AttemptID = r.PathValue("attempt")
	}
	return f
}

type fenceCarrier interface{ fence() domain.Fence }
