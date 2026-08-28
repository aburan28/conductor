package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/adamburan/conductor/internal/localstate"
)

// ---------------------------------------------------------------------------
// pause / resume
// ---------------------------------------------------------------------------
//
// A person running three agents has three terminals. Standing up from that desk — a meeting,
// a laptop lid, an office move — should be one command, and sitting back down should be one
// command, even if some of those terminals no longer exist by then.
//
// `conductor pause` saves and freezes; `conductor resume` wakes in place where the terminal
// survived and reopens a terminal where it did not, using each harness's own
// conversation-resume invocation. The transcript never passes through Conductor: every
// harness reopens its own conversation from its own local state.

// pauseAction is one session's outcome, shared by both commands' --json output.
type pauseAction struct {
	localstate.Record
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

func cmdPause(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pause", flag.ExitOnError)
	harness := fs.String("harness", "", "pause only this harness (claude, codex, opencode, …)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor pause — freeze the agent sessions running in your terminals

Finds every interactive Claude Code, Codex, and OpenCode session on this machine — launched
through `+"`conductor wrap`"+` or bare — saves how to revive each one, and stops it with
SIGSTOP. The terminals stay open, frozen mid-thought. Wrapped sessions keep heartbeating as
waiting_for_input, so teammates see a parked session rather than a vanished one.

`+"`conductor resume`"+` wakes everything; terminals that were closed meanwhile are reopened.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	records, err := localstate.Prune()
	if err != nil {
		return err
	}
	discovered, err := discoverSessions(records)
	if err != nil {
		// Wrapped sessions are still pausable from their records; say what the scan could not do.
		fmt.Fprintf(os.Stderr, "conductor: cannot scan for unwrapped sessions: %v\n", err)
	}
	for _, rec := range discovered {
		if err := localstate.Save(rec); err != nil {
			return err
		}
		records = append(records, rec)
	}

	var results []pauseAction
	already, saved := 0, 0
	for _, rec := range records {
		if *harness != "" && rec.Harness != *harness {
			continue
		}
		switch rec.Status {
		case localstate.StatusPaused:
			already++
			continue
		case localstate.StatusSaved:
			// Its process is already gone; there is nothing to freeze, and the record must
			// stay so `conductor resume` can reopen it.
			saved++
			continue
		}
		var sigErr error
		var detail string
		wrapLive := rec.Wrapped && localstate.VerifyWrap(rec.WrapPID)
		harnessLive := localstate.VerifyHarness(rec.PID, rec.Harness)
		switch pausePlan(rec, wrapLive, harnessLive) {
		case planViaWrap:
			// The sidecar stops the harness child and flips its heartbeat, so the pause is
			// visible to the team, not just to this machine.
			sigErr = syscall.Kill(rec.WrapPID, syscall.SIGUSR1)
			detail = "via conductor wrap"
		case planDirect:
			// No live sidecar (bare launch, or the wrap died): stop the group directly so the
			// harness and whatever it spawned freeze together.
			if sigErr = localstate.StopGroup(rec.PGID); sigErr != nil {
				sigErr = localstate.StopProcess(rec.PID)
			}
			detail = "stopped directly"
		case planKeep:
			// Gone since the prune a moment ago, but saved: keep it for resume, as saved.
			rec.Status = localstate.StatusSaved
			if err := localstate.Save(rec); err != nil {
				return err
			}
			saved++
			continue
		default:
			// Live moments ago, unrecognizable now: it exited, or the pid was recycled.
			// Never signal a guess.
			_ = localstate.Remove(rec.ID)
			continue
		}
		if sigErr != nil {
			results = append(results, pauseAction{Record: rec, Action: "error", Detail: sigErr.Error()})
			continue
		}
		rec.Status = localstate.StatusPaused
		rec.PausedAt = time.Now()
		if err := localstate.Save(rec); err != nil {
			return err
		}
		results = append(results, pauseAction{Record: rec, Action: "paused", Detail: detail})
	}

	if *asJSON {
		if results == nil {
			results = []pauseAction{}
		}
		return emit(results)
	}
	if len(results) == 0 {
		switch {
		case already > 0:
			fmt.Printf("Nothing running to pause; %d session(s) already paused. `conductor resume` wakes them.\n", already)
		case saved > 0:
			fmt.Printf("Nothing running to pause; %d saved session(s) are waiting for `conductor resume` to reopen them.\n", saved)
		default:
			fmt.Println("No live Claude Code, Codex, or OpenCode sessions found in your terminals.")
		}
		return nil
	}

	paused := 0
	for _, r := range results {
		mark := "paused"
		if r.Action == "error" {
			mark = "FAILED"
		} else {
			paused++
		}
		fmt.Printf("  %-7s %-10s %-9s %s  %s\n",
			mark, r.Harness, orDash(r.TTY), shortPath(r.Cwd), describeRecord(r.Record))
		if r.Action == "error" {
			fmt.Printf("          %s\n", r.Detail)
		}
	}
	if already > 0 {
		fmt.Printf("\n  (%d session(s) were already paused.)\n", already)
	}
	if saved > 0 {
		fmt.Printf("\n  (%d saved session(s) have no process to pause; resume reopens them.)\n", saved)
	}
	fmt.Printf("\nPaused %d session(s). `conductor resume` wakes them — and reopens any terminal you close.\n", paused)
	return nil
}

func cmdResume(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	harness := fs.String("harness", "", "resume only this harness (claude, codex, opencode, …)")
	list := fs.Bool("list", false, "show saved sessions without waking anything")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor resume — wake the sessions `+"`conductor pause`"+` froze, and reopen the ones `+"`conductor sessions save`"+` kept

Each paused session comes back where it was: SIGCONT in its own terminal when that terminal
still exists, or a fresh terminal otherwise. A saved session whose terminal is gone — closed,
or lost to a reboot — is reopened the same way; one that is still running is left alone. A
fresh terminal is a VS Code integrated terminal when the session lived in VS Code and the
Conductor extension is installed, else a tmux window, the platform's terminal app, or a
detached tmux session — running the harness's own resume invocation
(claude --continue, codex resume --last, opencode --continue). Set CONDUCTOR_TERMINAL to
choose the terminal, e.g. CONDUCTOR_TERMINAL="kitty --directory {cwd} sh -c {cmd}".

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	// On a fresh machine (a replaced cloud instance) there are no local records to resume;
	// pull this machine's records back from S3 first, when off-host backup is configured.
	maybeRestoreBeforeResume(ctx)

	records, err := localstate.Prune()
	if err != nil {
		return err
	}

	if *list {
		if *asJSON {
			return emit(records)
		}
		if len(records) == 0 {
			fmt.Println("No saved sessions.")
			return nil
		}
		printRecords(records)
		return nil
	}

	var targets []localstate.Record
	for _, rec := range records {
		resumable := rec.Status == localstate.StatusPaused || rec.Status == localstate.StatusSaved
		if resumable && (*harness == "" || rec.Harness == *harness) {
			targets = append(targets, rec)
		}
	}
	if len(targets) == 0 {
		if *asJSON {
			return emit([]pauseAction{})
		}
		fmt.Println("Nothing is paused or saved. `conductor pause` freezes the live sessions; `conductor sessions save all` keeps them resumable.")
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		exe = "conductor"
	}

	var results []pauseAction
	for _, rec := range targets {
		var res pauseAction
		wrapLive := rec.Wrapped && localstate.VerifyWrap(rec.WrapPID)
		harnessLive := localstate.VerifyHarness(rec.PID, rec.Harness)
		switch resumePlan(rec, wrapLive, harnessLive) {
		case planRunning:
			// A saved session found dead a moment ago and alive now: the process is back
			// (or the pid was recycled by another copy of the same harness). Either way there
			// is nothing to wake, and signaling a guess is never right.
			rec.Status = localstate.StatusRunning
			res = pauseAction{Record: rec, Action: "running",
				Detail: fmt.Sprintf("still running in its terminal (%s); nothing to do", orDash(rec.TTY))}

		case planViaWrap:
			// The sidecar wakes its child and heartbeats `working` again; the session was in
			// the terminal's foreground the whole time, so input works immediately.
			if err := syscall.Kill(rec.WrapPID, syscall.SIGUSR2); err != nil {
				res = pauseAction{Record: rec, Action: "error", Detail: err.Error()}
				break
			}
			rec.Status = localstate.StatusRunning
			rec.PausedAt = time.Time{}
			res = pauseAction{Record: rec, Action: "resumed",
				Detail: fmt.Sprintf("woken in its terminal (%s)", orDash(rec.TTY))}

		case planDirect:
			var contErr error
			if contErr = localstate.ContinueGroup(rec.PGID); contErr != nil {
				contErr = localstate.ContinueProcess(rec.PID)
			}
			if contErr != nil {
				res = pauseAction{Record: rec, Action: "error", Detail: contErr.Error()}
				break
			}
			rec.Status = localstate.StatusRunning
			rec.PausedAt = time.Time{}
			// A bare session was its shell's foreground job; the shell took the terminal back
			// when it stopped, and only the shell can hand it over again.
			res = pauseAction{Record: rec, Action: "resumed",
				Detail: fmt.Sprintf("continued in %s — if the keyboard is dead there, run `fg` in that terminal",
					orDash(rec.TTY))}

		default:
			// Its terminal is gone (or its pid was recycled); reopen one on the harness's own
			// resume invocation. A session that lived in VS Code goes back to VS Code when the
			// companion extension can take it — the record stays paused until the extension
			// actually opens the terminal, so a handoff that goes nowhere is retryable.
			if where, ok := localstate.ResumeInVSCode(rec); ok {
				res = pauseAction{Record: rec, Action: "reopened", Detail: "handed to " + where}
				break
			}
			argv, fresh := relaunchArgv(rec, exe)
			where, err := localstate.OpenTerminal(rec.Harness, rec.Cwd, argv)
			if err != nil {
				res = pauseAction{Record: rec, Action: "error", Detail: err.Error()}
				break
			}
			_ = localstate.Remove(rec.ID) // a relaunched wrap registers its own record
			detail := "reopened in " + where
			if fresh {
				detail += " (fresh session: this harness has no resume invocation)"
			}
			res = pauseAction{Record: rec, Action: "reopened", Detail: detail}
		}

		if res.Action == "resumed" || res.Action == "running" {
			if err := localstate.Save(rec); err != nil {
				return err
			}
		}
		results = append(results, res)
	}

	if *asJSON {
		return emit(results)
	}
	woken, running, failed := 0, 0, 0
	for _, r := range results {
		mark := r.Action
		switch r.Action {
		case "error":
			mark, failed = "FAILED", failed+1
		case "running":
			running++
		default:
			woken++
		}
		fmt.Printf("  %-8s %-10s %s\n", mark, r.Harness, r.Detail)
	}
	fmt.Printf("\nResumed %d session(s).", woken)
	if running > 0 {
		fmt.Printf(" %d were already running.", running)
	}
	if failed > 0 {
		fmt.Printf(" %d could not be resumed — see above.", failed)
	}
	fmt.Println()
	if failed > 0 && woken == 0 && running == 0 {
		return fmt.Errorf("no session could be resumed")
	}
	return nil
}

// Plans: what pause and resume decide to do with one record, given what the process table
// says about it. Kept as pure functions so the decision — the part that must never signal
// the wrong process — is testable without signaling anything.
const (
	planViaWrap = "via-wrap" // signal the `conductor wrap` sidecar; it handles its child
	planDirect  = "direct"   // signal the harness's process group ourselves
	planKeep    = "keep"     // process gone, record saved: keep it for resume to reopen
	planForget  = "forget"   // process gone, nothing asked us to remember it: drop the record
	planReopen  = "reopen"   // process gone: open a terminal on the harness's resume invocation
	planRunning = "running"  // saved record whose process turned out to be alive: leave it be
)

func pausePlan(rec localstate.Record, wrapLive, harnessLive bool) string {
	switch {
	case wrapLive:
		return planViaWrap
	case harnessLive:
		return planDirect
	case rec.Saved:
		return planKeep
	default:
		return planForget
	}
}

func resumePlan(rec localstate.Record, wrapLive, harnessLive bool) string {
	alive := wrapLive || harnessLive
	switch {
	case rec.Status == localstate.StatusSaved && alive:
		return planRunning
	case wrapLive:
		return planViaWrap
	case harnessLive:
		return planDirect
	default:
		return planReopen
	}
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

func describeRecord(r localstate.Record) string {
	if r.Wrapped && r.SessionID != "" {
		id := r.SessionID
		if len(id) > 8 {
			id = id[:8]
		}
		return "wrapped session " + id
	}
	return fmt.Sprintf("pid %d", r.PID)
}

func shortPath(p string) string {
	if p == "" {
		return "-"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rest, ok := strings.CutPrefix(p, home); ok {
			return "~" + rest
		}
	}
	return p
}
