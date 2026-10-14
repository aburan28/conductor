package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The wrap heartbeat reports which paths the working tree has touched since the session
// started — committed, modified, and untracked alike — and nothing else.
func TestObservedPathsListsTheWorkingTreeDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
		// A git hook (the pre-push check) sets GIT_DIR and friends; inherited, they would
		// point this git at the repository being pushed instead of dir.
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(k, "GIT_") || k == "GIT_CONFIG_NOSYSTEM" {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("a.go", "package a\n")
	write("b.go", "package b\n")
	write(".gitignore", "*.log\n")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	t.Chdir(dir)
	base, err := gitOutput(context.Background(), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	if got := observedPaths(context.Background(), dir, base); len(got) != 0 {
		t.Fatalf("clean tree = %v, want nothing", got)
	}

	write("b.go", "package b // changed\n")        // modified, then committed
	git("commit", "-q", "-am", "work")             // since the session started
	write("a.go", "package a // edited\n")         // modified, uncommitted
	write("internal/new.go", "package internal\n") // untracked
	write("debug.log", "noise\n")                  // ignored
	// From a subdirectory, as a tool started there would run it.
	t.Chdir(filepath.Join(dir, "internal"))
	got := observedPaths(context.Background(), worktreeRoot(context.Background()), base)
	want := []string{"a.go", "b.go", "internal/new.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("observed = %v, want %v", got, want)
	}
}
