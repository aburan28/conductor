package tokencost

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The estimator's one job is to be honestly close: near 4 chars/token on prose, denser on
// code and JSON, and identical on every run. These tests pin the documented behavior, not
// exact magic numbers — the weights can be re-calibrated without rewriting the suite.

func TestEstimateBasics(t *testing.T) {
	if got := Estimate(""); got != 0 {
		t.Errorf("empty text = %d tokens, want 0", got)
	}
	if got := Estimate("."); got < 1 {
		t.Errorf("non-empty text = %d tokens, want >= 1", got)
	}
	if a, b := Estimate("hello world"), Estimate("hello world"); a != b {
		t.Errorf("estimate is not deterministic: %d vs %d", a, b)
	}

	prose := "The quick brown fox jumps over the lazy dog and keeps going until the sentence has enough length to smooth out rounding."
	got := Estimate(prose)
	perTok := float64(len(prose)) / float64(got)
	if perTok < 3.2 || perTok > 4.8 {
		t.Errorf("prose density = %.2f chars/token, want ~4 (±20%%)", perTok)
	}
}

func TestEstimateCodeIsDenserThanProse(t *testing.T) {
	prose := strings.Repeat("Reading files costs tokens and someone pays for every one of them. ", 20)
	code := strings.Repeat("if err != nil {\n\treturn fmt.Errorf(\"claim %q: %w\", id, err)\n}\n", 20)
	json := strings.Repeat(`{"task_ref":"T-42","tokens_in":18234,"state":"running"},`, 20)

	perTok := func(s string) float64 { return float64(len(s)) / float64(Estimate(s)) }
	if perTok(code) >= perTok(prose) {
		t.Errorf("code %.2f chars/token should be denser than prose %.2f", perTok(code), perTok(prose))
	}
	if perTok(json) >= perTok(prose) {
		t.Errorf("json %.2f chars/token should be denser than prose %.2f", perTok(json), perTok(prose))
	}
}

func TestFileRefusesBinary(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(text, []byte("# Notes\n\nSome ordinary prose.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "blob")
	if err := os.WriteFile(binary, append([]byte("PNG"), 0, 1, 2, 3), 0o644); err != nil {
		t.Fatal(err)
	}

	cost, ok, err := File(text)
	if err != nil || !ok || cost.Tokens <= 0 {
		t.Errorf("text file: cost=%+v ok=%v err=%v, want counted", cost, ok, err)
	}
	if _, ok, err := File(binary); err != nil || ok {
		t.Errorf("binary file: ok=%v err=%v, want refused without error", ok, err)
	}
}

func TestWalkSkipsWhatAgentsNeverRead(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n\nfunc main() {}\n")
	write("docs/guide.md", strings.Repeat("A much longer document about the system. ", 50))
	write(".git/config", "[core]\n")
	write("node_modules/pkg/index.js", "module.exports = 1\n")
	write(".conductor/runtime/worktrees/w1/main.go", "package main\n")
	write(".conductor/policies.yaml", "budget: {}\n")
	write("blob.bin", "x\x00y")

	files, skipped, err := Walk(dir)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[filepath.ToSlash(f.Path)] = true
	}
	for _, want := range []string{"main.go", "docs/guide.md", ".conductor/policies.yaml"} {
		if !got[want] {
			t.Errorf("Walk missed %s; got %v", want, got)
		}
	}
	for _, never := range []string{".git/config", "node_modules/pkg/index.js", ".conductor/runtime/worktrees/w1/main.go"} {
		if got[never] {
			t.Errorf("Walk should have skipped %s", never)
		}
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the binary)", skipped)
	}
	// Largest first, so the expensive read is the first thing a person sees.
	if len(files) > 0 && filepath.ToSlash(files[0].Path) != "docs/guide.md" {
		t.Errorf("first file = %s, want the largest (docs/guide.md)", files[0].Path)
	}
}

func TestInstructionFilesFindsStandingContext(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("AGENTS.md", "# Rules for agents\n")
	write("CLAUDE.md", "# Claude-specific rules\n")
	write("README.md", "# The project\n")
	write(".conductor/WORKFLOW.md", "# Workflow\n")
	write("internal/api/AGENTS.md", "# API-local rules\n")
	// A nested README is not standing context — harnesses only auto-load nested
	// AGENTS.md/CLAUDE.md as the agent moves through the tree.
	write("internal/api/README.md", "# not auto-loaded\n")

	files, err := InstructionFiles(dir)
	if err != nil {
		t.Fatalf("InstructionFiles: %v", err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, filepath.ToSlash(f.Path))
	}
	want := []string{"AGENTS.md", "CLAUDE.md", ".conductor/WORKFLOW.md", "README.md", "internal/api/AGENTS.md"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("instruction files = %v, want %v", paths, want)
	}
	if total := Total(files); total <= 0 {
		t.Errorf("total = %d, want > 0", total)
	}
}

func TestIsBinarySniffsLikeGit(t *testing.T) {
	if isBinary([]byte("plain text, no nulls")) {
		t.Error("plain text misread as binary")
	}
	if !isBinary(append(bytes.Repeat([]byte("a"), 10), 0)) {
		t.Error("NUL byte in head not detected")
	}
	// A NUL beyond the sniff window does not flip the answer; git behaves the same way.
	long := append(bytes.Repeat([]byte("a"), 9000), 0)
	if isBinary(long) {
		t.Error("NUL beyond the 8000-byte sniff window should not mark the file binary")
	}
}
