package localstate

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordRoundTrip(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())

	want := Record{
		ID: "abc-123", Harness: "claude", Command: "claude",
		Args: []string{"--model", "claude-opus-5"}, ResumeArgs: []string{"--continue"},
		WrapFlags: []string{"--effort", "high"},
		Cwd:       "/home/alice/repo", PID: 41231, PGID: 41231, WrapPID: 41230,
		TTY: "pts/3", TermProgram: "vscode",
		SessionID: "abc-123", Project: "myrepo", Wrapped: true,
		Status: StatusRunning, StartedAt: time.Now().Round(time.Second),
	}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	records, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("List returned %d records, want 1", len(records))
	}
	got := records[0]
	if got.ID != want.ID || got.Harness != want.Harness || got.PID != want.PID ||
		got.Status != want.Status || got.TTY != want.TTY || !got.Wrapped {
		t.Errorf("round trip mangled the record: got %+v", got)
	}
	if len(got.ResumeArgs) != 1 || got.ResumeArgs[0] != "--continue" {
		t.Errorf("ResumeArgs = %v, want [--continue]", got.ResumeArgs)
	}
	if len(got.WrapFlags) != 2 || got.WrapFlags[0] != "--effort" {
		t.Errorf("WrapFlags = %v, want [--effort high]", got.WrapFlags)
	}
	if got.TermProgram != "vscode" {
		t.Errorf("TermProgram = %q, want vscode", got.TermProgram)
	}

	if err := Remove(want.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	records, _ = List()
	if len(records) != 0 {
		t.Errorf("record survived Remove: %v", records)
	}
	// Removing again must not error: the wrap sidecar and the pause CLI can race to clean up.
	if err := Remove(want.ID); err != nil {
		t.Errorf("second Remove: %v", err)
	}
}

func TestRecordsAreOwnerPrivate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", dir)

	// An argv can contain a prompt, and a prompt is owner-private (DESIGN.md §12).
	if err := Save(Record{ID: "x", Harness: "claude", Args: []string{"fix the login bug"}, PID: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "sessions", "x.json"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("record mode = %o, want 600", perm)
	}
}

func TestListSkipsCorruptRecords(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", dir)

	if err := Save(Record{ID: "good", Harness: "codex", PID: 2}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions", "bad.json"), []byte("{trunca"), 0o600); err != nil {
		t.Fatalf("write corrupt record: %v", err)
	}

	records, err := List()
	if err != nil {
		t.Fatalf("one damaged record must not make every session unlistable: %v", err)
	}
	if len(records) != 1 || records[0].ID != "good" {
		t.Errorf("List = %+v, want just the good record", records)
	}
}

func TestFileNameCannotEscapeDirectory(t *testing.T) {
	if got := fileName("../../etc/passwd"); got != "______etc_passwd.json" {
		t.Errorf("fileName(../../etc/passwd) = %q; path separators must not survive", got)
	}
}

func TestPruneKeepsPausedDeadSessions(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())

	deadPID := reapedPID(t)
	// A paused session whose terminal was closed is exactly what resume needs to reopen.
	pausedDead := Record{ID: "paused-dead", Harness: "claude", PID: deadPID, WrapPID: deadPID,
		Wrapped: true, Status: StatusPaused, StartedAt: time.Now().Add(-2 * time.Hour)}
	// A running record whose process is gone is debris from a crash.
	runningDead := Record{ID: "running-dead", Harness: "codex", PID: deadPID,
		Status: StatusRunning, StartedAt: time.Now().Add(-time.Hour)}
	// A running record whose process is alive stays.
	runningLive := Record{ID: "running-live", Harness: "opencode", PID: os.Getpid(),
		Status: StatusRunning, StartedAt: time.Now()}
	for _, r := range []Record{pausedDead, runningDead, runningLive} {
		if err := Save(r); err != nil {
			t.Fatalf("Save(%s): %v", r.ID, err)
		}
	}

	live, err := Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range live {
		ids[r.ID] = true
	}
	if !ids["paused-dead"] {
		t.Error("Prune dropped a paused session with a dead process; resume can never reopen it now")
	}
	if ids["running-dead"] {
		t.Error("Prune kept a running record with no process behind it")
	}
	if !ids["running-live"] {
		t.Error("Prune dropped a live session")
	}
}

func TestResumeInvocation(t *testing.T) {
	cases := map[string][]string{
		"claude":   {"--continue"},
		"opencode": {"--continue"},
		"codex":    {"resume", "--last"},
		"aider":    nil, // unknown harness: no resume invocation, caller relaunches fresh
	}
	for harness, want := range cases {
		got := ResumeInvocation(harness)
		if len(got) != len(want) {
			t.Errorf("ResumeInvocation(%s) = %v, want %v", harness, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("ResumeInvocation(%s) = %v, want %v", harness, got, want)
			}
		}
	}
}

// reapedPID returns the pid of a process that has already exited and been reaped, i.e. a pid
// that is definitely not alive right now.
func reapedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("waiting for helper: %v", err)
	}
	return pid
}
