package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/localstate"
	"github.com/adamburan/conductor/internal/privacy"
	"github.com/adamburan/conductor/internal/shutdownhook"
)

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------
//
// `conductor sessions` is about the agent sessions themselves, as opposed to the work they
// hold. `save` keeps the ones on this machine resumable past the life of their terminals;
// `list` shows what this machine knows; `export` writes the project's whole session history
// from the control plane.

// discoverSessions scans this machine's process table for bare harness sessions. It is a
// variable so tests can replace it: a test that let the real scan run would find — and then
// pause — whatever agent sessions the developer has open in other terminals.
var discoverSessions = localstate.Discover

func cmdSessions(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: conductor sessions <save|list|export|install-hook>")
	}
	sub, rest := args[0], args[1:]

	switch sub {
	case "save":
		return sessionsSave(ctx, rest)
	case "list":
		return sessionsList(ctx, rest)
	case "export":
		return sessionsExport(ctx, rest)
	case "install-hook":
		return sessionsInstallHook(ctx, rest)
	default:
		return fmt.Errorf("unknown sessions subcommand %q", sub)
	}
}

// ---------------------------------------------------------------------------
// sessions install-hook — capture at shutdown, machine-wide
// ---------------------------------------------------------------------------

// sessionsInstallHook installs the OS integration that captures every session when the
// machine goes down. `conductor wrap` already saves its own session on shutdown; this covers
// bare sessions and unclean shutdowns, by running `conductor sessions save all` at
// logout/shutdown and periodically. It writes the unit files but never enables them for you —
// activating a system service is a deliberate act, so it prints the exact command to run.
func sessionsInstallHook(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions install-hook", flag.ExitOnError)
	install := fs.Bool("install", false, "write the unit files (default is to print them)")
	uninstall := fs.Bool("uninstall", false, "remove the unit files")
	interval := fs.Duration("interval", 5*time.Minute, "how often the periodic capture runs")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor sessions install-hook — capture sessions when the machine shuts down

Generates a systemd user service+timer (Linux) or a launchd agent (macOS) that runs
`+"`conductor sessions save all`"+` at shutdown and periodically, so a reboot — even an unclean
one — leaves your agent sessions resumable. Sessions started with `+"`conductor wrap`"+` already
save themselves on shutdown; this adds bare sessions and the periodic safety net, and pushes
to S3 when off-host backup is configured.

  conductor sessions install-hook              print the unit files and how to enable them
  conductor sessions install-hook --install    write the files (then run the printed command)
  conductor sessions install-hook --uninstall  remove the files

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "conductor"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	plan, err := shutdownhook.Build(shutdownhook.Options{
		Exe: exe, Home: home, GOOS: runtime.GOOS, User: os.Getenv("USER"), Interval: *interval,
	})
	if err != nil {
		return err
	}

	if *uninstall {
		removed := 0
		for _, f := range plan.Files {
			if err := os.Remove(f.Path); err == nil {
				removed++
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if *asJSON {
			return emit(map[string]any{"removed": removed, "disable": plan.Disable})
		}
		fmt.Printf("Removed %d hook file(s). Deactivate it with:\n", removed)
		for _, cmd := range plan.Disable {
			fmt.Printf("  %s\n", cmd)
		}
		return nil
	}

	if *asJSON {
		return emit(plan)
	}

	if !*install {
		fmt.Printf("Shutdown-capture hook for %s. Run with --install to write these files:\n\n", plan.OS)
		for _, f := range plan.Files {
			fmt.Printf("# %s\n%s\n", f.Path, f.Content)
		}
		fmt.Println("Then enable it:")
		for _, cmd := range plan.Enable {
			fmt.Printf("  %s\n", cmd)
		}
		printHookNotes(plan)
		return nil
	}

	for _, f := range plan.Files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(f.Path, []byte(f.Content), f.Mode); err != nil {
			return err
		}
		fmt.Printf("  wrote  %s\n", f.Path)
	}
	fmt.Println("\nActivate it (Conductor does not run system commands for you):")
	for _, cmd := range plan.Enable {
		fmt.Printf("  %s\n", cmd)
	}
	printHookNotes(plan)
	return nil
}

func printHookNotes(plan shutdownhook.Plan) {
	for _, n := range plan.Notes {
		fmt.Printf("\nNote: %s\n", n)
	}
}

// ---------------------------------------------------------------------------
// sessions save — this machine, for resume
// ---------------------------------------------------------------------------

// sessionsSave pins the interactive sessions on this machine so `conductor resume` can bring
// them back after their terminals are gone.
//
// `conductor pause` already saves a record of every session, but only a *paused* record
// survives the death of its process; a running one is treated as debris the moment its pid
// disappears, which is what you want after a crash and not what you want after a reboot.
// Saving marks the record as deliberately kept. Nothing is frozen and nothing is signaled:
// the sessions keep running exactly as they were.
func sessionsSave(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions save", flag.ExitOnError)
	harness := fs.String("harness", "", "save only this harness (claude, codex, opencode, …)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor sessions save — keep agent sessions resumable

Finds every interactive Claude Code, Codex, and OpenCode session on this machine — launched
through `+"`conductor wrap`"+` or bare — and saves how to revive each one under
~/.conductor/sessions/. Nothing is stopped. A saved session's record outlives its terminal:
close the window, reboot the laptop, and `+"`conductor resume`"+` reopens the conversation
where it left off, using the harness's own resume invocation (claude --continue, codex resume
--last, opencode --continue). A session you quit yourself is forgotten; a session you saved and
then lost is reopened.

  conductor sessions save all
  conductor sessions save all --harness claude
  conductor sessions save 48213 3f9c2a1e        by pid, or by session id prefix

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return errors.New("usage: conductor sessions save <all|pid|session-id…> [--harness claude]")
	}

	records, err := localstate.Prune()
	if err != nil {
		return err
	}
	discovered, err := discoverSessions(records)
	if err != nil {
		// Wrapped sessions are still saveable from their records; say what the scan could not do.
		fmt.Fprintf(os.Stderr, "conductor: cannot scan for unwrapped sessions: %v\n", err)
	}
	records = append(records, discovered...)

	selected, err := selectRecords(records, positional, *harness)
	if err != nil {
		return err
	}

	now := time.Now()
	for i := range selected {
		if !selected[i].Saved {
			selected[i].Saved = true
			selected[i].SavedAt = now
		}
		if err := localstate.Save(selected[i]); err != nil {
			return err
		}
	}

	// Off-host durability, when configured: a local save survives a reboot, an S3 push
	// survives losing the machine. Best-effort; it never fails the local save.
	if len(selected) > 0 {
		maybeBackupAfterSave(ctx)
	}

	if *asJSON {
		if selected == nil {
			selected = []localstate.Record{}
		}
		return emit(selected)
	}
	if len(selected) == 0 {
		fmt.Println("No live Claude Code, Codex, or OpenCode sessions found in your terminals.")
		return nil
	}
	printRecords(selected)
	fmt.Printf("\nSaved %d session(s). They stay resumable after a closed terminal or a reboot: `conductor resume` reopens them.\n",
		len(selected))
	return nil
}

// selectRecords picks the local records named by args. "all" means every session (optionally
// narrowed to one harness); anything else is a pid, a record id, or a prefix of a wrapped
// session's id — the forms `conductor sessions list` prints.
//
// A name that matches nothing is an error rather than a silent omission: a script that saves
// "the session in that terminal" must not succeed having saved nothing.
func selectRecords(records []localstate.Record, args []string, harness string) ([]localstate.Record, error) {
	matchesHarness := func(r localstate.Record) bool {
		return harness == "" || r.Harness == harness
	}
	for _, a := range args {
		if a == "all" {
			out := []localstate.Record{}
			for _, r := range records {
				if matchesHarness(r) {
					out = append(out, r)
				}
			}
			return out, nil
		}
	}

	out := []localstate.Record{}
	seen := map[string]bool{}
	for _, want := range args {
		pid, _ := strconv.Atoi(want)
		var matches []localstate.Record
		for _, r := range records {
			if !matchesHarness(r) {
				continue
			}
			switch {
			case r.ID == want, pid > 0 && r.PID == pid,
				r.SessionID != "" && strings.HasPrefix(r.SessionID, want),
				len(want) >= 4 && strings.HasPrefix(r.ID, want):
				matches = append(matches, r)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("no session matches %q (see `conductor sessions list`)", want)
		case 1:
			if !seen[matches[0].ID] {
				seen[matches[0].ID] = true
				out = append(out, matches[0])
			}
		default:
			return nil, fmt.Errorf("%q is ambiguous: it matches %d sessions", want, len(matches))
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// sessions list — this machine
// ---------------------------------------------------------------------------

func sessionsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	records, err := localstate.Prune()
	if err != nil {
		return err
	}
	if *asJSON {
		if records == nil {
			records = []localstate.Record{}
		}
		return emit(records)
	}
	if len(records) == 0 {
		fmt.Println("No saved sessions. `conductor sessions save all` keeps the live ones resumable.")
		return nil
	}
	printRecords(records)
	return nil
}

// printRecords renders local session records the same way everywhere they are listed.
func printRecords(records []localstate.Record) {
	for _, r := range records {
		fmt.Printf("  %-8s %-10s %-9s %s  %s\n",
			r.Status, r.Harness, orDash(r.TTY), shortPath(r.Cwd), describeRecord(r))
	}
}

// ---------------------------------------------------------------------------
// sessions export — the project's history, from the control plane
// ---------------------------------------------------------------------------

// sessionExport is the file `conductor sessions export` writes. It is an envelope rather
// than a bare array so a file found on disk a month later says which project and moment it
// is from.
type sessionExport struct {
	Project    string                `json:"project"`
	ExportedAt time.Time             `json:"exported_at"`
	Count      int                   `json:"count"`
	Sessions   []privacy.SessionView `json:"sessions"`
}

func fetchSessions(ctx context.Context, project string) (string, []privacy.SessionView, error) {
	api, creds, err := mustClient()
	if err != nil {
		return "", nil, err
	}
	ref, err := projectRef(project, creds)
	if err != nil {
		return "", nil, err
	}
	var out struct {
		Sessions []privacy.SessionView `json:"sessions"`
	}
	if err := api.Get(ctx, "/v1/projects/"+ref+"/sessions", &out); err != nil {
		return "", nil, err
	}
	if out.Sessions == nil {
		out.Sessions = []privacy.SessionView{}
	}
	return ref, out.Sessions, nil
}

func sessionsExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions export", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	output := fs.String("output", "", "file to write; '-' for stdout (default: conductor-sessions-<project>-<time>.json)")
	asJSON := fs.Bool("json", false, "machine-readable summary of what was written")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor sessions export — write the project's session history as JSON

Exports every session the project has had — live, stale, and closed, from everyone — or only
the ones you name. Sessions are never deleted on the server, so this is the complete history.
What you see of a teammate's session follows the project's identity policy, exactly as it
does in presence.

  conductor sessions export
  conductor sessions export --output sessions.json
  conductor sessions export 3f9c2a1e 7b0d --output -

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		positional = []string{"all"}
	}

	ref, sessions, err := fetchSessions(ctx, *project)
	if err != nil {
		return err
	}
	selected, err := selectSessions(sessions, positional)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	export := sessionExport{Project: ref, ExportedAt: now, Count: len(selected), Sessions: selected}
	if *output == "-" {
		return emit(export)
	}
	path := *output
	if path == "" {
		path = defaultSessionsFile(ref, now)
	}
	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if *asJSON {
		return emit(map[string]any{"output": path, "count": len(selected)})
	}
	fmt.Printf("Wrote %d session(s) to %s\n", len(selected), path)
	return nil
}

// selectSessions picks the exported sessions named by args. "all" means every session;
// anything else is a session id or a unique prefix of one.
func selectSessions(sessions []privacy.SessionView, args []string) ([]privacy.SessionView, error) {
	for _, a := range args {
		if a == "all" {
			if sessions == nil {
				return []privacy.SessionView{}, nil
			}
			return sessions, nil
		}
	}
	out := []privacy.SessionView{}
	seen := map[string]bool{}
	for _, want := range args {
		var matches []privacy.SessionView
		for _, s := range sessions {
			if strings.HasPrefix(s.ID, want) {
				matches = append(matches, s)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("no session matches %q", want)
		case 1:
			if !seen[matches[0].ID] {
				seen[matches[0].ID] = true
				out = append(out, matches[0])
			}
		default:
			return nil, fmt.Errorf("%q is ambiguous: it matches %d sessions", want, len(matches))
		}
	}
	return out, nil
}

func defaultSessionsFile(project string, at time.Time) string {
	return fmt.Sprintf("conductor-sessions-%s-%s.json", project, at.Format("20060102T150405Z"))
}
