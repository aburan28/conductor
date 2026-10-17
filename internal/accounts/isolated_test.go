package accounts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBindResolvePerProject(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", state)
	work, personal := t.TempDir(), t.TempDir()
	noEnv := func(string) string { return "" }

	if err := Bind(work, "claude", "work"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := Bind(personal, "codex", "me"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	iso, ok, err := Resolve("claude", work, noEnv)
	if err != nil || !ok {
		t.Fatalf("Resolve = %+v, %v, %v", iso, ok, err)
	}
	if want := filepath.Join(state, "accounts", "claude", "work"); iso.Dir != want || iso.Env != "CLAUDE_CONFIG_DIR" {
		t.Fatalf("Resolve = %+v, want dir %s via CLAUDE_CONFIG_DIR", iso, want)
	}
	if st, err := os.Stat(iso.Dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("state dir not created owner-only: %v, %v", st, err)
	}

	// Bindings are per project and per harness.
	if _, ok, _ := Resolve("claude", personal, noEnv); ok {
		t.Error("claude resolved in a project bound only for codex")
	}
	if iso, ok, _ := Resolve("codex", personal, noEnv); !ok || iso.Env != "CODEX_HOME" {
		t.Errorf("codex = %+v, %v", iso, ok)
	}

	// CONDUCTOR_ACCOUNT overrides the binding for one launch.
	env := func(k string) string {
		if k == "CONDUCTOR_ACCOUNT" {
			return "other"
		}
		return ""
	}
	if iso, _, _ := Resolve("claude", work, env); iso.Account != "other" || iso.Source != "CONDUCTOR_ACCOUNT" {
		t.Errorf("override = %+v", iso)
	}

	// Removing the binding returns the harness to its default login.
	if err := Bind(work, "claude", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Resolve("claude", work, noEnv); ok {
		t.Error("still bound after unbinding")
	}
	all, err := Bindings()
	if err != nil || len(all) != 1 || all[0].Harness != "codex" {
		t.Errorf("Bindings = %+v, %v", all, err)
	}
}

func TestCursorIsolatesWithAFlag(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	iso, err := Prepare("cursor", "work")
	if err != nil {
		t.Fatal(err)
	}
	if iso.Env != "" || len(iso.Args) != 2 || iso.Args[0] != "--user-data-dir" || iso.Args[1] != iso.Dir {
		t.Fatalf("cursor isolation = %+v", iso)
	}
}

func TestIsolatedDirsDoNotLookLikeSnapshots(t *testing.T) {
	_, _ = withEnv(t)
	if err := Add("claude", "snap"); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare("claude", "iso"); err != nil {
		t.Fatal(err)
	}
	profiles, _ := List("claude")
	if len(profiles) != 1 || profiles[0].Name != "snap" {
		t.Errorf("List = %+v, want only the snapshot", profiles)
	}
	names, _ := Isolated("claude")
	if len(names) != 1 || names[0] != "iso" {
		t.Errorf("Isolated = %v, want [iso]", names)
	}
}

func TestRejectsPathLikeNames(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	for _, name := range []string{"", "..", "a/b", ".hidden"} {
		if _, err := Prepare("claude", name); err == nil {
			t.Errorf("Prepare(%q) accepted", name)
		}
	}
	if err := Bind(t.TempDir(), "vim", "x"); err == nil {
		t.Error("Bind accepted a harness with no isolation")
	}
}
