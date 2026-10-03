package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/harness"
)

// writeProject lays down the minimum .conductor a config.Load will accept, with the caller's
// `harnesses:` block appended, and returns the repository root.
func writeProject(t *testing.T, harnesses string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, config.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `apiVersion: conductor.dev/v1alpha1
kind: Project
metadata:
  id: test
  displayName: Test
` + harnesses
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestHarnessConfigsFallBackToDefaults(t *testing.T) {
	// No repository at all: `conductor doctor` runs from anywhere, so this must not fail.
	got := harnessConfigs("")
	if _, ok := got["claude"]; !ok {
		t.Fatalf("claude missing from defaults: %v", keys(got))
	}

	// A repository that declares no harnesses gets exactly the defaults.
	root := writeProject(t, "")
	if len(harnessConfigs(root)) != len(got) {
		t.Errorf("declaring no harnesses changed the set: %v", keys(harnessConfigs(root)))
	}

	// An unreadable or absent .conductor degrades rather than propagating.
	if _, ok := harnessConfigs(filepath.Join(t.TempDir(), "nope"))["claude"]; !ok {
		t.Error("a missing .conductor should fall back to defaults, not an empty map")
	}
}

func TestHarnessConfigsDeclaresAnUnknownRuntime(t *testing.T) {
	root := writeProject(t, `
harnesses:
  cairn:
    enabled: true
    command: claude
    arg_template: ["-p", "--mcp-config", "{mcp_config}", "--model", "{model}"]
`)
	configs := harnessConfigs(root)
	cairn, ok := configs["cairn"]
	if !ok {
		t.Fatalf("cairn not loaded from project.yaml: %v", keys(configs))
	}
	if !cairn.Enabled || cairn.Command != "claude" {
		t.Errorf("cairn = %+v, want enabled with command claude", cairn)
	}
	if len(cairn.ArgTemplate) != 5 || cairn.ArgTemplate[2] != "{mcp_config}" {
		t.Errorf("arg_template = %v, want the placeholder preserved verbatim", cairn.ArgTemplate)
	}
	// The declared runtime is additive: the built-ins are still there.
	if _, ok := configs["claude"]; !ok {
		t.Error("declaring cairn dropped the default harnesses")
	}

	// And it reaches the registry, through BuildRegistry's exec-driver branch.
	if _, err := buildHarnessRegistry(root).Get("cairn"); err != nil {
		t.Errorf("registry.Get(cairn): %v", err)
	}
}

func TestHarnessConfigsCanDisableABuiltIn(t *testing.T) {
	root := writeProject(t, `
harnesses:
  opencode:
    enabled: false
`)
	if harnessConfigs(root)["opencode"].Enabled {
		t.Error("opencode still enabled after the repository disabled it")
	}
	// A disabled harness is absent from the registry, not present-and-broken.
	if _, err := buildHarnessRegistry(root).Get("opencode"); err == nil {
		t.Error("registry.Get(opencode) succeeded for a disabled harness")
	}
}

func keys(m map[string]harness.HarnessConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
