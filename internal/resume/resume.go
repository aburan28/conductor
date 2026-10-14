// Package resume wakes the sessions `conductor pause` froze and reopens the ones
// `conductor sessions save` kept. It is the shareable core behind both
// `conductor resume` and the dashboard's machine-local resume endpoint: after a
// laptop restart (or just a closed lid) every paused or saved Claude Code, Codex,
// and OpenCode session comes back — SIGCONT where the terminal survived, a fresh
// terminal running the harness's own resume invocation where it did not.
package resume

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/aburan28/conductor/internal/localstate"
)

// Action is one session's outcome, shared by the CLI's --json output and the
// dashboard endpoint's response.
type Action struct {
	localstate.Record
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

// Plans: what resume decides to do with one record, given what the process table
// says about it. Kept as pure functions so the decision — the part that must
// never signal the wrong process — is testable without signaling anything.
const (
	PlanViaWrap = "via-wrap" // signal the `conductor wrap` sidecar; it handles its child
	PlanDirect  = "direct"  // signal the harness's process group ourselves
	PlanReopen  = "reopen"  // process gone: open a terminal on the harness's resume invocation
	PlanRunning = "running" // saved record whose process turned out to be alive: leave it be
)

// Plan is the decision for one record; see the constants above.
func Plan(rec localstate.Record, wrapLive, harnessLive bool) string {
	alive := wrapLive || harnessLive
	switch {
	case rec.Status == localstate.StatusSaved && alive:
		return PlanRunning
	case wrapLive:
		return PlanViaWrap
	case harnessLive:
		return PlanDirect
	default:
		return PlanReopen
	}
}

// All wakes every paused or saved session on this machine — the reboot case —
// filtered to one harness when harness is non-empty. It never touches sessions
// that are already running. Errors from one session do not stop the rest; the
// caller sees each outcome in the returned actions.
func All(harness string) ([]Action, error) {
	records, err := localstate.Prune()
	if err != nil {
		return nil, err
	}

	var targets []localstate.Record
	for _, rec := range records {
		resumable := rec.Status == localstate.StatusPaused || rec.Status == localstate.StatusSaved
		if resumable && (harness == "" || rec.Harness == harness) {
			targets = append(targets, rec)
		}
	}
	if len(targets) == 0 {
		return []Action{}, nil
	}

	exe, err := os.Executable()
	if err != nil {
		exe = "conductor"
	}

	var results []Action
	for _, rec := range targets {
		var res Action
		wrapLive := rec.Wrapped && localstate.VerifyWrap(rec.WrapPID)
		harnessLive := localstate.VerifyHarness(rec.PID, rec.Harness)
		switch Plan(rec, wrapLive, harnessLive) {
		case PlanRunning:
			// A saved session found dead a moment ago and alive now: the process is back
			// (or the pid was recycled by another copy of the same harness). Either way there
			// is nothing to wake, and signaling a guess is never right.
			rec.Status = localstate.StatusRunning
			res = Action{Record: rec, Action: "running",
				Detail: fmt.Sprintf("still running in its terminal (%s); nothing to do", orDash(rec.TTY))}

		case PlanViaWrap:
			// The sidecar wakes its child and heartbeats `working` again; the session was in
			// the terminal's foreground the whole time, so input works immediately.
			if err := syscall.Kill(rec.WrapPID, syscall.SIGUSR2); err != nil {
				res = Action{Record: rec, Action: "error", Detail: err.Error()}
				break
			}
			rec.Status = localstate.StatusRunning
			rec.PausedAt = time.Time{}
			res = Action{Record: rec, Action: "resumed",
				Detail: fmt.Sprintf("woken in its terminal (%s)", orDash(rec.TTY))}

		case PlanDirect:
			var contErr error
			if contErr = localstate.ContinueGroup(rec.PGID); contErr != nil {
				contErr = localstate.ContinueProcess(rec.PID)
			}
			if contErr != nil {
				res = Action{Record: rec, Action: "error", Detail: contErr.Error()}
				break
			}
			rec.Status = localstate.StatusRunning
			rec.PausedAt = time.Time{}
			// A bare session was its shell's foreground job; the shell took the terminal back
			// when it stopped, and only the shell can hand it over again.
			res = Action{Record: rec, Action: "resumed",
				Detail: fmt.Sprintf("continued in %s — if the keyboard is dead there, run `fg` in that terminal",
					orDash(rec.TTY))}

		default:
			// Its terminal is gone (or its pid was recycled); reopen one on the harness's own
			// resume invocation. A session that lived in VS Code goes back to VS Code when the
			// companion extension can take it — the record stays paused until the extension
			// actually opens the terminal, so a handoff that goes nowhere is retryable.
			if where, ok := localstate.ResumeInVSCode(rec); ok {
				res = Action{Record: rec, Action: "reopened", Detail: "handed to " + where}
				break
			}
			argv, fresh := relaunchArgv(rec, exe)
			where, err := localstate.OpenTerminal(rec.Harness, rec.Cwd, argv)
			if err != nil {
				res = Action{Record: rec, Action: "error", Detail: err.Error()}
				break
			}
			_ = localstate.Remove(rec.ID) // a relaunched wrap registers its own record
			detail := "reopened in " + where
			if fresh {
				detail += " (fresh session: this harness has no resume invocation)"
			}
			res = Action{Record: rec, Action: "reopened", Detail: detail}
		}

		if res.Action == "resumed" || res.Action == "running" {
			if err := localstate.Save(rec); err != nil {
				return nil, err
			}
		}
		results = append(results, res)
	}
	return results, nil
}

// relaunchArgv builds the command that revives a session whose terminal is gone. Wrapped
// sessions come back under `conductor wrap` — with the same capability flags — so they
// re-register and heartbeat; bare sessions come back bare, matching how the user ran them.
func relaunchArgv(rec localstate.Record, conductorExe string) (argv []string, fresh bool) {
	tail := rec.ResumeArgs
	if len(tail) == 0 {
		// No known resume invocation for this harness: the best available truth is a fresh
		// session with the original arguments, said plainly rather than silently.
		tail, fresh = rec.Args, true
	}
	if rec.Wrapped {
		argv = append(argv, conductorExe, "wrap")
		argv = append(argv, rec.WrapFlags...)
		argv = append(argv, rec.Harness)
		return append(argv, tail...), fresh
	}
	command := rec.Command
	if command == "" {
		command = rec.Harness
	}
	return append([]string{command}, tail...), fresh
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
