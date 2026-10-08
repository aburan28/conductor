//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// A failing fetch-wal must end the process with SIGKILL, not exit 1. The child runs the real
// command with no bucket configured, so every attempt fails with an error that is not "not
// archived".
func TestFetchWALFailureKillsTheProcess(t *testing.T) {
	if os.Getenv("CONDUCTOR_TEST_FETCH_WAL") == "1" {
		_ = dbFetchWAL(context.Background(), []string{"000000010000000000000001", filepath.Join(os.TempDir(), "unused")})
		os.Exit(0) // not reached: the failure above kills the process
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFetchWALFailureKillsTheProcess$")
	cmd.Env = []string{"CONDUCTOR_TEST_FETCH_WAL=1", "HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("the child did not fail as expected: %v\n%s", err, out)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		t.Fatalf("the child exited with status %d, not a signal; Postgres reads 1 as the end of the archive\n%s", ee.ExitCode(), out)
	}
	if ws.Signal() != syscall.SIGKILL {
		t.Fatalf("the child was killed by %v; want SIGKILL\n%s", ws.Signal(), out)
	}
}
