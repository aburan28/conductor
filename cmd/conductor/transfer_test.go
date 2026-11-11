package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTransferExportImportCLI drives the two commands end to end against a throwaway
// Claude home, so the flag wiring, the file it writes, and the destination it chooses are
// all exercised without touching a real ~/.claude.
func TestTransferExportImportCLI(t *testing.T) {
	src := t.TempDir()
	project := filepath.Join(t.TempDir(), "app")
	id := "11111111-1111-4111-8111-111111111111"
	dir := filepath.Join(src, "projects", strings.ReplaceAll(project, "/", "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","sessionId":"` + id + `","cwd":"` + project + `","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "app.codexbundle")
	if err := transferExport([]string{
		"--tool", "claude", "--claude-home", src, "--project", project, "--output", out,
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("bundle not written: %v", err)
	}

	dst := t.TempDir()
	if err := transferImport([]string{out, "--claude-home", dst}); err != nil {
		t.Fatalf("import: %v", err)
	}
	got := filepath.Join(dst, "projects", strings.ReplaceAll(project, "/", "-"), id+".jsonl")
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("session not imported: %v", err)
	}

	// A dry run on a fresh destination must write nothing.
	dst2 := t.TempDir()
	if err := transferImport([]string{out, "--claude-home", dst2, "--dry-run"}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst2, "projects")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote to the destination: %v", err)
	}
}
