package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func leaderTestEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"DATABASE_URL", "CONDUCTOR_CONFIG", "CONDUCTOR_DATABASE_MODE", "CONDUCTOR_ADDR", "CONDUCTOR_PUBLIC_URL", "CONDUCTOR_TLS_CERT", "CONDUCTOR_TLS_KEY", "PGSSLMODE", "PGSSLROOTCERT"} {
		t.Setenv(name, "")
	}
}

func TestLeaderExternalNeedsExplicitDatabase(t *testing.T) {
	leaderTestEnvironment(t)
	// A locally saved database must never be silently used for an external leader.
	if err := saveDSN("postgres://user@localhost/local?sslmode=disable"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"external", "rds"} {
		if _, err := parseLeaderConfig([]string{"--database", mode}, io.Discard); err == nil {
			t.Fatalf("%s accepted without an explicit DSN", mode)
		}
	}
}

func TestLeaderForwardsTLSAndNATWithoutCredentialsInArgv(t *testing.T) {
	leaderTestEnvironment(t)
	c, err := parseLeaderConfig([]string{
		"--database", "external", "--dsn", "postgres://user:private-secret@localhost/db?sslmode=disable",
		"--addr", "0.0.0.0:8443", "--public-url", "https://leader.example:9443",
		"--tls-cert", "server.pem", "--tls-key", "server.key", "--nat-mode", "upnp",
		"--nat-internal-ip", "192.168.1.20", "--nat-external-port", "9443", "--nat-lease", "30m", "--behind-proxy=false",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(c.daemonArgs, " ")
	for _, required := range []string{"--database external", "--security-mode enhanced", "--addr=0.0.0.0:8443", "--tls-cert=server.pem", "--tls-key=server.key", "--nat-mode=upnp", "--nat-external-port=9443", "--nat-lease=30m", "--behind-proxy=false"} {
		if !strings.Contains(joined, required) {
			t.Errorf("missing %q from forwarded server arguments", required)
		}
	}
	if strings.Contains(joined, "private-secret") || strings.Contains(joined, "--dsn") {
		t.Fatal("database credentials placed in argv")
	}
	if c.endpoint != "https://leader.example:9443" {
		t.Fatalf("bootstrap endpoint = %q", c.endpoint)
	}
}

func TestLeaderConfigSecretAndFlagPrecedence(t *testing.T) {
	leaderTestEnvironment(t)
	t.Setenv("LEADER_TEST_DB", "postgres://user@localhost/configdb?sslmode=disable")
	path := filepath.Join(t.TempDir(), "leader.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nserver:\n  addr: 0.0.0.0:8443\n  public_url: https://leader.example\ndatabase:\n  mode: external\n  url_env: LEADER_TEST_DB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseLeaderConfig([]string{"--config", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.database != "external" || !strings.Contains(c.dsn, "configdb") || c.endpoint != "https://leader.example" {
		t.Fatal("config database, secret or public URL was lost")
	}
	if strings.Contains(strings.Join(c.daemonArgs, " "), "--addr") {
		t.Fatal("absent CLI address overrides the configured address")
	}
	t.Setenv("DATABASE_URL", "postgres://user@localhost/envdb?sslmode=disable")
	c, err = parseLeaderConfig([]string{"--config", path, "--dsn", "postgres://user@localhost/flagdb?sslmode=disable", "--public-url", "https://override.example"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.dsn, "flagdb") || c.endpoint != "https://override.example" {
		t.Fatal("explicit flags did not override environment/config")
	}
}

func TestLeaderExternalLoopbackSkipsDockerAndStartsLocally(t *testing.T) {
	leaderTestEnvironment(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "daemon.args")
	daemon := filepath.Join(dir, "conductord")
	if err := os.WriteFile(daemon, []byte("#!/bin/sh\nif [ \"$1\" = config ]; then exit 0; fi\nprintf '%s\\n' \"$@\" > \"$LEADER_TEST_ARGS\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf called > \"$LEADER_TEST_DOCKER\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CONDUCTOR_DAEMON", daemon)
	t.Setenv("LEADER_TEST_ARGS", marker)
	t.Setenv("LEADER_TEST_DOCKER", filepath.Join(dir, "docker.called"))
	if err := cmdLeader(context.Background(), []string{"--database", "external", "--dsn", "postgres://user@localhost:1/db?sslmode=disable", "--public-url", "https://remote.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("advertised remote URL prevented the local leader from starting")
	}
	if _, err := os.Stat(filepath.Join(dir, "docker.called")); !os.IsNotExist(err) {
		t.Fatal("external loopback database triggered Docker")
	}
}

func TestLeaderCancellationSendsGracefulTermination(t *testing.T) {
	dir := t.TempDir()
	ready, stopped := filepath.Join(dir, "ready"), filepath.Join(dir, "stopped")
	daemon := filepath.Join(dir, "daemon")
	body := "#!/bin/sh\ntrap 'printf stopped > \"$LEADER_TEST_STOPPED\"; exit 0' TERM\nprintf ready > \"$LEADER_TEST_READY\"\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(daemon, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADER_TEST_READY", ready)
	t.Setenv("LEADER_TEST_STOPPED", stopped)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := leaderCommand(ctx, daemon, "postgres://user@localhost/db", nil)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("test daemon did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	_ = cmd.Wait()
	if _, err := os.Stat(stopped); err != nil {
		t.Fatal("daemon was killed without its SIGTERM cleanup")
	}
}
