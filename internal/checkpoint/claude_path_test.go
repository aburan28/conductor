package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureClaudePathThroughSymlinkedConfigRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", filepath.Join(root, "state"))
	physical := filepath.Join(root, "physical")
	alias := filepath.Join(root, "alias")
	project := filepath.Join(physical, "projects", "one")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "session.jsonl"),
		[]byte(`{"type":"user","sessionId":"session","cwd":"/repo","message":{"role":"user","content":"continue"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Capture(context.Background(), Request{
		Harness: "claude", SessionID: "session", Cwd: root,
		ClaudeConfigDir: alias, ClaudePath: filepath.Join(alias, "projects", "one", "session.jsonl"),
		SkipWorkspace: true, Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Manifest.Transcript.NativeRelPath; got != "projects/one/session.jsonl" {
		t.Fatalf("native relative path = %q", got)
	}
	if _, err := OpenFile(result.Path, nil); err != nil {
		t.Fatalf("checkpoint was not sealed with a safe native path: %v", err)
	}
}
