package localstate

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStopAndContinueGroup exercises the actual pause mechanics against a real process:
// SIGSTOP freezes it without its cooperation, SIGCONT wakes it with everything intact.
func TestStopAndContinueGroup(t *testing.T) {
	requirePS(t)

	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	if IsStopped(pid) {
		t.Fatal("sleep is stopped before anyone stopped it")
	}
	if err := StopGroup(pid); err != nil {
		t.Fatalf("StopGroup: %v", err)
	}
	waitFor(t, "process to stop", func() bool { return IsStopped(pid) })

	if err := ContinueGroup(pid); err != nil {
		t.Fatalf("ContinueGroup: %v", err)
	}
	waitFor(t, "process to wake", func() bool { return !IsStopped(pid) })
}

func TestAliveDistinguishesLiveFromReaped(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("Alive(self) = false")
	}

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if Alive(pid) {
		t.Errorf("Alive(%d) = true for a reaped process", pid)
	}
}

// A corrupt or zero pgid must be an error, never a broadcast: kill(-1, SIGSTOP) would freeze
// every process this user owns, including the terminal they would use to recover.
func TestSignalGroupRefusesDangerousTargets(t *testing.T) {
	for _, pgid := range []int{0, 1, -1, -42} {
		if err := StopGroup(pgid); err == nil {
			t.Errorf("StopGroup(%d) did not refuse", pgid)
		}
		if err := ContinueGroup(pgid); err == nil {
			t.Errorf("ContinueGroup(%d) did not refuse", pgid)
		}
	}
	for _, pid := range []int{0, 1, -1} {
		if err := StopProcess(pid); err == nil {
			t.Errorf("StopProcess(%d) did not refuse", pid)
		}
	}
}

func TestVerifyHarnessRejectsRecycledPids(t *testing.T) {
	requirePS(t)

	// This test binary is not claude, whatever pid it got.
	if VerifyHarness(os.Getpid(), "claude") {
		t.Error("VerifyHarness matched the test binary as claude")
	}
	if VerifyHarness(reapedPID(t), "claude") {
		t.Error("VerifyHarness matched a dead pid")
	}

	// A real process that IS the named harness must verify: sleep standing in for a harness
	// via an argv whose basename matches.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if VerifyHarness(cmd.Process.Pid, "claude") {
		t.Error("VerifyHarness matched sleep as claude")
	}
}

func TestVerifyWrapRejectsStrangers(t *testing.T) {
	requirePS(t)
	if VerifyWrap(os.Getpid()) {
		t.Error("VerifyWrap matched the test binary")
	}
	if VerifyWrap(0) || VerifyWrap(-1) {
		t.Error("VerifyWrap matched a non-pid")
	}
}

func requirePS(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not installed")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, _ := exec.Command("ps", "-o", "pid=,state=,args=", "-A").Output()
	t.Fatalf("timed out waiting for %s; ps:\n%s", what, firstLines(string(out), 20))
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
