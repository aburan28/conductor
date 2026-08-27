package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedBlockBodyHasNoMarkers(t *testing.T) {
	body := managedBlockBody()
	if strings.Contains(body, "conductor:begin") || strings.Contains(body, "conductor:end") {
		t.Fatalf("markers leaked: %q", body)
	}
	if !strings.Contains(body, "conductor check") {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestIntegrationOptionsRejectsBadTransport(t *testing.T) {
	if _, err := integrationOptions(false, "carrier-pigeon", "", "p", true, true, false); err == nil {
		t.Fatal("bad transport accepted")
	}
}

func TestWrapMCPArgsClaude(t *testing.T) {
	t.Chdir(t.TempDir()) // no repository: ClaudeProjectConfigured is false, FindRoot fails safely
	restore := lookPath
	lookPath = func(name string) (string, error) { return "/usr/local/bin/" + name, nil }
	defer func() { lookPath = restore }()

	extra, cleanup := wrapMCPArgs("claude", nil, "sess-1", "demo", "http://localhost:8080")
	defer cleanup()
	if len(extra) != 2 || extra[0] != "--mcp-config" {
		t.Fatalf("extra = %v", extra)
	}
	body, err := os.ReadFile(extra[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"conductor-mcp", "sess-1", "demo"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("config missing %q:\n%s", want, body)
		}
	}
	info, _ := os.Stat(extra[1])
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mcp config mode = %v, want 0600", info.Mode().Perm())
	}
	cleanup()
	if _, err := os.Stat(extra[1]); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup left the temp config behind")
	}

	// An explicit flag from the user wins.
	if extra, _ := wrapMCPArgs("claude", []string{"--mcp-config", "mine.json"}, "s", "p", ""); extra != nil {
		t.Fatalf("should defer to the user's flag, got %v", extra)
	}
}

func TestWrapMCPArgsCodexAndMissingBinary(t *testing.T) {
	t.Chdir(t.TempDir())
	restore := lookPath
	defer func() { lookPath = restore }()

	lookPath = func(name string) (string, error) { return "/opt/bin/" + name, nil }
	extra, _ := wrapMCPArgs("codex", nil, "sess", "demo", "")
	joined := strings.Join(extra, " ")
	if !strings.Contains(joined, "mcp_servers.conductor.command=") ||
		!strings.Contains(joined, `mcp_servers.conductor.args=["--project","demo"]`) {
		t.Fatalf("codex overrides wrong: %v", extra)
	}

	// conductor-mcp not installed: mount nothing rather than break the launch.
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if extra, _ := wrapMCPArgs("claude", nil, "s", "p", ""); extra != nil {
		t.Fatalf("expected nil without conductor-mcp, got %v", extra)
	}

	// A repository already configured by `conductor integrate claude` mounts nothing extra.
	lookPath = func(name string) (string, error) { return "/opt/bin/" + name, nil }
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"),
		[]byte(`{"mcpServers":{"conductor":{"command":"conductor-mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	if extra, _ := wrapMCPArgs("claude", nil, "s", "p", ""); extra != nil {
		t.Fatalf("expected nil when .mcp.json already mounts conductor, got %v", extra)
	}
}
