package integrations

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testOptions builds Options rooted entirely inside temp directories, so no test can touch
// the real home directory or repository.
func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Root:         t.TempDir(),
		Home:         t.TempDir(),
		Project:      "demo",
		Endpoint:     "http://localhost:8080",
		Token:        "cdt_secret",
		Transport:    TransportStdio,
		Hooks:        true,
		Rules:        true,
		ManagedBlock: "Check with Conductor before editing.",
		Getenv:       func(string) string { return "" },
		LookPath:     func(string) (string, error) { return "", errors.New("not found") },
	}
}

func apply(t *testing.T, r Result) {
	t.Helper()
	if err := Apply(r, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func TestClaudeProjectPlanWritesMCPAndHooksAndIsIdempotent(t *testing.T) {
	o := testOptions(t)

	// Seed both files with user content that must survive.
	mcpPath := filepath.Join(o.Root, ".mcp.json")
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"other":{"command":"other-mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(o.Root, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	userHook := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"my-linter"}]}]},"model":"opus"}`
	if err := os.WriteFile(settingsPath, []byte(userHook), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := claudeTool.Plan(o)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	apply(t, res)

	m := readJSON(t, mcpPath)
	if !hasKey(m, "mcpServers", "conductor") || !hasKey(m, "mcpServers", "other") {
		t.Fatalf("conductor entry missing or user entry lost: %v", m)
	}
	settings := readJSON(t, settingsPath)
	if settings["model"] != "opus" {
		t.Fatalf("user setting lost: %v", settings)
	}
	if !claudeHooksInstalled(settings) {
		t.Fatalf("hooks not installed: %v", settings)
	}
	body, _ := os.ReadFile(settingsPath)
	if !strings.Contains(string(body), "my-linter") {
		t.Fatalf("user hook lost:\n%s", body)
	}
	if !strings.Contains(string(body), "Edit|Write|MultiEdit|NotebookEdit") {
		t.Fatalf("matcher missing:\n%s", body)
	}

	// Second run changes nothing.
	res2, err := claudeTool.Plan(o)
	if err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	for _, op := range res2.Ops {
		if op.Action != "unchanged" {
			t.Fatalf("second run not idempotent: %s %s", op.Action, op.Path)
		}
	}

	// Removal takes ours out and leaves the user's.
	o.Remove = true
	res3, err := claudeTool.Plan(o)
	if err != nil {
		t.Fatalf("remove plan: %v", err)
	}
	apply(t, res3)
	m = readJSON(t, mcpPath)
	if hasKey(m, "mcpServers", "conductor") || !hasKey(m, "mcpServers", "other") {
		t.Fatalf("removal wrong: %v", m)
	}
	settings = readJSON(t, settingsPath)
	if claudeHooksInstalled(settings) {
		t.Fatal("hooks not removed")
	}
	body, _ = os.ReadFile(settingsPath)
	if !strings.Contains(string(body), "my-linter") {
		t.Fatalf("user hook lost on removal:\n%s", body)
	}
}

func TestClaudeGlobalUsesTheCLI(t *testing.T) {
	o := testOptions(t)
	o.Global = true
	res, err := claudeTool.Plan(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Commands) != 1 || !strings.Contains(res.Commands[0], "claude mcp add --scope user") {
		t.Fatalf("commands = %v", res.Commands)
	}
	// No token may appear anywhere in a plan's commands.
	if strings.Contains(res.Commands[0], o.Token) {
		t.Fatalf("token leaked into command: %s", res.Commands[0])
	}
}

func TestCursorHTTPReferencesEnvNotToken(t *testing.T) {
	o := testOptions(t)
	o.Transport = TransportHTTP
	res, err := cursorTool.Plan(o)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, res)
	body, _ := os.ReadFile(filepath.Join(o.Root, ".cursor", "mcp.json"))
	if !strings.Contains(string(body), "${env:CONDUCTOR_TOKEN}") {
		t.Fatalf("no env reference:\n%s", body)
	}
	if strings.Contains(string(body), o.Token) {
		t.Fatalf("token written to project file:\n%s", body)
	}
	if !strings.Contains(string(body), "/mcp?project=demo") {
		t.Fatalf("wrong URL:\n%s", body)
	}
	// Rules file rides along.
	rules, err := os.ReadFile(filepath.Join(o.Root, ".cursor", "rules", "conductor.mdc"))
	if err != nil || !strings.Contains(string(rules), "alwaysApply: true") {
		t.Fatalf("rules file: %v\n%s", err, rules)
	}
}

func TestCodexTOMLPreservesEverythingElse(t *testing.T) {
	o := testOptions(t)
	path := filepath.Join(o.Home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := "# my settings\nmodel = \"gpt-5-codex\"\n\n[mcp_servers.github]\ncommand = \"gh-mcp\"\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := codexTool.Plan(o)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, res)
	body, _ := os.ReadFile(path)
	text := string(body)
	for _, want := range []string{"# my settings", "model = \"gpt-5-codex\"", "[mcp_servers.github]",
		"[mcp_servers.conductor]", `command = "conductor-mcp"`, `args = ["--project", "demo"]`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}

	// Idempotent.
	res2, _ := codexTool.Plan(o)
	if res2.Ops[0].Action != "unchanged" {
		t.Fatalf("not idempotent: %s", res2.Ops[0].Action)
	}

	// HTTP swaps the table body for url + env-var token reference.
	o.Transport = TransportHTTP
	res3, _ := codexTool.Plan(o)
	apply(t, res3)
	body, _ = os.ReadFile(path)
	if !strings.Contains(string(body), `bearer_token_env_var = "CONDUCTOR_TOKEN"`) ||
		strings.Contains(string(body), o.Token) {
		t.Fatalf("http table wrong:\n%s", body)
	}

	// Removal leaves the rest of the file alone.
	o.Remove = true
	res4, _ := codexTool.Plan(o)
	apply(t, res4)
	body, _ = os.ReadFile(path)
	if strings.Contains(string(body), "conductor") || !strings.Contains(string(body), "[mcp_servers.github]") {
		t.Fatalf("removal wrong:\n%s", body)
	}
}

func TestOpenCodeWritesConfigAndPlugin(t *testing.T) {
	o := testOptions(t)
	res, err := opencodeTool.Plan(o)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, res)

	m := readJSON(t, filepath.Join(o.Root, "opencode.json"))
	if !hasKey(m, "mcp", "conductor") {
		t.Fatalf("mcp entry missing: %v", m)
	}
	if m["$schema"] != "https://opencode.ai/config.json" {
		t.Fatalf("schema missing on create: %v", m)
	}
	plugin, err := os.ReadFile(filepath.Join(o.Root, ".opencode", "plugins", "conductor.js"))
	if err != nil || !strings.Contains(string(plugin), pluginMarker) {
		t.Fatalf("plugin: %v", err)
	}

	// A user's own conductor.js (no marker) must never be deleted by --remove.
	own := filepath.Join(o.Root, ".opencode", "plugins", "conductor.js")
	if err := os.WriteFile(own, []byte("// mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.Remove = true
	res2, _ := opencodeTool.Plan(o)
	apply(t, res2)
	if _, err := os.Stat(own); err != nil {
		t.Fatal("user's plugin file was deleted")
	}
}

func TestOpenCodeJSONCIsNotClobbered(t *testing.T) {
	o := testOptions(t)
	path := filepath.Join(o.Root, "opencode.jsonc")
	if err := os.WriteFile(path, []byte("{\n  // my config\n  \"theme\": \"dark\",\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := opencodeTool.Plan(o)
	if err != nil {
		t.Fatalf("plan should not fail hard: %v", err)
	}
	if res.Unparseable == "" || res.Snippet == "" {
		t.Fatalf("expected unparseable + snippet, got %+v", res)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "// my config") {
		t.Fatal("jsonc file was modified")
	}
}

func TestZedUnparseableSettingsGetSnippet(t *testing.T) {
	o := testOptions(t)
	path := zedSettings(o)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\n  // comments are legal in zed\n  \"theme\": \"One Dark\",\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := zedTool.Plan(o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Unparseable != path || !strings.Contains(res.Snippet, "context_servers") {
		t.Fatalf("expected snippet for %s, got %+v", path, res)
	}
}

func TestGeminiHTTPRefusesTokenInProjectFile(t *testing.T) {
	o := testOptions(t)
	o.Transport = TransportHTTP
	if _, err := geminiTool.Plan(o); err == nil {
		t.Fatal("project-level http should refuse to write a literal token")
	}
	o.Global = true
	res, err := geminiTool.Plan(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("writing a literal token must warn")
	}
	apply(t, res)
	body, _ := os.ReadFile(filepath.Join(o.Home, ".gemini", "settings.json"))
	if !strings.Contains(string(body), "Bearer cdt_secret") || !strings.Contains(string(body), "httpUrl") {
		t.Fatalf("global http entry wrong:\n%s", body)
	}
}

func TestVSCodeWorkspacePlanAndGlobalSnippet(t *testing.T) {
	o := testOptions(t)
	res, err := vscodeTool.Plan(o)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, res)
	m := readJSON(t, filepath.Join(o.Root, ".vscode", "mcp.json"))
	if !hasKey(m, "servers", "conductor") {
		t.Fatalf("servers entry missing: %v", m)
	}
	o.Global = true
	res2, err := vscodeTool.Plan(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Ops) != 0 || res2.Snippet == "" {
		t.Fatalf("global should produce a snippet, got %+v", res2)
	}
}

func TestStatusesReflectApplication(t *testing.T) {
	o := testOptions(t)
	res, err := claudeTool.Plan(o)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, res)
	st := claudeTool.Status(o)
	if !st.Configured || !st.Hooks || st.Transport != TransportStdio {
		t.Fatalf("status = %+v", st)
	}
	if cursor := cursorTool.Status(o); cursor.Configured {
		t.Fatalf("cursor should not be configured: %+v", cursor)
	}
}

func TestSetTOMLTableInsertsBetweenTables(t *testing.T) {
	in := "[a]\nx = 1\n\n[b]\ny = 2\n"
	out := setTOMLTable(in, "a", []string{"x = 3"})
	if !strings.Contains(out, "x = 3") || strings.Contains(out, "x = 1") || !strings.Contains(out, "[b]\ny = 2") {
		t.Fatalf("wrong:\n%s", out)
	}
	if removeTOMLTable(out, "b") != "[a]\nx = 3\n" {
		t.Fatalf("remove wrong:\n%q", removeTOMLTable(out, "b"))
	}
}
