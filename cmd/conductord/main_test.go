package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/scheduler"
)

// clearEnv blanks every variable serve reads, so the developer's own environment cannot
// change what a test parses.
func clearEnv(t *testing.T) {
	for _, k := range []string{"CONDUCTOR_ADDR", "DATABASE_URL", "CONDUCTOR_DATABASE_MODE", "CONDUCTOR_NAT_MODE", "CONDUCTOR_NAT_INTERNAL_IP", "CONDUCTOR_TLS_CERT", "CONDUCTOR_TLS_KEY",
		"CONDUCTOR_PUBLIC_URL", "CONDUCTOR_PEERS", "CONDUCTOR_PEER_CA", "CONDUCTOR_PEER_CERT", "CONDUCTOR_PEER_KEY",
		"CONDUCTOR_PEER_DISCOVER_DNS", "CONDUCTOR_PEER_DNS_SERVER", "CONDUCTOR_SECURITY_MODE",
		"CONDUCTOR_GITHUB_API", "CONDUCTOR_GITHUB_WEB", "CONDUCTOR_RETENTION_DAYS",
		"CONDUCTOR_AUDIT_RETENTION_DAYS", "CONDUCTOR_METRICS_TOKEN", "CONDUCTOR_SECRET_KEY",
		"CONDUCTOR_SECRET_KEY_FILE", "CONDUCTOR_STATE_DIR", "CONDUCTOR_SSO_PROVIDERS",
		"CONDUCTOR_SSO_AUTO_PROVISION", "CONDUCTOR_SSO_DEFAULT_PROJECT", "CONDUCTOR_NOTIFY_PROXY", "CONDUCTOR_CONFIG"} {
		t.Setenv(k, "")
	}
}

func TestServeConfigDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/x")
	c, err := parseServeConfig(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.addr != "127.0.0.1:8080" || c.dsn != "postgres://localhost/x" || c.tick != 2*time.Second ||
		c.tickTimeout != 30*time.Second || c.shutdownTimeout != 25*time.Second {
		t.Errorf("defaults = %+v", c)
	}
	if c.database.StatementTimeout != db.DefaultStatementTimeout || c.database.LockTimeout != db.DefaultLockTimeout {
		t.Errorf("database bounds = %+v", c.database)
	}
	if c.retention != scheduler.DefaultRetention() {
		t.Errorf("retention = %+v, want the scheduler's defaults %+v", c.retention, scheduler.DefaultRetention())
	}
	if c.ops.BodyTimeout != 30*time.Second || c.ops.MaxStreams != 1000 || c.ops.MaxStreamsPerPrincipal != 16 || c.ops.MetricsToken != "" {
		t.Errorf("ops = %+v", c.ops)
	}
}

func TestServeConfigSecretKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("CONDUCTOR_STATE_DIR", "/srv/conductor")
	c, err := parseServeConfigQuiet([]string{"--dsn", "x"})
	if err != nil || c.secretKeyFile != "/srv/conductor/secret.key" || c.secretKeyEnv != "" {
		t.Errorf("default key = %q %q %v", c.secretKeyFile, c.secretKeyEnv, err)
	}
	t.Setenv("CONDUCTOR_SECRET_KEY_FILE", "/run/secrets/key")
	if c, err = parseServeConfigQuiet([]string{"--dsn", "x"}); err != nil || c.secretKeyFile != "/run/secrets/key" {
		t.Errorf("env key file = %q %v", c.secretKeyFile, err)
	}
	t.Setenv("CONDUCTOR_SECRET_KEY", "a2V5")
	if c, err = parseServeConfigQuiet([]string{"--dsn", "x", "--secret-key-file", "/etc/k"}); err != nil ||
		c.secretKeyFile != "/etc/k" || c.secretKeyEnv != "a2V5" {
		t.Errorf("flag key file = %q %q %v", c.secretKeyFile, c.secretKeyEnv, err)
	}
}

func TestServeConfigOperationsFlags(t *testing.T) {
	clearEnv(t)
	t.Setenv("CONDUCTOR_RETENTION_DAYS", "14")
	t.Setenv("CONDUCTOR_METRICS_TOKEN", "scrape")
	c, err := parseServeConfigQuiet([]string{"--dsn", "postgres://x", "--audit-retention-days", "0",
		"--db-statement-timeout", "5s", "--db-lock-timeout", "-1s", "--shutdown-timeout", "40s",
		"--tick-timeout", "7s", "--outage-after", "2m", "--max-streams-per-principal", "3", "--idempotency-ttl", "2h"})
	if err != nil {
		t.Fatal(err)
	}
	const day = 24 * time.Hour
	if c.retention.Events != 14*day || c.retention.OutboxDelivered != 7*day || c.retention.Audit != 0 || c.retention.Idempotency != 2*time.Hour {
		t.Errorf("retention = %+v", c.retention)
	}
	if c.database.StatementTimeout != 5*time.Second || c.database.LockTimeout != -time.Second {
		t.Errorf("database = %+v", c.database)
	}
	if c.shutdownTimeout != 40*time.Second || c.tickTimeout != 7*time.Second || c.outageAfter != 2*time.Minute {
		t.Errorf("timeouts = %+v", c)
	}
	if c.ops.MetricsToken != "scrape" || c.ops.MaxStreamsPerPrincipal != 3 {
		t.Errorf("ops = %+v", c.ops)
	}

	// Keeping events forever keeps their delivered outbox rows too.
	c, err = parseServeConfigQuiet([]string{"--dsn", "x", "--retention-days", "0"})
	if err != nil || c.retention.Events != 0 || c.retention.OutboxDelivered != 0 {
		t.Errorf("--retention-days 0 = %+v %v", c, err)
	}
	// A window shorter than its delivered-outbox default shortens that too.
	c, err = parseServeConfigQuiet([]string{"--dsn", "x", "--retention-days", "3"})
	if err != nil || c.retention.OutboxDelivered != 3*day {
		t.Errorf("--retention-days 3 = %+v %v", c, err)
	}
}

// The notification relay is on, and confined to public https destinations, unless the
// operator says otherwise.
func TestServeConfigNotifyFlags(t *testing.T) {
	clearEnv(t)
	c, err := parseServeConfigQuiet([]string{"--dsn", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.notifyPoll <= 0 || c.notifyNetwork.AllowPrivate || c.notifyNetwork.AllowHTTP {
		t.Errorf("defaults: poll %v, network %+v", c.notifyPoll, c.notifyNetwork)
	}
	c, err = parseServeConfigQuiet([]string{"--dsn", "x", "--notify-poll", "-1s",
		"--notify-allow-private-networks", "--notify-allow-http"})
	if err != nil {
		t.Fatal(err)
	}
	if c.notifyPoll >= 0 || !c.notifyNetwork.AllowPrivate || !c.notifyNetwork.AllowHTTP {
		t.Errorf("flags: poll %v, network %+v", c.notifyPoll, c.notifyNetwork)
	}
	if c.notifyNetwork.Proxy != nil {
		t.Errorf("a proxy with none configured: %v", c.notifyNetwork.Proxy)
	}

	t.Setenv("CONDUCTOR_NOTIFY_PROXY", "http://proxy.internal:3128")
	c, err = parseServeConfigQuiet([]string{"--dsn", "x"})
	if err != nil || c.notifyNetwork.Proxy == nil || c.notifyNetwork.Proxy.Host != "proxy.internal:3128" {
		t.Errorf("CONDUCTOR_NOTIFY_PROXY = %+v, %v", c, err)
	}
	if _, err := parseServeConfigQuiet([]string{"--dsn", "x", "--notify-proxy", "ftp://proxy"}); err == nil {
		t.Error("an ftp proxy was accepted")
	}
}

func TestServeConfigRejects(t *testing.T) {
	clearEnv(t)
	for name, args := range map[string][]string{
		"no database":          {},
		"half a TLS pair":      {"--dsn", "x", "--tls-cert", "c.pem"},
		"plaintext on a LAN":   {"--dsn", "x", "--addr", "0.0.0.0:8080"},
		"bad security mode":    {"--dsn", "x", "--security-mode", "lax"},
		"negative retention":   {"--dsn", "x", "--retention-days", "-1"},
		"short usage window":   {"--dsn", "x", "--usage-retention-days", "10"},
		"zero shutdown":        {"--dsn", "x", "--shutdown-timeout", "0s"},
		"peering without a CA": {"--dsn", "x", "--peer", "a=https://a:1"},
		"unknown flag":         {"--dsn", "x", "--no-such-flag"},
	} {
		if _, err := parseServeConfigQuiet(args); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
	}
	if _, err := parseServeConfigQuiet([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h = %v, want flag.ErrHelp (main exits 0 on it)", err)
	}
	// Each refusal above has an explicit way through.
	for name, args := range map[string][]string{
		"insecure LAN":   {"--dsn", "x", "--addr", "0.0.0.0:8080", "--insecure"},
		"behind a proxy": {"--dsn", "x", "--addr", "0.0.0.0:8080", "--behind-proxy"},
		"keep usage":     {"--dsn", "x", "--usage-retention-days", "0"},
	} {
		if _, err := parseServeConfigQuiet(args); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestServeConfigSSO(t *testing.T) {
	clearEnv(t)
	t.Setenv("CONDUCTOR_SSO_GOOGLE_CLIENT_SECRET", "g-secret")
	t.Setenv("CONDUCTOR_SSO_GITHUB_CLIENT_SECRET", "gh-secret")
	google := "name=google,issuer=https://accounts.google.com,client-id=g,domain=example.com"
	github := "name=github,client-id=gh,org=acme"

	c, err := parseServeConfigQuiet([]string{"--dsn", "x", "--public-url", "https://conductor.example.com",
		"--sso-provider", google, "--sso-provider", github,
		"--sso-auto-provision", "contributor", "--sso-default-project", "acme/web", "--sso-token-ttl", "8h"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.sso.Providers) != 2 || c.sso.AutoProvisionRole != "contributor" || c.sso.TokenTTL != 8*time.Hour ||
		c.sso.PublicURL != "https://conductor.example.com" {
		t.Fatalf("sso = %+v", c.sso)
	}

	// The environment carries several providers separated by semicolons.
	t.Setenv("CONDUCTOR_SSO_PROVIDERS", google+";"+github)
	if c, err = parseServeConfigQuiet([]string{"--dsn", "x"}); err != nil || len(c.sso.Providers) != 2 ||
		c.sso.PublicURL != "http://127.0.0.1:8080" {
		t.Fatalf("from the environment: %v %+v", err, c)
	}
	t.Setenv("CONDUCTOR_SSO_PROVIDERS", "")

	for name, args := range map[string][]string{
		"secret on the command line": {"--sso-provider", google + ",client-secret=leaked"},
		"auto-provision above contributor": {"--public-url", "https://c.example.com", "--sso-provider", google,
			"--sso-auto-provision", "maintainer", "--sso-default-project", "a/b"},
		"auto-provision without a project": {"--public-url", "https://c.example.com", "--sso-provider", google,
			"--sso-auto-provision", "observer"},
		"auto-provision with an open provider": {"--public-url", "https://c.example.com",
			"--sso-provider", "name=google,issuer=https://accounts.google.com,client-id=g",
			"--sso-auto-provision", "observer", "--sso-default-project", "a/b"},
		"plaintext public URL": {"--public-url", "http://conductor.example.com", "--sso-provider", google},
	} {
		if _, err := parseServeConfigQuiet(append([]string{"--dsn", "x"}, args...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// parseServeConfigQuiet keeps flag's usage text out of the test output.
func parseServeConfigQuiet(args []string) (*serveConfig, error) {
	return parseServeConfig(args, io.Discard)
}

// Shutdown waits for in-flight requests and for background work before runServer returns
// and the caller closes the store.
func TestRunServerShutdownOrder(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		order []string
	)
	note := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	inHandler := make(chan struct{})
	releaseHandler := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inHandler)
		<-releaseHandler
		note("request finished")
		_, _ = io.WriteString(w, "done")
	})}

	ctx, cancel := context.WithCancel(context.Background())
	var background sync.WaitGroup
	background.Add(1)
	go func() { // the scheduler: stops when ctx ends, then takes a moment to unwind
		defer background.Done()
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		note("background stopped")
	}()

	returned := make(chan error, 1)
	go func() {
		returned <- runServer(ctx, cancel, srv, func() error { return srv.Serve(ln) }, &background, 10*time.Second, logger)
		note("runServer returned")
	}()

	var body atomic.Value
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		resp, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			body.Store("error: " + err.Error())
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body.Store(string(b))
	}()
	<-inHandler
	cancel() // SIGTERM

	select {
	case <-returned:
		t.Fatal("runServer returned while a request was still in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseHandler)
	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("runServer = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer did not return after the request finished")
	}
	<-clientDone
	if got, _ := body.Load().(string); got != "done" {
		t.Errorf("the in-flight request got %q; it was cut off", got)
	}
	time.Sleep(10 * time.Millisecond) // let the last note land
	mu.Lock()
	defer mu.Unlock()
	// The scheduler stops as soon as shutdown begins, concurrently with the request draining;
	// what matters is that both happen before runServer returns and the store is closed.
	if len(order) != 3 || order[2] != "runServer returned" {
		t.Errorf("shutdown order = %v; runServer must return last", order)
	}
}

// A request that never finishes holds shutdown only until the timeout.
func TestRunServerShutdownIsBounded(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inHandler := make(chan struct{})
	stuck := make(chan struct{})
	defer close(stuck)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inHandler)
		<-stuck
	})}
	ctx, cancel := context.WithCancel(context.Background())
	var background sync.WaitGroup
	returned := make(chan struct{})
	go func() {
		_ = runServer(ctx, cancel, srv, func() error { return srv.Serve(ln) }, &background, 300*time.Millisecond, logger)
		close(returned)
	}()
	go func() { _, _ = http.Get("http://" + ln.Addr().String()) }()
	<-inHandler
	start := time.Now()
	cancel()
	select {
	case <-returned:
		if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
			t.Errorf("returned after %s, before the timeout, with a request in flight", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown was not bounded by its timeout")
	}
}

// A listener that cannot start ends the process's background work rather than leaving it
// running with no server.
func TestRunServerListenFailure(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var background sync.WaitGroup
	background.Add(1)
	go func() {
		defer background.Done()
		<-ctx.Done()
	}()
	srv := &http.Server{}
	err := runServer(ctx, cancel, srv, func() error { return errors.New("address already in use") }, &background, time.Second, logger)
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("runServer = %v", err)
	}
	if ctx.Err() == nil {
		t.Error("background work was not cancelled after the listener failed")
	}
}

// TestMain lets a test run the real main() in a child process (runMain), so exit status and
// stderr are observed exactly as an operator sees them.
func TestMain(m *testing.M) {
	if args := os.Getenv("CONDUCTORD_RUN_MAIN"); args != "" {
		os.Args = append([]string{"conductord"}, strings.Fields(args)...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runMain(t *testing.T, args string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "CONDUCTORD_RUN_MAIN="+args, "DATABASE_URL=", "CONDUCTOR_ADDR=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), stderr.String()
	default:
		t.Fatal(err)
		return 0, ""
	}
}

// A flag that does not parse exits 2 and is reported once, as flag's ExitOnError did; -h
// exits 0; a configuration error exits 1.
func TestMainExitStatus(t *testing.T) {
	code, stderr := runMain(t, "--no-such-flag")
	if code != 2 {
		t.Errorf("bad flag exited %d, want 2", code)
	}
	if n := strings.Count(stderr, "flag provided but not defined: -no-such-flag"); n != 1 {
		t.Errorf("the flag error was printed %d times:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("no usage text:\n%s", stderr)
	}

	if code, stderr := runMain(t, "-h"); code != 0 || !strings.Contains(stderr, "Usage:") {
		t.Errorf("-h exited %d:\n%s", code, stderr)
	}

	code, stderr = runMain(t, "--addr 127.0.0.1:0")
	if code != 1 || strings.Count(stderr, "no database configured") != 1 || strings.Contains(stderr, "Usage:") {
		t.Errorf("missing database exited %d:\n%s", code, stderr)
	}
}
