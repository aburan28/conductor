package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/localstate"
	"github.com/adamburan/conductor/internal/privacy"
)

// ---------------------------------------------------------------------------
// sessions save — selection and the pause/resume decisions
// ---------------------------------------------------------------------------

func TestSelectRecords(t *testing.T) {
	all := []localstate.Record{
		{ID: "3f9c2a1e-0000-4000-8000-000000000001", SessionID: "3f9c2a1e-0000-4000-8000-000000000001", Harness: "claude", PID: 100, Wrapped: true},
		{ID: "3f9c2a1e-0000-4000-8000-000000000002", SessionID: "3f9c2a1e-0000-4000-8000-000000000002", Harness: "codex", PID: 200, Wrapped: true},
		{ID: "p300", Harness: "claude", PID: 300},
	}
	ids := func(rs []localstate.Record) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return strings.Join(out, ",")
	}

	if got, err := selectRecords(all, []string{"all"}, ""); err != nil || len(got) != 3 {
		t.Fatalf("all: %d records, err %v", len(got), err)
	}
	if got, _ := selectRecords(all, []string{"all"}, "claude"); ids(got) != all[0].ID+",p300" {
		t.Errorf("all --harness claude = %s", ids(got))
	}
	// "all" wins even when mixed with ids: the intent is unambiguous.
	if got, _ := selectRecords(all, []string{"300", "all"}, ""); len(got) != 3 {
		t.Errorf("all mixed with ids: got %d, want 3", len(got))
	}

	// By pid, by record id, by session-id prefix — the forms `sessions list` prints.
	got, err := selectRecords(all, []string{"300", "p300", "3f9c2a1e-0000-4000-8000-000000000002"}, "")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if ids(got) != "p300,"+all[1].ID {
		t.Errorf("select = %s (a session named twice must be saved once)", ids(got))
	}

	if _, err := selectRecords(all, []string{"3f9c"}, ""); err == nil {
		t.Error("an ambiguous prefix must be refused, not resolved to the first match")
	}
	if _, err := selectRecords(all, []string{"999"}, ""); err == nil {
		t.Error("an unknown pid must be an error, not an empty save")
	}
	if _, err := selectRecords(all, []string{"300"}, "codex"); err == nil {
		t.Error("--harness must narrow explicit selections too")
	}
	if got, err := selectRecords(nil, []string{"all"}, ""); err != nil || got == nil || len(got) != 0 {
		t.Errorf("saving all of nothing should give an empty list, not nil or an error: %v %v", got, err)
	}
}

func TestPausePlan(t *testing.T) {
	saved := localstate.Record{Saved: true}
	plain := localstate.Record{}
	cases := []struct {
		name                  string
		rec                   localstate.Record
		wrapLive, harnessLive bool
		want                  string
	}{
		{"live wrap goes through the sidecar", saved, true, true, planViaWrap},
		{"bare harness is stopped directly", plain, false, true, planDirect},
		{"wrap died but harness lives: stop directly", saved, false, true, planDirect},
		{"gone and saved: keep for resume", saved, false, false, planKeep},
		{"gone and unsaved: forget", plain, false, false, planForget},
	}
	for _, tc := range cases {
		if got := pausePlan(tc.rec, tc.wrapLive, tc.harnessLive); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestResumePlan(t *testing.T) {
	paused := localstate.Record{Status: localstate.StatusPaused}
	pausedSaved := localstate.Record{Status: localstate.StatusPaused, Saved: true}
	saved := localstate.Record{Status: localstate.StatusSaved, Saved: true}
	cases := []struct {
		name                  string
		rec                   localstate.Record
		wrapLive, harnessLive bool
		want                  string
	}{
		{"paused under a live wrap: wake via sidecar", paused, true, true, planViaWrap},
		{"paused bare: SIGCONT directly", paused, false, true, planDirect},
		{"paused, terminal gone: reopen", paused, false, false, planReopen},
		{"paused and saved, terminal gone: reopen", pausedSaved, false, false, planReopen},
		{"saved, process gone: reopen", saved, false, false, planReopen},
		{"saved but alive again: never signal it", saved, false, true, planRunning},
		{"saved but its wrap is alive: never signal it", saved, true, false, planRunning},
	}
	for _, tc := range cases {
		if got := resumePlan(tc.rec, tc.wrapLive, tc.harnessLive); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// sessions save → terminal closed → resume, against real processes
// ---------------------------------------------------------------------------

// fakeHarnessEnv makes a copy of this test binary behave as an idle harness: it sleeps until
// killed. Copying `sleep` would be simpler, but macOS refuses to run relocated platform
// binaries, and a shebang script shows up in ps as its interpreter, which discovery
// deliberately does not recognize.
const fakeHarnessEnv = "CONDUCTOR_TEST_FAKE_HARNESS"

func TestMain(m *testing.M) {
	if os.Getenv(fakeHarnessEnv) != "" {
		select {}
	}
	// No test in this package may scan the real process table: the scan finds the
	// developer's own agent sessions, and pause would freeze them. Every test works from
	// records it wrote itself.
	discoverSessions = func([]localstate.Record) ([]localstate.Record, error) { return nil, nil }
	os.Exit(m.Run())
}

// A harness stand-in: a real process that `ps` reports under the harness's name, so the
// re-identification that guards every signal (VerifyHarness) sees what it would see in life.
func startFakeHarness(t *testing.T, dir, harness string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not installed")
	}
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	body, err := os.ReadFile(self)
	if err != nil {
		t.Skipf("cannot read the test binary: %v", err)
	}
	path := filepath.Join(dir, harness)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "--model", "x")
	cmd.Env = append(os.Environ(), fakeHarnessEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot run a copy of the test binary as %s: %v", harness, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func recordByID(t *testing.T, id string) (localstate.Record, bool) {
	t.Helper()
	records, err := localstate.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range records {
		if r.ID == id {
			return r, true
		}
	}
	return localstate.Record{}, false
}

// The whole promise, end to end: save the sessions, lose the terminal (here: the process
// dies), and resume reopens each one on the harness's resume invocation, in its directory.
// An unsaved session that dies the same way is forgotten — that is what saving changes.
func TestSaveAllThenResumeReopensLostSessions(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", stateDir)
	binDir := t.TempDir()
	work := t.TempDir()

	claude := startFakeHarness(t, binDir, "claude")
	codex := startFakeHarness(t, binDir, "codex")
	if !localstate.VerifyHarness(claude.Process.Pid, "claude") {
		t.Skip("ps does not report the fake harness under its name on this platform")
	}

	// Two records as `conductor wrap` and discovery would have written them. Discovery
	// itself is skipped here: it wants a controlling tty, and a test has none.
	savedRec := localstate.Record{
		ID: "p" + fmt.Sprint(claude.Process.Pid), Harness: "claude", Command: "claude",
		Args: []string{"--model", "x"}, ResumeArgs: localstate.ResumeInvocation("claude"),
		Cwd: work, PID: claude.Process.Pid, PGID: claude.Process.Pid,
		Status: localstate.StatusRunning, StartedAt: time.Now(),
	}
	unsavedRec := localstate.Record{
		ID: "p" + fmt.Sprint(codex.Process.Pid), Harness: "codex", Command: "codex",
		ResumeArgs: localstate.ResumeInvocation("codex"),
		Cwd:        work, PID: codex.Process.Pid, PGID: codex.Process.Pid,
		Status: localstate.StatusRunning, StartedAt: time.Now(),
	}
	for _, r := range []localstate.Record{savedRec, unsavedRec} {
		if err := localstate.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	// Save only the claude session, by pid — the codex one is the control.
	if err := cmdSessions(context.Background(), []string{"save", fmt.Sprint(claude.Process.Pid), "--json"}); err != nil {
		t.Fatalf("sessions save: %v", err)
	}
	if rec, ok := recordByID(t, savedRec.ID); !ok || !rec.Saved || rec.SavedAt.IsZero() {
		t.Fatalf("save did not pin the record: %+v (found %v)", rec, ok)
	}
	if rec, _ := recordByID(t, unsavedRec.ID); rec.Saved {
		t.Fatal("save pinned a session that was not named")
	}
	// Saving stops nothing.
	if localstate.IsStopped(claude.Process.Pid) {
		t.Fatal("sessions save froze the session; only pause may do that")
	}

	// Nothing to resume while everything is still running.
	if err := cmdResume(context.Background(), []string{"--json"}); err != nil {
		t.Fatalf("resume with nothing to do: %v", err)
	}

	// The terminals go away.
	for _, c := range []*exec.Cmd{claude, codex} {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		_ = c.Wait()
	}
	waitUntil(t, "fake harnesses to die", func() bool {
		return !localstate.Alive(claude.Process.Pid) && !localstate.Alive(codex.Process.Pid)
	})

	// `sessions list` (any command, via Prune) now shows the saved one as saved and has
	// forgotten the other.
	if err := cmdSessions(context.Background(), []string{"list", "--json"}); err != nil {
		t.Fatalf("sessions list: %v", err)
	}
	if rec, ok := recordByID(t, savedRec.ID); !ok || rec.Status != localstate.StatusSaved {
		t.Fatalf("lost saved session should be listed as saved: %+v (found %v)", rec, ok)
	}
	if _, ok := recordByID(t, unsavedRec.ID); ok {
		t.Fatal("an unsaved session that died should have been forgotten")
	}

	// Resume reopens it. CONDUCTOR_TERMINAL stands in for a terminal: it records the shell
	// command a real one would run.
	spawned := filepath.Join(t.TempDir(), "spawned")
	recorder := filepath.Join(binDir, "fake-terminal")
	if err := os.WriteFile(recorder, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> "+spawned+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONDUCTOR_TERMINAL", recorder+" {cmd}")

	if err := cmdResume(context.Background(), []string{"--json"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	var got string
	waitUntil(t, "the terminal to be opened", func() bool {
		b, err := os.ReadFile(spawned)
		got = string(b)
		return err == nil && strings.Contains(got, "claude")
	})
	if !strings.Contains(got, "claude --continue") {
		t.Errorf("reopened command should use the harness's resume invocation; got %q", got)
	}
	if !strings.Contains(got, "cd "+work) {
		t.Errorf("reopened command should start in the session's directory; got %q", got)
	}
	if strings.Contains(got, "codex") {
		t.Errorf("the unsaved session must not be reopened; got %q", got)
	}
	// Ownership passed to the new terminal; the record it came from is gone, so a second
	// resume does not open a second copy.
	if _, ok := recordByID(t, savedRec.ID); ok {
		t.Error("record survived its own reopening; the next resume would duplicate the session")
	}
}

// Pause must not throw away a saved session whose process is gone, and resume must not
// signal a saved session that turns out to be alive.
func TestPauseKeepsSavedSessionsAndResumeLeavesLiveOnesAlone(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	binDir := t.TempDir()

	claude := startFakeHarness(t, binDir, "claude")
	if !localstate.VerifyHarness(claude.Process.Pid, "claude") {
		t.Skip("ps does not report the fake harness under its name on this platform")
	}
	pid := claude.Process.Pid

	// A record whose pid is stale, saved: pause finds nothing to stop and keeps it.
	dead := localstate.Record{ID: "dead", Harness: "claude", PID: 1 << 30, Saved: true,
		Status: localstate.StatusRunning, StartedAt: time.Now()}
	// The same, unsaved: pause forgets it.
	debris := localstate.Record{ID: "debris", Harness: "claude", PID: 1 << 30,
		Status: localstate.StatusRunning, StartedAt: time.Now()}
	// A record that Prune already marked saved but whose pid is, in fact, a live harness
	// again: resume must leave it alone.
	alive := localstate.Record{ID: "alive", Harness: "claude", PID: pid, PGID: pid, Saved: true,
		Status: localstate.StatusSaved, StartedAt: time.Now()}
	for _, r := range []localstate.Record{dead, debris, alive} {
		if err := localstate.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	// pause: the live one gets stopped, the saved-dead one is kept, the debris is dropped.
	if err := cmdPause(context.Background(), []string{"--json"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if rec, ok := recordByID(t, "dead"); !ok || rec.Status != localstate.StatusSaved {
		t.Errorf("pause should keep a saved session with no process, as saved: %+v (found %v)", rec, ok)
	}
	if _, ok := recordByID(t, "debris"); ok {
		t.Error("pause should forget an unsaved record with no process")
	}
	// The "alive" record was saved-status, so pause skipped it: still running, not frozen.
	if localstate.IsStopped(pid) {
		t.Fatal("pause froze a record it should have skipped as saved")
	}

	// A terminal that opens nothing: enough to see that resume reopened the session.
	trueBin, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no `true` on PATH")
	}
	t.Setenv("CONDUCTOR_TERMINAL", trueBin+" {cmd}")
	if err := cmdResume(context.Background(), []string{"--json"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if rec, ok := recordByID(t, "alive"); !ok || rec.Status != localstate.StatusRunning || !rec.Saved {
		t.Errorf("a saved session found alive should be marked running and stay saved: %+v (found %v)", rec, ok)
	}
	if localstate.IsStopped(pid) {
		t.Error("resume signaled a live session it had no reason to touch")
	}
	// The saved-dead one was reopened (into /bin/true) and its record released.
	if _, ok := recordByID(t, "dead"); ok {
		t.Error("resume should have reopened the saved session and released its record")
	}
}

// ---------------------------------------------------------------------------
// sessions export — selection over the control plane's view
// ---------------------------------------------------------------------------

func TestSelectSessions(t *testing.T) {
	all := []privacy.SessionView{
		{ID: "3f9c2a1e-0000-4000-8000-000000000001"},
		{ID: "3f9c2a1e-0000-4000-8000-000000000002"},
		{ID: "7b0d1111-0000-4000-8000-000000000003"},
	}

	got, err := selectSessions(all, []string{"all"})
	if err != nil || len(got) != 3 {
		t.Fatalf("all: got %d sessions, err %v", len(got), err)
	}
	if got, _ := selectSessions(all, []string{"7b0d", "all"}); len(got) != 3 {
		t.Errorf("all mixed with ids: got %d, want 3", len(got))
	}

	got, err = selectSessions(all, []string{"7b0d", "3f9c2a1e-0000-4000-8000-000000000001", "7b0d1111"})
	if err != nil {
		t.Fatalf("prefix select: %v", err)
	}
	if len(got) != 2 || got[0].ID != all[2].ID || got[1].ID != all[0].ID {
		t.Errorf("prefix select = %v", got)
	}

	if _, err := selectSessions(all, []string{"3f9c"}); err == nil {
		t.Error("an ambiguous prefix must be refused, not resolved to the first match")
	}
	if _, err := selectSessions(all, []string{"deadbeef"}); err == nil {
		t.Error("an unknown id must be an error, not an empty export")
	}
	if got, err := selectSessions(nil, []string{"all"}); err != nil || got == nil || len(got) != 0 {
		t.Errorf("exporting all of nothing should give an empty list, not nil or an error: %v %v", got, err)
	}
}

func TestDefaultSessionsFile(t *testing.T) {
	at := time.Date(2026, 8, 25, 14, 3, 5, 0, time.UTC)
	if got, want := defaultSessionsFile("payments", at), "conductor-sessions-payments-20260825T140305Z.json"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
