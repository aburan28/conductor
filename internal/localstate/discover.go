package localstate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Discovery finds the interactive agent sessions that were launched bare, without
// `conductor wrap`. People do that — wrap is a habit, not a requirement — and a pause command
// that silently skipped half the terminals would be worse than none. Discovery reads the
// process table; it never reads another process's memory, environment, or file descriptors
// beyond its working directory.

type procInfo struct {
	pid, pgid, ppid, uid int
	tty                  string
	args                 []string
}

// knownHarnesses are the interactive TUIs discovery recognizes. A harness launched through
// `conductor wrap` is tracked whatever its name; this list only bounds the bare-process scan,
// where guessing wrong would freeze something that is not a coding agent.
var knownHarnesses = map[string]bool{"claude": true, "codex": true, "opencode": true}

// interpreters are runtimes a harness may be launched through (`node …/claude`). The script
// argument, not the runtime, names the harness.
var interpreters = map[string]bool{
	"node": true, "bun": true, "deno": true, "python": true, "python3": true,
}

// Discover scans the process table for interactive harness sessions this user is running in
// terminals, excluding anything already covered by a known record (or descended from one).
func Discover(known []Record) ([]Record, error) {
	// -ww: never truncate args. args= last, because it is the only field with spaces.
	out, err := exec.Command("ps", "-axww", "-o", "pid=,pgid=,ppid=,uid=,tty=,args=").Output()
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	records := discoverFrom(string(out), os.Getpid(), os.Getuid(), known)
	for i := range records {
		records[i].Cwd = cwdOf(records[i].PID)
	}
	return records, nil
}

// discoverFrom is the pure core of Discover, operating on captured ps output.
func discoverFrom(psOutput string, selfPID, uid int, known []Record) []Record {
	procs := parsePSOutput(psOutput)

	parent := make(map[int]int, len(procs))
	for _, p := range procs {
		parent[p.pid] = p.ppid
	}

	knownPID := make(map[int]bool, len(known)*2)
	for _, r := range known {
		if r.PID > 0 {
			knownPID[r.PID] = true
		}
		if r.WrapPID > 0 {
			knownPID[r.WrapPID] = true
		}
	}

	// Everything above this command in the process tree — its shell, its terminal, possibly
	// an agent that invoked `conductor pause` itself — must never be frozen: stopping an
	// ancestor stops us.
	selfChain := make(map[int]bool)
	for pid, depth := selfPID, 0; pid > 1 && depth < 128; depth++ {
		selfChain[pid] = true
		next, ok := parent[pid]
		if !ok || next == pid {
			break
		}
		pid = next
	}

	var candidates []procInfo
	for _, p := range procs {
		if p.uid != uid || !hasTTY(p.tty) {
			continue
		}
		harness, rest, ok := matchHarness(p.args)
		if !ok || nonInteractive(harness, rest) {
			continue
		}
		if knownPID[p.pid] || selfChain[p.pid] {
			continue
		}
		// A subprocess of a wrapped session belongs to that session's record.
		if chainContains(p.ppid, parent, knownPID) {
			continue
		}
		candidates = append(candidates, p)
	}

	// An agent can spawn another copy of its own harness; the outermost process is the
	// session, and stopping it stops the whole tree's terminal anyway.
	candidatePID := make(map[int]bool, len(candidates))
	for _, c := range candidates {
		candidatePID[c.pid] = true
	}
	var out []Record
	for _, c := range candidates {
		if chainContains(c.ppid, parent, candidatePID) {
			continue
		}
		harness, rest, _ := matchHarness(c.args)
		out = append(out, Record{
			ID:         fmt.Sprintf("p%d", c.pid),
			Harness:    harness,
			Command:    harness,
			Args:       append([]string(nil), rest...),
			ResumeArgs: ResumeInvocation(harness),
			PID:        c.pid,
			PGID:       c.pgid,
			TTY:        c.tty,
			Status:     StatusRunning,
			StartedAt:  time.Now(),
		})
	}
	return out
}

func parsePSOutput(out string) []procInfo {
	var procs []procInfo
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		pgid, err2 := strconv.Atoi(fields[1])
		ppid, err3 := strconv.Atoi(fields[2])
		uid, err4 := strconv.Atoi(fields[3])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		procs = append(procs, procInfo{pid, pgid, ppid, uid, fields[4], fields[5:]})
	}
	return procs
}

// matchHarness identifies which harness (if any) an argv is running, and returns the
// arguments that follow the harness itself.
func matchHarness(args []string) (string, []string, bool) {
	if len(args) == 0 {
		return "", nil, false
	}
	base := filepath.Base(args[0])
	if knownHarnesses[base] {
		return base, args[1:], true
	}
	if !interpreters[base] {
		return "", nil, false
	}
	for i, a := range args[1:] {
		if strings.HasPrefix(a, "-") {
			continue // interpreter flags before the script
		}
		if b := filepath.Base(a); knownHarnesses[b] {
			return b, args[i+2:], true
		}
		break
	}
	return "", nil, false
}

// nonInteractive reports whether an invocation is not a resumable terminal session: one-shot
// executions, servers, and utility subcommands. There is no conversation to come back to in a
// `codex exec` run, and a runner-driven attempt belongs to its lease, not to a keyboard.
func nonInteractive(harness string, rest []string) bool {
	switch harness {
	case "claude":
		for _, a := range rest {
			if a == "-p" || a == "--print" {
				return true
			}
		}
		return firstPositional(rest) == "mcp"
	case "codex":
		switch firstPositional(rest) {
		case "exec", "mcp", "app-server", "login", "logout", "apply", "completion", "debug":
			return true
		}
	case "opencode":
		switch firstPositional(rest) {
		case "run", "serve", "auth", "upgrade", "generate", "github":
			return true
		}
	}
	return false
}

func firstPositional(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

func hasTTY(tty string) bool {
	switch tty {
	case "", "?", "??", "-":
		return false
	}
	return true
}

// chainContains walks pid's ancestry and reports whether any ancestor (or pid itself) is in
// set. Depth-capped so a corrupt snapshot with a ppid cycle cannot spin.
func chainContains(pid int, parent map[int]int, set map[int]bool) bool {
	for depth := 0; pid > 1 && depth < 128; depth++ {
		if set[pid] {
			return true
		}
		next, ok := parent[pid]
		if !ok || next == pid {
			return false
		}
		pid = next
	}
	return false
}

// cwdOf finds a process's working directory: /proc where it exists, lsof elsewhere. Best
// effort — an empty cwd degrades resume to "reopen in the home directory", not to failure.
func cwdOf(pid int) string {
	if link, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		return link
	}
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "n"); ok {
			return rest
		}
	}
	return ""
}
