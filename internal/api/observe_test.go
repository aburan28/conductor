package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/db"
)

// syncBuffer is a log sink safe for the server's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// opsServer is a server over the harness's store with its own options and log.
func (h *harness) opsServer(t *testing.T, opts Options) (*Server, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	opts.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return New(h.store, coord.New(h.store), opts), logs
}

func serve(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestRequestIDAndAccessLog(t *testing.T) {
	h := newHarness(t)
	srv, logs := h.opsServer(t, Options{})
	ts := serve(t, srv)

	// A caller's request ID is propagated; the access log names the route pattern and the
	// principal, at Info.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+h.projectPath(""), nil)
	req.Header.Set("Authorization", "Bearer "+h.aliceTok)
	req.Header.Set("X-Request-Id", "trace-abc.123")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != "trace-abc.123" {
		t.Errorf("X-Request-Id = %q", got)
	}
	log := logs.String()
	for _, want := range []string{"level=INFO", "msg=request", "request_id=trace-abc.123",
		`route="GET /v1/projects/{project}"`, "status=200", "principal=" + string(h.alice.ID)} {
		if !strings.Contains(log, want) {
			t.Errorf("access log lacks %s:\n%s", want, log)
		}
	}
	if strings.Contains(log, h.aliceTok) {
		t.Error("the access log contains a bearer token")
	}

	// A malformed or hostile ID is replaced, never echoed.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/v1/health", nil)
	req.Header.Set("X-Request-Id", "evil\" level=ERROR")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got == "" || strings.Contains(got, "evil") {
		t.Errorf("X-Request-Id = %q, want a generated one", got)
	}
}

func TestPanicIsLoggedWithStack(t *testing.T) {
	h := newHarness(t)
	srv, logs := h.opsServer(t, Options{})
	srv.mux.HandleFunc("GET /v1/test/panic", func(http.ResponseWriter, *http.Request) { panic("boom") })
	ts := serve(t, srv)

	resp, err := ts.Client().Get(ts.URL + "/v1/test/panic")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusInternalServerError || body["request_id"] == "" || body["request_id"] != resp.Header.Get("X-Request-Id") {
		t.Errorf("panic response = %d %v", resp.StatusCode, body)
	}
	log := logs.String()
	if !strings.Contains(log, "panic recovered") || !strings.Contains(log, "goroutine ") || !strings.Contains(log, "request_id="+body["request_id"]) {
		t.Errorf("panic log has no stack or request id:\n%s", log)
	}
}

func TestMetricsEndpointIsProtected(t *testing.T) {
	h := newHarness(t)
	srv, _ := h.opsServer(t, Options{})
	ts := serve(t, srv)
	h.doOn(ts, h.aliceTok, http.MethodGet, h.projectPath(""))

	// Loopback, no token configured: served.
	resp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("metrics = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{
		`conductor_http_requests_total{route="GET /v1/projects/{project}",method="GET",status="200"}`,
		"# TYPE conductor_http_request_duration_seconds histogram",
		"# TYPE conductor_events_written_total counter",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	// Behind a proxy every request looks local, so loopback proves nothing: refused.
	proxied := serve(t, New(h.store, coord.New(h.store), Options{BehindProxy: true}))
	if code, _ := h.doOn(proxied, "", http.MethodGet, "/metrics"); code != http.StatusForbidden {
		t.Errorf("metrics behind a proxy without a token = %d, want 403", code)
	}

	// With a token, the token is required.
	tokened := serve(t, New(h.store, coord.New(h.store), Options{Ops: OpsOptions{MetricsToken: "scrape-secret"}}))
	if code, _ := h.doOn(tokened, "", http.MethodGet, "/metrics"); code != http.StatusUnauthorized {
		t.Errorf("metrics without the token = %d, want 401", code)
	}
	if code, _ := h.doOn(tokened, h.aliceTok, http.MethodGet, "/metrics"); code != http.StatusUnauthorized {
		t.Errorf("metrics with a user token = %d, want 401", code)
	}
	if code, _ := h.doOn(tokened, "scrape-secret", http.MethodGet, "/metrics"); code != http.StatusOK {
		t.Errorf("metrics with the token = %d, want 200", code)
	}
}

func TestHealthDoesNotLeakDatabaseErrors(t *testing.T) {
	// A pool that cannot connect: pgxpool connects lazily, so construction succeeds.
	pool, err := pgxpool.New(context.Background(), "postgres://secretuser:hunter2@127.0.0.1:1/nodb?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := db.NewWithPool(pool)
	logs := &syncBuffer{}
	srv := New(store, coord.New(store), Options{Logger: slog.New(slog.NewTextHandler(logs, nil))})
	ts := serve(t, srv)
	resp, err := ts.Client().Get(ts.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("health with no database = %d", resp.StatusCode)
	}
	for _, leak := range []string{"secretuser", "127.0.0.1", "connect", "dial"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("health body leaks %q: %s", leak, body)
		}
	}
	if !strings.Contains(string(body), `"unreachable"`) || !strings.Contains(logs.String(), "health: database unreachable") {
		t.Errorf("health body %s / log %s", body, logs.String())
	}
}

func TestReadyReportsSchemaAndRequiresAuth(t *testing.T) {
	h := newHarness(t)
	ts := serve(t, New(h.store, coord.New(h.store), Options{}))
	if code, _ := h.doOn(ts, "", http.MethodGet, "/v1/ready"); code != http.StatusUnauthorized {
		t.Errorf("ready without a token = %d, want 401", code)
	}
	code, body := h.doJSONOn(ts, h.aliceTok, http.MethodGet, "/v1/ready", nil)
	checks, _ := body["checks"].(map[string]any)
	schema, _ := checks["schema"].(map[string]any)
	if checks["database"] != "ok" || schema["known"] != db.KnownMigrations()[len(db.KnownMigrations())-1] {
		t.Errorf("ready = %d %v", code, body)
	}
	if _, ok := checks["scheduler"]; !ok {
		t.Errorf("ready reports no scheduler: %v", body)
	}
}

// A request body that trickles in is cut off at the body deadline instead of holding the
// handler forever, while a long event stream is untouched by it.
func TestBodyDeadline(t *testing.T) {
	h := newHarness(t)
	ts := serve(t, New(h.store, coord.New(h.store), Options{Ops: OpsOptions{BodyTimeout: 300 * time.Millisecond}}))

	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "POST "+h.projectPath("/tasks")+" HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+h.aliceTok+
		"\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"title\":")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	_, err = bufio.NewReader(conn).ReadString('\n') // a response line, or EOF when the server gives up
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("a trickled body held the request for %s (read: %v)", elapsed, err)
	}

	// The stream outlives the body deadline many times over and still delivers.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+h.projectPath("/events/stream"), nil)
	req.Header.Set("Authorization", "Bearer "+h.aliceTok)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	time.Sleep(time.Second)
	h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"), map[string]any{"summary": "after the deadline"})
	if !streamSees(t, resp.Body, "event: task.claimed", 8*time.Second) {
		t.Error("the event stream stopped delivering after the body deadline")
	}
}

func streamSees(t *testing.T, body io.Reader, needle string, within time.Duration) bool {
	t.Helper()
	found := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if strings.Contains(sc.Text(), needle) {
				found <- true
				return
			}
		}
		found <- false
	}()
	select {
	case ok := <-found:
		return ok
	case <-time.After(within):
		return false
	}
}

func TestStreamCapsAndSharedFeed(t *testing.T) {
	h := newHarness(t)
	srv := New(h.store, coord.New(h.store), Options{Ops: OpsOptions{MaxStreamsPerPrincipal: 2, StreamPoll: 50 * time.Millisecond}})
	ts := serve(t, srv)
	open := func(tok string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+h.projectPath("/events/stream"), nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	a1, a2 := open(h.aliceTok), open(h.aliceTok)
	defer a2.Body.Close()
	b1 := open(h.bobTok)
	defer b1.Body.Close()
	if a1.StatusCode != http.StatusOK || a2.StatusCode != http.StatusOK || b1.StatusCode != http.StatusOK {
		t.Fatalf("streams = %d %d %d", a1.StatusCode, a2.StatusCode, b1.StatusCode)
	}
	a3 := open(h.aliceTok)
	a3.Body.Close()
	if a3.StatusCode != http.StatusServiceUnavailable || a3.Header.Get("Retry-After") == "" {
		t.Errorf("a stream past the per-principal cap = %d", a3.StatusCode)
	}

	// Three streams, one project: one feed polls for all of them.
	srv.ops.hub.mu.Lock()
	feeds, total := len(srv.ops.hub.feeds), srv.ops.hub.total
	srv.ops.hub.mu.Unlock()
	if feeds != 1 || total != 3 {
		t.Errorf("feeds=%d streams=%d, want one feed for three streams", feeds, total)
	}

	// Both of alice's tabs and bob's see the same live event.
	h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"), map[string]any{"summary": "fan out"})
	for i, r := range []*http.Response{a1, a2, b1} {
		if !streamSees(t, r.Body, "event: task.claimed", 5*time.Second) {
			t.Errorf("stream %d missed the event", i)
		}
	}

	// Closing a stream frees its slot.
	a1.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a4 := open(h.aliceTok)
		a4.Body.Close()
		if a4.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a closed stream's slot was never released")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Shutting the server down ends open streams, so http.Server.Shutdown is not held open by
// dashboards until its deadline.
func TestShutdownEndsStreams(t *testing.T) {
	h := newHarness(t)
	life, stop := context.WithCancel(context.Background())
	defer stop()
	ts := serve(t, New(h.store, coord.New(h.store), Options{Ops: OpsOptions{BaseContext: life}}))
	req, _ := http.NewRequest(http.MethodGet, ts.URL+h.projectPath("/events/stream"), nil)
	req.Header.Set("Authorization", "Bearer "+h.aliceTok)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived the server's lifetime context")
	}
}

func TestBackgroundWorkIsBoundedAndAwaited(t *testing.T) {
	h := newHarness(t)
	srv := New(h.store, coord.New(h.store), Options{Ops: OpsOptions{MaxWebhookChecks: 1}})
	release := make(chan struct{})
	finished := make(chan struct{})
	if !srv.goBackground(func(context.Context) { <-release; close(finished) }) {
		t.Fatal("the first background job was refused")
	}
	if srv.goBackground(func(context.Context) {}) {
		t.Error("a background job past the cap was started")
	}
	waited := make(chan struct{})
	go func() { srv.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("Wait returned while background work was running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the work finished")
	}
	<-finished
}
