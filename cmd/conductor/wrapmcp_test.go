package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWrapMCPArgsCodexOverridesGlobalProjectPerSession(t *testing.T) {
	t.Chdir(t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	configPath := filepath.Join(codexHome, "config.toml")
	globalConfig := "[mcp_servers.conductor]\ncommand = \"conductor-mcp\"\nargs = [\"--project\", \"demo\"]\n"
	if err := os.WriteFile(configPath, []byte(globalConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	restore := lookPath
	lookPath = func(name string) (string, error) { return "/opt/bin/" + name, nil }
	t.Cleanup(func() { lookPath = restore })

	extra, cleanup := wrapMCPArgs("codex", nil, "session-1", "cryptanalysis", "https://conductor.example")
	cleanup()
	joined := strings.Join(extra, " ")
	for _, want := range []string{
		`mcp_servers.conductor.command=/opt/bin/conductor-mcp`,
		`mcp_servers.conductor.args=["--project","cryptanalysis","--endpoint","https://conductor.example"]`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Codex overrides missing %q in %v", want, extra)
		}
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != globalConfig {
		t.Fatalf("wrap changed the global MCP config:\n%s", after)
	}
}
