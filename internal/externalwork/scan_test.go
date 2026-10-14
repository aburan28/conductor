package externalwork

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const sampleID = "12345678-1234-1234-1234-123456789abc"

func TestScanFilesUsesOnlyNamesAndMetadata(t *testing.T) {
	root := t.TempDir()
	transcript := filepath.Join(root, "some-project", "agent-transcripts", sampleID, sampleID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("private conversation that must not be read"), 0o000); err != nil {
		t.Fatal(err)
	}
	got := scanFiles(root, "cursor", time.Now().Add(-time.Hour), func(p string) bool { return filepath.Ext(p) == ".jsonl" })
	if len(got) != 1 || got[0].ID != sampleID || got[0].Title != "" || got[0].Activity != "saved; running state unknown" {
		t.Fatalf("unexpected metadata: %#v", got)
	}
	if got := scanFiles(root, "cursor", time.Now().Add(time.Hour), func(string) bool { return true }); len(got) != 0 {
		t.Fatalf("future cutoff returned %d files", len(got))
	}
}

func TestCodexRowsAndImportIdentifiers(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 unavailable")
	}
	path := filepath.Join(t.TempDir(), "state.sqlite")
	cmd := exec.Command("sqlite3", path, "CREATE TABLE threads (id TEXT,title TEXT,cwd TEXT,updated_at INTEGER,archived INTEGER); INSERT INTO threads VALUES ('"+sampleID+"','private title','/repo',100,0);")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create index: %v: %s", err, out)
	}
	rows, err := codexRows(context.Background(), path)
	if err != nil || len(rows) != 1 || rows[0].ID != sampleID {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	for _, source := range []string{"codex", "claude", "cursor"} {
		if !ValidCandidate(Candidate{Source: source, ID: sampleID}) {
			t.Errorf("rejected %s", source)
		}
	}
	if ValidCandidate(Candidate{Source: "github", ID: sampleID}) || ValidCandidate(Candidate{Source: "codex", ID: "../../bad"}) {
		t.Fatal("accepted unsupported source or ID")
	}
}
