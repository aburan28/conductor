package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/metrics"
)

// The operational surface (DESIGN.md §26): request IDs, access logs, panic recovery, request
// body deadlines, Prometheus metrics, readiness, and the server-lifetime context background
// work runs under. It wraps the mux from outside, so handlers need nothing from it beyond
// requestID for their error bodies.

// OpsOptions configures the operational surface. The zero value is usable.
type OpsOptions struct {
	// BaseContext ends when the server starts shutting down. Event streams end with it, so
	// open dashboards do not hold http.Server.Shutdown until its deadline, and background
	// work a request starts (a webhook's pull request check) is cancelled by it rather than
	// outliving the store. Nil means context.Background().
	BaseContext context.Context
	// MetricsToken, when set, is the bearer token /metrics requires. Unset, /metrics answers
	// only direct loopback connections, and never when BehindProxy (every request would then
	// arrive from the proxy's address).
	MetricsToken string
	// BodyTimeout bounds how long a client may take to send a request body. 0 means 30s;
	// negative disables. Requests without a body (every GET, including the event stream)
	// are not affected.
	BodyTimeout time.Duration
	// MaxStreams and MaxStreamsPerPrincipal cap concurrent event-stream connections; 0 means
	// 1000 and 16.
	MaxStreams             int
	MaxStreamsPerPrincipal int
	// StreamPoll is how often each streamed project's events are read; 0 means one second.
	StreamPoll time.Duration
	// MaxWebhookChecks caps pull request checks running in the background on behalf of
	// webhook deliveries; 0 means 8. A delivery past the cap is acknowledged and left to the
	// poller.
	MaxWebhookChecks int
	// ReadyStaleAfter is how old the shared scheduler heartbeat may be before /v1/ready
	// reports degraded; 0 means two minutes.
	ReadyStaleAfter time.Duration
}

// opsState is the Server's operational state.
type opsState struct {
	opts    OpsOptions
	baseCtx context.Context
	// bg tracks goroutines a request started and did not wait for.
	bg         sync.WaitGroup
	webhookSem chan struct{}
	hub        *eventHub
}

func newOpsState(store *db.Store, s *Server, opts OpsOptions) *opsState {
	if opts.BaseContext == nil {
		opts.BaseContext = context.Background()
	}
	if opts.BodyTimeout == 0 {
		opts.BodyTimeout = 30 * time.Second
	}
	if opts.MaxStreams <= 0 {
		opts.MaxStreams = 1000
	}
	if opts.MaxStreamsPerPrincipal <= 0 {
		opts.MaxStreamsPerPrincipal = 16
	}
	if opts.StreamPoll <= 0 {
		opts.StreamPoll = time.Second
	}
	if opts.MaxWebhookChecks <= 0 {
		opts.MaxWebhookChecks = 8
	}
	if opts.ReadyStaleAfter <= 0 {
		opts.ReadyStaleAfter = 2 * time.Minute
	}
	return &opsState{
		opts:       opts,
		baseCtx:    opts.BaseContext,
		webhookSem: make(chan struct{}, opts.MaxWebhookChecks),
		hub: newEventHub(store, s.logger, opts.BaseContext, opts.StreamPoll,
			opts.MaxStreams, opts.MaxStreamsPerPrincipal),
	}
}

// Wait blocks until background work started by requests has finished. conductord calls it
// after http.Server.Shutdown and before closing the store; cancelling BaseContext is what
// makes that work finish promptly.
func (s *Server) Wait() { s.ops.bg.Wait() }

// goBackground runs fn on a goroutine Wait accounts for, under the server-lifetime context,
// if fewer than MaxWebhookChecks are running. It reports whether fn was started.
func (s *Server) goBackground(fn func(ctx context.Context)) bool {
	select {
	case s.ops.webhookSem <- struct{}{}:
	default:
		return false
	}
	s.ops.bg.Add(1)
	go func() {
		defer s.ops.bg.Done()
		defer func() { <-s.ops.webhookSem }()
		fn(s.ops.baseCtx)
	}()
	return true
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	httpRequests = metrics.Default.NewCounter("conductor_http_requests_total",
		"HTTP requests served, by route pattern, method and status.", "route", "method", "status")
	httpDuration = metrics.Default.NewHistogram("conductor_http_request_duration_seconds",
		"HTTP request duration by route pattern (event streams included: they last as long as the connection).",
		metrics.DurationBuckets, "route")
	httpInFlight = metrics.Default.NewGauge("conductor_http_requests_in_flight",
		"HTTP requests being served.")
	httpPanics = metrics.Default.NewCounter("conductor_http_panics_total",
		"Handler panics recovered.")
	webhookDeferred = metrics.Default.NewCounter("conductor_github_webhooks_deferred_total",
		"Webhook deliveries acknowledged without an immediate check because the check pool was full.")
)

// RegisterPoolMetrics exposes the database pool's statistics at /metrics. Call it once per
// process.
func RegisterPoolMetrics(store *db.Store) {
	stat := func(f func(s *pgxpool.Stat) float64) func() float64 {
		return func() float64 { return f(store.Pool().Stat()) }
	}
	r := metrics.Default
	r.NewGaugeFunc("conductor_db_pool_acquired_conns", "Connections in use.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.AcquiredConns()) }))
	r.NewGaugeFunc("conductor_db_pool_idle_conns", "Idle connections.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.IdleConns()) }))
	r.NewGaugeFunc("conductor_db_pool_total_conns", "Open connections.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.TotalConns()) }))
	r.NewGaugeFunc("conductor_db_pool_max_conns", "Pool size limit.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.MaxConns()) }))
	r.NewCounterFunc("conductor_db_pool_acquires_total", "Connections acquired from the pool.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.AcquireCount()) }))
	r.NewCounterFunc("conductor_db_pool_empty_acquires_total", "Acquires that had to wait for a connection.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.EmptyAcquireCount()) }))
	r.NewCounterFunc("conductor_db_pool_acquire_wait_seconds_total", "Total time spent waiting to acquire.",
		stat(func(s *pgxpool.Stat) float64 { return s.AcquireDuration().Seconds() }))
}

// serveMetrics renders the registry. Metrics name routes, counts and timings, nothing a
// tenant owns, but they still describe the deployment, so they are not public.
func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if tok := s.ops.opts.MetricsToken; tok != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "metrics require the token given to --metrics-token", http.StatusUnauthorized)
			return
		}
	} else if s.behindProxy || !isLoopbackRemote(r.RemoteAddr) {
		http.Error(w, "metrics are served only to loopback connections; set --metrics-token to scrape from elsewhere",
			http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", metrics.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	if err := metrics.Default.WriteText(w); err != nil {
		s.logger.Debug("write metrics failed", "error", err)
	}
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// Request IDs and access logs
// ---------------------------------------------------------------------------

type requestInfoKey struct{}

// requestInfo is shared, by pointer, between the observe middleware and the handler, so what
// the handler learns (who the caller is) reaches the access log written after it returns.
type requestInfo struct {
	id        string
	principal domain.ID
}

// requestID returns the request's ID: the caller's X-Request-Id when it sent a usable one,
// otherwise one generated here. Every response carries it in X-Request-Id; error bodies and
// log lines should carry it too, so a user's report can be matched to the server's log.
func requestID(r *http.Request) string {
	if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok {
		return info.id
	}
	return ""
}

// notePrincipal records the authenticated caller for the access log.
func notePrincipal(r *http.Request, id domain.ID) {
	if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok {
		info.principal = id
	}
}

// A propagated request ID is logged and echoed, so it is limited to characters that cannot
// forge a log field or a header.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._:/+=-]{1,128}$`)

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// quietRoutes are polled by probes and scrapers; logging each at Info would drown the rest.
var quietRoutes = map[string]bool{"GET /v1/health": true, "GET /v1/ready": true, "GET /metrics": true}

// observe is the outermost middleware: request ID, body deadline, panic recovery, access
// log and HTTP metrics.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{id: r.Header.Get("X-Request-Id")}
		if !validRequestID.MatchString(info.id) {
			info.id = newRequestID()
		}
		w.Header().Set("X-Request-Id", info.id)
		r = r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info))
		s.limitBody(w, r)

		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		httpInFlight.Add(1)
		defer func() {
			httpInFlight.Add(-1)
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p) // the handler asked for the connection to be dropped
				}
				httpPanics.Inc()
				s.logger.Error("panic recovered", "request_id", info.id,
					"method", r.Method, "path", r.URL.Path, "panic", p, "stack", string(debug.Stack()))
				if rec.wrote {
					// Headers are out; all that is left is to end the response.
					rec.status = http.StatusInternalServerError
				} else {
					s.ok(rec, r, http.StatusInternalServerError, map[string]string{
						"error": "internal error", "code": "panic", "request_id": info.id,
					})
				}
			}

			// r.Pattern is set by the mux on this same request. Unmatched paths share one
			// label, so scanners probing random URLs cannot mint metric series.
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			elapsed := time.Since(start)
			status := rec.status
			httpRequests.Inc(route, r.Method, statusText(status))
			httpDuration.Observe(elapsed.Seconds(), route)

			// Identifiers and outcomes only: no query string (it can carry a token), no
			// headers, no bodies — bodies hold task titles, which are project-visibility data
			// (DESIGN.md §26.3).
			level := slog.LevelInfo
			if quietRoutes[route] && status < 500 {
				level = slog.LevelDebug
			}
			s.logger.Log(r.Context(), level, "request",
				"request_id", info.id, "method", r.Method, "route", route, "path", r.URL.Path,
				"status", status, "duration_ms", elapsed.Milliseconds(), "bytes", rec.bytes,
				"principal", string(info.principal))
		}()
		next.ServeHTTP(rec, r)
	})
}

func statusText(code int) string {
	const digits = "0123456789"
	if code < 100 || code > 999 {
		return "000"
	}
	return string([]byte{digits[code/100], digits[code/10%10], digits[code%10]})
}

// responseRecorder captures the status and size for the access log. It forwards Flush, for
// event streams, and Unwrap, so http.ResponseController (read deadlines) reaches the real
// connection through it.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (w *responseRecorder) WriteHeader(status int) {
	if !w.wrote {
		w.status, w.wrote = status, true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(b []byte) (int, error) {
	w.wrote = true
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *responseRecorder) Flush() {
	w.wrote = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ---------------------------------------------------------------------------
// Request body deadline
// ---------------------------------------------------------------------------

// limitBody gives a request with a body BodyTimeout to deliver it, so a client trickling a
// body a byte at a time cannot hold a handler and its goroutine indefinitely.
//
// A server-wide ReadTimeout cannot do this here: it stays armed after the request is read,
// and when it fires during a long response (an event stream, a long check) the server
// cancels that request's context. So the deadline is set per request, only when there is a
// body, and cleared as soon as the body has been read, before the handler's own work.
func (s *Server) limitBody(w http.ResponseWriter, r *http.Request) {
	timeout := s.ops.opts.BodyTimeout
	if timeout < 0 || r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return
	}
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return // a transport that cannot (some HTTP/2 paths) keeps the old behaviour
	}
	r.Body = &deadlineBody{ReadCloser: r.Body, rc: rc}
}

type deadlineBody struct {
	io.ReadCloser
	rc      *http.ResponseController
	cleared bool
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) && !b.cleared {
		// The body is in. The server's own background read for client disconnects starts
		// now, and must not inherit the body's deadline. Any other error (the deadline
		// itself, an oversized body) leaves the deadline armed, so the server's attempt to
		// drain the rest of the body before responding fails fast instead of waiting on a
		// client that stopped sending.
		b.cleared = true
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// ---------------------------------------------------------------------------
// Health and readiness
// ---------------------------------------------------------------------------

func (s *Server) opsRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /metrics", s.serveMetrics)
	m.HandleFunc("GET /v1/ready", s.authenticate(s.ready))
}

// ready is the deep check, for an operator or an orchestrator holding a token: the database,
// the schema version against this binary, the shared scheduler heartbeat, and the GitHub
// poller. It answers 503 when this server should not take traffic or the control plane's
// background work has stopped. Failures are named, never quoted: database error text can
// carry hostnames and credentials, and the caller may be any member of any tenant.
func (s *Server) ready(w http.ResponseWriter, r *http.Request, _ domain.Principal) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	healthy := true
	checks := map[string]any{}

	if err := s.store.Pool().Ping(ctx); err != nil {
		s.logger.Error("readiness: database unreachable", "request_id", requestID(r), "error", err)
		checks["database"] = "unreachable"
		s.ok(w, r, http.StatusServiceUnavailable, map[string]any{"status": "degraded", "checks": checks, "request_id": requestID(r)})
		return
	}
	checks["database"] = "ok"

	if schema, err := s.store.SchemaStatus(ctx); err != nil {
		healthy = false
		checks["schema"] = "unreadable"
		s.logger.Error("readiness: schema status", "request_id", requestID(r), "error", err)
	} else {
		checks["schema"] = schema
		if !schema.Current() {
			healthy = false
		}
	}

	sched := map[string]any{"status": "never_ran"}
	if hb, found, err := s.store.GetHeartbeat(ctx, db.ComponentScheduler); err != nil {
		sched["status"] = "unreadable"
		healthy = false
	} else if found {
		age := time.Since(hb.LastRunAt)
		sched["last_tick"] = hb.LastRunAt.UTC()
		sched["age_seconds"] = int(age.Seconds())
		sched["status"] = "ok"
		if age > s.ops.opts.ReadyStaleAfter {
			// No replica has ticked: leases are not being reclaimed, nor budgets checked.
			sched["status"] = "stale"
			healthy = false
		}
	}
	checks["scheduler"] = sched

	poller := map[string]any{"status": "never_ran"}
	if hb, found, err := s.store.GetHeartbeat(ctx, db.ComponentGitHubPoller); err == nil && found {
		poller["last_poll"] = hb.LastRunAt.UTC()
		poller["status"] = "ok"
		if hb.LastError != "" {
			// Whether, not what: the message names the owner's repositories.
			poller["status"] = "error"
		}
	}
	checks["github_poller"] = poller

	status, code := "ok", http.StatusOK
	if !healthy {
		status, code = "degraded", http.StatusServiceUnavailable
	}
	s.ok(w, r, code, map[string]any{"status": status, "checks": checks, "time": time.Now().UTC()})
}

// errTooManyStreams is returned when an event-stream cap is reached.
var errTooManyStreams = errors.New("too many open event streams")
