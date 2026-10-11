package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The checkpoint tool is the one tool that does not go to the API. Its boundary is the
// transport: the stdio gateway runs on the harness's machine and may read the transcript
// there; the HTTP gateway runs in the control plane and must never.

func TestCheckpointToolRefusesOverHTTP(t *testing.T) {
	s := newTestServer("http://127.0.0.1:1") // nothing is called
	text, isErr := call(t, s, "coord_checkpoint", map[string]any{"note": "x"})
	if !isErr || !strings.Contains(text, "conductor checkpoint capture") {
		t.Fatalf("HTTP gateway answered %q (isError=%v); it must refuse and point at the CLI", text, isErr)
	}
}

func TestCheckpointToolHonoursKillSwitch(t *testing.T) {
	t.Setenv("CONDUCTOR_CHECKPOINT", "off")
	s := newTestServer("http://127.0.0.1:1")
	s.local = true
	if _, isErr := call(t, s, "coord_checkpoint", nil); !isErr {
		t.Fatal("disabled checkpoints were taken")
	}
}

func TestCheckpointToolCapturesLocally(t *testing.T) {
	state := t.TempDir()
	cfg := t.TempDir()
	// The real path: on macOS t.TempDir() is under /var, a symlink to /private/var, and the
	// tool sees the working directory as the latter after the chdir below. Claude Code names
	// its project directory after the real path too, so the fixture must use it.
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONDUCTOR_STATE_DIR", state)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CONDUCTOR_HARNESS", "claude")
	t.Setenv("CONDUCTOR_CHECKPOINT", "")
	slug := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, work)
	dir := filepath.Join(cfg, "projects", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","uuid":"u1","timestamp":"2026-10-05T17:37:06Z","cwd":"` + work + `","sessionId":"abc-123","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "abc-123.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	s := newTestServer("http://127.0.0.1:1")
	s.local = true
	text, isErr := call(t, s, "coord_checkpoint", map[string]any{"note": "about to hit the limit"})
	if isErr {
		t.Fatalf("checkpoint failed: %s", text)
	}
	if !strings.Contains(text, `"session_id": "abc-123"`) || !strings.Contains(text, "conductor checkpoint resume") {
		t.Errorf("unexpected result: %s", text)
	}
	if strings.Contains(text, "hello") {
		t.Error("the tool result echoed transcript content")
	}
	entries, _ := os.ReadDir(filepath.Join(state, "checkpoints"))
	if len(entries) != 2 {
		t.Errorf("expected a bundle and a manifest in the store, found %d entries", len(entries))
	}
}
