package localstate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Signaling. Pause is SIGSTOP and resume is SIGCONT — the only pair that freezes a TUI
// mid-thought without asking it to cooperate and wakes it with everything intact. Neither can
// be caught, which is the point: a harness needs no pause support to be pausable.

// Alive reports whether a process exists. EPERM means it exists but is not ours to signal,
// which for liveness is still "alive".
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// IsStopped reports whether a process is currently stopped (SIGSTOP/SIGTSTP). Both Linux and
// macOS render that state as a leading 'T' in ps.
func IsStopped(pid int) bool {
	out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "T")
}

// StopProcess freezes a single process.
func StopProcess(pid int) error { return signalPID(pid, syscall.SIGSTOP) }

// ContinueProcess wakes a single process.
func ContinueProcess(pid int) error { return signalPID(pid, syscall.SIGCONT) }

// StopGroup freezes a whole process group — the harness and whatever it spawned.
func StopGroup(pgid int) error { return signalGroup(pgid, syscall.SIGSTOP) }

// ContinueGroup wakes a whole process group.
func ContinueGroup(pgid int) error { return signalGroup(pgid, syscall.SIGCONT) }

func signalPID(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to signal pid %d", pid)
	}
	return syscall.Kill(pid, sig)
}

func signalGroup(pgid int, sig syscall.Signal) error {
	// kill(-1, SIGSTOP) would freeze every process this user owns. A zero or negative pgid
	// from a corrupt record must be an error, never a broadcast.
	if pgid <= 1 {
		return fmt.Errorf("refusing to signal process group %d", pgid)
	}
	return syscall.Kill(-pgid, sig)
}

// VerifyHarness reports whether pid is still the harness process a record described. Pids are
// recycled; between saving a record and signaling it, the process is re-identified, because a
// SIGSTOP delivered to a stranger is a denial of service on whatever it hit.
func VerifyHarness(pid int, harness string) bool {
	info, ok := processInfo(pid)
	if !ok {
		return false
	}
	name, _, ok := matchHarness(info.args)
	return ok && name == harness
}

// VerifyWrap reports whether pid still looks like a `conductor wrap` sidecar. A false
// negative merely downgrades to reopening a fresh terminal; a false positive would send
// SIGUSR1 to a stranger, so the check errs toward strict.
func VerifyWrap(pid int) bool {
	info, ok := processInfo(pid)
	if !ok || len(info.args) < 2 {
		return false
	}
	return strings.Contains(filepath.Base(info.args[0]), "conductor") && info.args[1] == "wrap"
}

// CurrentTTY names this process's controlling terminal ("pts/3", "ttys004"), or "" when
// there is none.
func CurrentTTY() string {
	if link, err := os.Readlink("/proc/self/fd/0"); err == nil && strings.HasPrefix(link, "/dev/") {
		return strings.TrimPrefix(link, "/dev/")
	}
	if info, ok := processInfo(os.Getpid()); ok && hasTTY(info.tty) {
		return info.tty
	}
	return ""
}

func processInfo(pid int) (procInfo, bool) {
	if pid <= 0 {
		return procInfo{}, false
	}
	out, err := exec.Command("ps", "-ww", "-o", "pid=,pgid=,ppid=,uid=,tty=,args=",
		"-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return procInfo{}, false
	}
	for _, p := range parsePSOutput(string(out)) {
		if p.pid == pid {
			return p, true
		}
	}
	return procInfo{}, false
}
