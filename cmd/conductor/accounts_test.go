package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aburan28/conductor/internal/accounts"
)

func TestApplyAccountUsesProjectBinding(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", state)
	t.Setenv("CONDUCTOR_ACCOUNT", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	project := t.TempDir()
	if err := accounts.Bind(project, "claude", "work"); err != nil {
		t.Fatal(err)
	}

	if extra := applyAccount("claude", project, nil); extra != nil {
		t.Errorf("claude extra args = %v, want none", extra)
	}
	want := filepath.Join(state, "accounts", "claude", "work")
	if got := os.Getenv("CLAUDE_CONFIG_DIR"); got != want {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", got, want)
	}
}

func TestApplyAccountLeavesAnExportedDirectoryAlone(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	t.Setenv("CONDUCTOR_ACCOUNT", "")
	t.Setenv("CODEX_HOME", "/elsewhere")
	project := t.TempDir()
	if err := accounts.Bind(project, "codex", "work"); err != nil {
		t.Fatal(err)
	}
	applyAccount("codex", project, nil)
	if got := os.Getenv("CODEX_HOME"); got != "/elsewhere" {
		t.Errorf("CODEX_HOME = %q, want the exported value kept", got)
	}
}

func TestApplyAccountCursorFlag(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	t.Setenv("CONDUCTOR_ACCOUNT", "")
	project := t.TempDir()
	if err := accounts.Bind(project, "cursor", "work"); err != nil {
		t.Fatal(err)
	}
	if extra := applyAccount("cursor", project, []string{"."}); len(extra) != 2 || extra[0] != "--user-data-dir" {
		t.Errorf("cursor extra args = %v", extra)
	}
	if extra := applyAccount("cursor", project, []string{"--user-data-dir=/mine", "."}); extra != nil {
		t.Errorf("explicit --user-data-dir overridden: %v", extra)
	}
}
