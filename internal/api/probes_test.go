package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

func probeServer(t *testing.T, cfg *pgxpool.Config, life context.Context) *Server {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := db.NewWithPool(pool)
	return New(store, coord.New(store), Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Ops:    OpsOptions{BaseContext: life},
	})
}

func assertProbe(t *testing.T, srv *Server, path string, code int, status string) {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != code {
		t.Fatalf("%s status = %d, want %d; body %s", path, w.Code, code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// The public probes expose no schema versions, database details or tenant data.
	if len(body) != 1 || body["status"] != status {
		t.Errorf("%s body = %v", path, body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("%s must disable response caching", path)
	}
}

func TestPublicProbesDatabaseUnavailable(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://secretuser:secretpassword@127.0.0.1:1/nodb?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := probeServer(t, cfg, life)
	assertProbe(t, srv, "/v1/live", http.StatusOK, "ok")
	assertProbe(t, srv, "/v1/readyz", http.StatusServiceUnavailable, "unavailable")
	cancel()
	assertProbe(t, srv, "/v1/live", http.StatusServiceUnavailable, "unavailable")
	assertProbe(t, srv, "/v1/readyz", http.StatusServiceUnavailable, "unavailable")
}

func TestPublicReadinessBoundsUnresponsiveDatabase(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-stop // Accept TCP but never answer PostgreSQL's handshake.
	}()
	cfg, err := pgxpool.ParseConfig("postgres://secretuser:secretpassword@" + listener.Addr().String() + "/nodb?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	srv := probeServer(t, cfg, context.Background())
	started := time.Now()
	assertProbe(t, srv, "/v1/readyz", http.StatusServiceUnavailable, "unavailable")
	if elapsed := time.Since(started); elapsed > readinessTimeout+time.Second {
		t.Errorf("readiness took %s, budget is %s", elapsed, readinessTimeout)
	}
}

func TestPublicReadinessDatabaseAndShutdown(t *testing.T) {
	h := newHarness(t)
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(h.store, coord.New(h.store), Options{Ops: OpsOptions{BaseContext: life}})
	assertProbe(t, srv, "/v1/readyz", http.StatusOK, "ok")
	cancel()
	assertProbe(t, srv, "/v1/readyz", http.StatusServiceUnavailable, "unavailable")
	assertProbe(t, srv, "/v1/live", http.StatusServiceUnavailable, "unavailable")
}

func TestPublicReadinessRejectsReadOnlyAndMissingSchema(t *testing.T) {
	newHarness(t) // Ensure the shared test database is migrated.
	for _, tc := range []struct{ name, parameter, value string }{
		{"read-only", "default_transaction_read_only", "on"},
		{"missing-schema", "search_path", "pg_catalog"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			cfg.ConnConfig.RuntimeParams[tc.parameter] = tc.value
			srv := probeServer(t, cfg, context.Background())
			assertProbe(t, srv, "/v1/readyz", http.StatusServiceUnavailable, "unavailable")
		})
	}
}
