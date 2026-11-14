package externalwork

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const sampleID = "12345678-1234-1234-1234-123456789abc"

func TestCodexRootsDiscoversLiteralMountedDirectories(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	volumes := filepath.Join(base, "Volumes")
	for _, name := range []string{"SSD990", "SSD990-2"} {
		if err := os.MkdirAll(filepath.Join(volumes, name, ".codex"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The .codex path is searchable even when listing the volume root is denied.
	closedRoot := filepath.Join(volumes, "SSD990")
	if err := os.Chmod(closedRoot, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(closedRoot, 0o700) })
	if err := os.MkdirAll(filepath.Join(volumes, "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(volumes, "SSD990", ".codex")
	want := []string{custom, filepath.Join(home, ".codex"), filepath.Join(volumes, "SSD990-2", ".codex")}
	if got := codexRoots(context.Background(), home, custom, volumes); !reflect.DeepEqual(got, want) {
		t.Fatalf("roots=%v, want %v", got, want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := codexRoots(ctx, home, custom, volumes); !reflect.DeepEqual(got, want[:2]) {
		t.Fatalf("canceled roots=%v, want %v", got, want[:2])
	}
}

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
	cmd := exec.Command("sqlite3", path, "CREATE TABLE threads (id TEXT,title TEXT,cwd TEXT,updated_at INTEGER,archived INTEGER); INSERT INTO threads VALUES ('"+sampleID+"','private title','/repo',200,0); INSERT INTO threads VALUES ('aaaaaaaa-1234-1234-1234-123456789abc','old title','/repo',100,0);")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create index: %v: %s", err, out)
	}
	rows, err := codexRows(context.Background(), path, time.Unix(150, 0))
	if err != nil || len(rows) != 1 || rows[0].ID != sampleID {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	all, err := codexRows(context.Background(), path, time.Time{})
	if err != nil || len(all) != 2 || all[0].ID != sampleID {
		t.Fatalf("unbounded rows=%#v err=%v", all, err)
	}
	empty, err := codexRows(context.Background(), path, time.Unix(201, 0))
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("zero-row query must be an empty result: rows=%#v err=%v", empty, err)
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

func TestParseResumeProcessesRequiresExactOwnedInvocation(t *testing.T) {
	ps := `101 501 /opt/bin/codex /opt/bin/codex resume 12345678-1234-1234-1234-123456789abc
102 501 claude claude --resume 12345678-1234-1234-1234-123456789abc
103 501 claude claude --resume=ABCDEFAB-1234-1234-1234-123456789ABC
104 502 codex codex resume 22222222-2222-2222-2222-222222222222
105 501 bash bash -c codex resume 33333333-3333-3333-3333-333333333333
106 501 codex codex exec --json 44444444-4444-4444-4444-444444444444
107 501 claude claude -p --resume 55555555-5555-5555-5555-555555555555
108 501 cursor cursor --resume 66666666-6666-6666-6666-666666666666
109 501 codex codex resume --last
`
	got := parseResumeProcesses(ps, 501)
	if len(got) != 3 || got["codex:"+sampleID] != 101 || got["claude:"+sampleID] != 102 || got["claude:abcdefab-1234-1234-1234-123456789abc"] != 103 {
		t.Fatalf("unexpected exact process matches: %#v", got)
	}
}

func TestMarkProcessAliveAtScanDoesNotInferOtherSavedWork(t *testing.T) {
	candidates := []Candidate{
		{Source: "codex", ID: "ABCDEFAB-1234-1234-1234-123456789ABC", Activity: "saved; running state unknown"},
		{Source: "cursor", ID: sampleID, Activity: "saved; running state unknown"},
	}
	markProcessAliveAtScan(candidates, map[string]int{"codex:abcdefab-1234-1234-1234-123456789abc": 101})
	if !candidates[0].ProcessAliveAtScan || candidates[0].ProcessID != 101 || candidates[0].Activity != "session process observed at scan; task state unknown" {
		t.Fatalf("matching live session not annotated: %#v", candidates[0])
	}
	if candidates[1].ProcessAliveAtScan || candidates[1].ProcessID != 0 || candidates[1].Activity != "saved; running state unknown" {
		t.Fatalf("unmatched saved record was promoted: %#v", candidates[1])
	}
}
