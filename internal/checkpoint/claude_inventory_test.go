package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryClaudeKeepsDistinctSameCwdSessions(t *testing.T) {
	root := t.TempDir()
	write := func(project, id, title string) string {
		t.Helper()
		path := filepath.Join(root, "projects", project, id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		lines := `{"type":"user","sessionId":"` + id + `","cwd":"/home/me/repo","timestamp":"2026-10-16T18:00:00Z","message":{"role":"user","content":"continue"}}` + "\n" +
			`{"type":"custom-title","sessionId":"` + id + `","customTitle":"` + title + `","timestamp":"2026-10-16T18:01:00Z"}` + "\n"
		if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	first := write("-home-me-repo", "first", "First")
	second := write("ssh-remote", "second", "Remote second")
	if err := os.WriteFile(filepath.Join(filepath.Dir(second), "agent-child.jsonl"),
		[]byte(`{"type":"assistant","sessionId":"second"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := InventoryClaude(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	byID := map[string]ClaudeInventoryRow{}
	for _, row := range rows {
		byID[row.SessionID] = row
		if row.Cwd != "/home/me/repo" || row.SHA256 == "" || row.Problem != "" {
			t.Fatalf("incomplete row: %+v", row)
		}
	}
	if byID["first"].Path != first || byID["second"].Path != second ||
		byID["first"].RemoteMirror || !byID["second"].RemoteMirror ||
		byID["second"].Title != "Remote second" {
		t.Fatalf("wrong session identity: %+v", byID)
	}
	selected, err := InventoryClaude(root, "second")
	if err != nil || len(selected) != 1 || selected[0].SessionID != "second" {
		t.Fatalf("selected = %+v, %v", selected, err)
	}
}

func TestInventoryClaudeReportsInvalidRowWithoutDroppingOthers(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "one")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for id, embedded := range map[string]string{"good": "good", "bad": "other"} {
		body := `{"type":"user","sessionId":"` + embedded + `","cwd":"/repo"}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := InventoryClaude(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	byID := map[string]ClaudeInventoryRow{}
	for _, row := range rows {
		byID[row.SessionID] = row
	}
	if byID["good"].Problem != "" || !strings.Contains(byID["bad"].Problem, "no matching") {
		t.Fatalf("invalid source was silently dropped or accepted: %+v", byID)
	}
}

func TestInventoryClaudeAcceptsForkedHistoryWithItsOwnSessionID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "fork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","sessionId":"parent","cwd":"/repo"}` + "\n" +
		`{"type":"assistant","sessionId":"child","cwd":"/repo"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "child.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := InventoryClaude(root, "child")
	if err != nil || len(rows) != 1 || rows[0].Problem != "" ||
		len(rows[0].EmbeddedSessionIDs) != 1 || rows[0].EmbeddedSessionIDs[0] != "parent" {
		t.Fatalf("forked history rejected: %+v, %v", rows, err)
	}
}

func TestInventoryClaudeReportsOversizedSidecar(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "one")
	if err := os.MkdirAll(filepath.Join(dir, "session"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"),
		[]byte(`{"type":"user","sessionId":"session","cwd":"/repo"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "session", "tool-result.txt")
	f, err := os.Create(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxUntrackedFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err := InventoryClaude(root, "session")
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].Problem, "exceeds checkpoint file limit") {
		t.Fatalf("oversized sidecar silently omitted: %+v, %v", rows, err)
	}
}
