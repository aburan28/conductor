package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListenAddrFromEndpoint(t *testing.T) {
	cases := []struct {
		ep   string
		want string
	}{
		{"http://localhost:8080", "localhost:8080"},
		{"http://127.0.0.1:9999", "127.0.0.1:9999"},
		{"http://localhost:8080/", "localhost:8080"},
		{"http://0.0.0.0:8081", "127.0.0.1:8081"},
		{"https://example.com:8443", "example.com:8443"},
		{"not a url", "127.0.0.1:8080"},
		{"", "127.0.0.1:8080"},
	}
	for _, tc := range cases {
		if got := listenAddrFromEndpoint(tc.ep); got != tc.want {
			t.Errorf("listenAddrFromEndpoint(%q) = %q, want %q", tc.ep, got, tc.want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"", "localhost", "127.0.0.1", "::1"} {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "10.0.0.5", "example.com"} {
		if isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = true, want false", host)
		}
	}
}

func TestRemoteEndpoint(t *testing.T) {
	if remoteEndpoint("http://localhost:8080") {
		t.Error("http://localhost:8080 reported remote")
	}
	if remoteEndpoint("http://127.0.0.1:8080") {
		t.Error("http://127.0.0.1:8080 reported remote")
	}
	if !remoteEndpoint("https://conductor.example.com") {
		t.Error("https://conductor.example.com not reported remote")
	}
}

func TestRemoteUpNeedsNoLocalDaemonOrDatabase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CONDUCTOR_DAEMON", "/missing/conductord")
	t.Setenv("CONDUCTOR_TOKEN", "")
	err := cmdUp(context.Background(), []string{"--endpoint", "https://leader.example"})
	if err == nil || !strings.Contains(err.Error(), "no valid login") {
		t.Fatalf("remote up should ask for login without starting a daemon: %v", err)
	}
	if strings.Contains(err.Error(), "conductord not found") {
		t.Fatal("remote worker requires a local daemon")
	}
}

func TestAuthenticatedRemoteUpDoesNotRequireDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" || r.Header.Get("Authorization") != "Bearer worker-token" {
			t.Errorf("unexpected worker login verification request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	oldTransport := http.DefaultTransport
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport; transport.CloseIdleConnections() })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CONDUCTOR_DAEMON", "/missing/conductord")
	t.Setenv("CONDUCTOR_ENDPOINT", "http://leader.example")
	t.Setenv("CONDUCTOR_TOKEN", "worker-token")
	t.Setenv("CONDUCTOR_CA_CERT", "")
	// Even an inherited database URL must not be used when the worker is logged in.
	t.Setenv("DATABASE_URL", "postgres://unused@localhost:1/db?sslmode=disable")
	if err := cmdUp(context.Background(), nil); err != nil {
		t.Fatalf("authenticated remote worker should need only the CLI: %v", err)
	}
}

func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := tailLines(path, 2); got != "three\nfour" {
		t.Errorf("tailLines = %q, want %q", got, "three\nfour")
	}
	if got := tailLines(filepath.Join(dir, "missing"), 2); got != "<no log>" {
		t.Errorf("tailLines(missing) = %q, want <no log>", got)
	}
}

func TestFindComposeFileSkipsUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	// A compose file that is not conductor's must not be picked up.
	other := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(other, []byte("services:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if d, n := findComposeFile(); d != "" || n != "" {
		t.Errorf("findComposeFile() = %q %q, want empty for an unrelated compose file", d, n)
	}
	// The conductor compose file is recognized.
	if err := os.WriteFile(other, []byte("services:\n  db:\n    container_name: conductor-db\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, n := findComposeFile(); filepath.Base(d) != filepath.Base(dir) || n != "docker-compose.yml" {
		t.Errorf("findComposeFile() = %q %q, want the local conductor compose file", d, n)
	}
}

// The DSN `up` hands conductord is the database password; on the command line it would be
// readable by every local user through ps.
func TestDaemonCommandPassesTheDSNInTheEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://stale@elsewhere/db")
	const dsn = "postgres://conductor:s3cret@localhost:55432/conductor?sslmode=disable"
	cmd := daemonCommand("/bin/conductord", dsn, "--addr", "127.0.0.1:8080")
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "s3cret") {
			t.Errorf("the DSN is on the command line: %q", cmd.Args)
		}
	}
	found := 0
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "DATABASE_URL=") {
			found++
			if kv != "DATABASE_URL="+dsn {
				t.Errorf("env carries %q, want the resolved DSN", kv)
			}
		}
	}
	if found != 1 {
		t.Errorf("DATABASE_URL appears %d times in the daemon's environment, want exactly once", found)
	}
}

// A fresh install gets a random password, saved owner-only and reused on the next `up`;
// --dsn and DATABASE_URL still win.
func TestResolveDSNGeneratesAndPersistsAPassword(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got, src, _ := resolveDSN("postgres://flag", "postgres://env"); got != "postgres://flag" || src != dsnFlag {
		t.Errorf("flag = %q (%s), want the flag", got, src)
	}
	if got, src, _ := resolveDSN("", "postgres://env"); got != "postgres://env" || src != dsnEnv {
		t.Errorf("env = %q (%s), want DATABASE_URL", got, src)
	}

	first, src, err := resolveDSN("", "")
	if err != nil || src != dsnGenerated {
		t.Fatalf("fresh resolve = %q (%s, %v), want a generated DSN", first, src, err)
	}
	u, err := url.Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := u.User.Password()
	if len(pass) < 32 || pass == "conductor" {
		t.Errorf("generated password %q is not random enough", pass)
	}
	if u.Hostname() != "localhost" || u.Port() != "55432" {
		t.Errorf("generated DSN targets %s, want the local database", u.Host)
	}
	again, _, _ := resolveDSN("", "")
	if again == first {
		t.Error("two unsaved resolutions produced the same password")
	}

	if err := saveDSN(first); err != nil {
		t.Fatal(err)
	}
	path, _ := dsnPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("saved DSN mode = %v, want 0600", info.Mode().Perm())
	}
	if got, src, _ := resolveDSN("", ""); got != first || src != dsnSaved {
		t.Errorf("after saving, resolve = %q (%s), want the saved DSN", got, src)
	}
}

func TestPublishedBeyondLoopback(t *testing.T) {
	for in, want := range map[string]bool{
		"":                   false,
		"127.0.0.1|":         false,
		"|":                  true, // docker's "all interfaces"
		"0.0.0.0|":           true,
		"127.0.0.1|0.0.0.0|": true,
		"::|":                true,
		"127.0.0.1|::1|\n":   false,
	} {
		if got := publishedBeyondLoopback(in); got != want {
			t.Errorf("publishedBeyondLoopback(%q) = %v, want %v", in, got, want)
		}
	}
}

// An installation from before generated passwords has a volume initialized with the fixed
// one. The first `up` after upgrading must keep working against it, not lock the operator out.
func TestSettleGeneratedDSNFallsBackToTheLegacyPassword(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := u.User.Password(); u.User.Username() != "conductor" || p != "conductor" {
		t.Skip("the test database does not use the legacy credentials")
	}
	u.User = url.UserPassword("conductor", "generated-and-wrong")
	if probeDSN(context.Background(), u.String()) == nil {
		t.Skip("the test database does not check passwords (trust authentication)")
	}
	got, err := settleGeneratedDSN(context.Background(), u.String())
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if pu, _ := url.Parse(got); pu.Path != u.Path || pu.Host != u.Host {
		t.Errorf("settled on %s, want the same database as %s", got, u.Redacted())
	}
	if p, _ := mustParse(t, got).User.Password(); p != "conductor" {
		t.Errorf("settled password = %q, want the legacy one", p)
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
