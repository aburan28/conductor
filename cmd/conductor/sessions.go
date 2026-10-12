package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/checkpoint"
	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/externalwork"
	"github.com/aburan28/conductor/internal/localstate"
	"github.com/aburan28/conductor/internal/privacy"
	"github.com/aburan28/conductor/internal/shutdownhook"
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
		return errors.New("usage: conductor sessions <save|list|discover|scan|install-scan|import|export|install-hook>")
	}
	sub, rest := args[0], args[1:]

	switch sub {
	case "save":
		return sessionsSave(ctx, rest)
	case "list":
		return sessionsList(ctx, rest)
	case "export":
		return sessionsExport(ctx, rest)
	case "discover":
		return sessionsDiscover(ctx, rest)
	case "scan":
		return sessionsScan(ctx, rest)
	case "install-scan":
		return sessionsInstallScan(ctx, rest)
	case "import":
		return sessionsImport(ctx, rest)
	case "install-hook":
		return sessionsInstallHook(ctx, rest)
	default:
		return fmt.Errorf("unknown sessions subcommand %q", sub)
	}
}

func externalWorkspace(ctx context.Context, override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if root, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output(); err == nil {
		return strings.TrimSpace(string(root)), nil
	}
	return wd, nil
}

// scan refreshes owner-local metadata state. Schedulers call this command directly.
func sessionsScan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions scan", flag.ExitOnError)
	since := fs.Duration("since", 7*24*time.Hour, "look back this far; 0 scans all saved records")
	workspace := fs.String("workspace", "", "repository whose project configs to include")
	stateDir := fs.String("state-dir", os.Getenv("CONDUCTOR_STATE_DIR"), "Conductor local state directory")
	status := fs.Bool("status", false, "show last stored scan without rescanning")
	asJSON := fs.Bool("json", false, "print summary as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *since < 0 {
		return errors.New("usage: conductor sessions scan [--since 168h] [--workspace path] [--status] [--json]")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if *stateDir != "" {
		*stateDir, err = filepath.Abs(*stateDir)
		if err != nil {
			return err
		}
	}
	path := externalwork.InventoryPath(home, *stateDir)
	var inv externalwork.Inventory
	if *status {
		inv, err = externalwork.LoadLatest(path)
		if err != nil {
			return err
		}
	} else {
		root, e := externalWorkspace(ctx, *workspace)
		if e != nil {
			return e
		}
		cutoff := time.Time{}
		if *since > 0 {
			cutoff = time.Now().Add(-*since)
		}
		inv = externalwork.Scan(ctx, home, root, cutoff)
		if err := externalwork.SaveLatest(path, inv); err != nil {
			return err
		}
	}
	count := map[string]int{}
	for _, c := range inv.Candidates {
		count[c.Source]++
	}
	summary := map[string]any{"scanned_at": inv.ScannedAt, "path": path, "total": len(inv.Candidates), "by_source": count, "warnings": inv.Warnings}
	if *asJSON {
		return emit(summary)
	}
	fmt.Printf("External work inventory: %s (%d saved records; running state unknown)\n", path, len(inv.Candidates))
	for _, w := range inv.Warnings {
		fmt.Fprintf(os.Stderr, "conductor: %s\n", w)
	}
	return nil
}

// install-scan uses a user-level OS timer; the Conductor server cannot inspect client files.
func sessionsInstallScan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions install-scan", flag.ExitOnError)
	interval := fs.Duration("interval", 15*time.Minute, "background scan cadence")
	since := fs.Duration("since", 7*24*time.Hour, "saved session lookback")
	workspace := fs.String("workspace", "", "repository whose project configs to include")
	stateDir := fs.String("state-dir", os.Getenv("CONDUCTOR_STATE_DIR"), "Conductor local state directory")
	install := fs.Bool("install", false, "write scheduler files")
	activate := fs.Bool("activate", false, "enable scheduler after installation")
	uninstall := fs.Bool("uninstall", false, "disable and remove scheduler files")
	asJSON := fs.Bool("json", false, "machine-readable schedule plan")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || (*activate && !*install) || (*uninstall && (*install || *activate)) {
		return errors.New("usage: conductor sessions install-scan [--interval 15m] [--install --activate | --uninstall]")
	}
	root, err := externalWorkspace(ctx, *workspace)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if *stateDir != "" {
		*stateDir, err = filepath.Abs(*stateDir)
		if err != nil {
			return err
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	plan, err := externalwork.BuildSchedule(externalwork.ScheduleOptions{Exe: exe, Home: home, Workspace: root, StateDir: *stateDir, GOOS: runtime.GOOS, Interval: *interval, Since: *since})
	if err != nil {
		return err
	}
	if *uninstall {
		if err := changeScanSchedule(ctx, plan, false); err != nil {
			return err
		}
		for _, f := range plan.Files {
			if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if runtime.GOOS == "linux" {
			if out, err := exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
				return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
			}
		}
		fmt.Println("Background scan removed.")
		return nil
	}
	if *install {
		for _, f := range plan.Files {
			if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(f.Path, []byte(f.Content), f.Mode); err != nil {
				return err
			}
		}
		if *activate {
			if err := changeScanSchedule(ctx, plan, true); err != nil {
				return err
			}
		}
	}
	if *asJSON {
		return emit(map[string]any{"plan": plan, "installed": *install, "active": *activate})
	}
	for _, f := range plan.Files {
		fmt.Printf("%s\n%s\n", f.Path, f.Content)
	}
	if *install && *activate {
		fmt.Println("Background scan activated.")
	} else if *install {
		fmt.Println("Schedule installed; run again with --install --activate to enable it.")
	}
	return nil
}

func changeScanSchedule(ctx context.Context, p externalwork.SchedulePlan, enable bool) error {
	run := func(name string, args ...string) error {
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if p.Label != "" {
		target := fmt.Sprintf("gui/%d", os.Getuid())
		_ = exec.CommandContext(ctx, "launchctl", "bootout", target, p.Files[0].Path).Run()
		if enable {
			return run("launchctl", "bootstrap", target, p.Files[0].Path)
		}
		return nil
	}
	if enable {
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if err := run("systemctl", "--user", "enable", "--now", p.Timer); err != nil {
			return err
		}
		return run("systemctl", "--user", "start", p.Service)
	}
	_ = exec.CommandContext(ctx, "systemctl", "--user", "disable", "--now", p.Timer).Run()
	return nil
}

// discover exports an owner-local metadata inventory. The export is deliberately distinct
// from `sessions export`, which is Conductor's own registered-session history.
func sessionsDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions discover", flag.ExitOnError)
	since := fs.Duration("since", 7*24*time.Hour, "look back this far for saved sessions; 0 scans all")
	output := fs.String("output", "", "write private JSON inventory to a new file")
	asJSON := fs.Bool("json", false, "print the inventory as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *since < 0 {
		return errors.New("usage: conductor sessions discover [--since 168h] [--output new-file] [--json]")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	workspace, err := os.Getwd()
	if err != nil {
		return err
	}
	if root, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output(); err == nil {
		workspace = strings.TrimSpace(string(root))
	}
	cutoff := time.Time{}
	if *since > 0 {
		cutoff = time.Now().Add(-*since)
	}
	inv := externalwork.Scan(ctx, home, workspace, cutoff)
	if *output != "" {
		f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		encodeErr := json.NewEncoder(f).Encode(inv)
		closeErr := f.Close()
		if encodeErr != nil {
			return encodeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if *asJSON {
		return emit(inv)
	}
	for _, source := range []string{"codex", "claude", "cursor"} {
		fmt.Printf("%s config files:\n", source)
		for _, p := range inv.Configs[source] {
			fmt.Printf("  %s\n", p)
		}
		if len(inv.Configs[source]) == 0 {
			fmt.Println("  (none found)")
		}
	}
	fmt.Printf("Saved sessions found: %d (running state unknown)\n", len(inv.Candidates))
	for _, c := range inv.Candidates {
		fmt.Printf("  %-43s %s %s\n", c.Ref(), c.UpdatedAt.Format(time.RFC3339), c.Title)
	}
	for _, w := range inv.Warnings {
		fmt.Fprintf(os.Stderr, "conductor: %s\n", w)
	}
	if *output != "" {
		fmt.Printf("Inventory written to %s (mode 0600)\n", *output)
	}
	return nil
}

// import creates review cards, never live session registrations. Even --apply leaves the
// task proposed and private, so discovering a chat cannot dispatch work by accident.
func sessionsImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions import", flag.ExitOnError)
	input := fs.String("input", "", "inventory written by sessions discover")
	project := fs.String("project", "", "destination Conductor project")
	apply := fs.Bool("apply", false, "create selected private proposed tasks")
	asJSON := fs.Bool("json", false, "machine-readable preview/result")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if *input == "" || len(pos) == 0 {
		return errors.New("usage: conductor sessions import --input inventory.json [--project slug] <source:uuid>... [--apply]")
	}
	f, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer f.Close()
	var inv externalwork.Inventory
	dec := json.NewDecoder(io.LimitReader(f, 32<<20))
	if err := dec.Decode(&inv); err != nil {
		return err
	}
	if inv.Version != 1 {
		return fmt.Errorf("unsupported inventory version %d", inv.Version)
	}
	chosen := make([]externalwork.Candidate, 0, len(pos))
	seen := map[string]bool{}
	for _, want := range pos {
		if seen[want] {
			continue
		}
		var matches []externalwork.Candidate
		for _, c := range inv.Candidates {
			if c.Ref() == want {
				matches = append(matches, c)
			}
		}
		if len(matches) != 1 {
			return fmt.Errorf("%q matches %d inventory records; require exactly one", want, len(matches))
		}
		c := matches[0]
		if !externalwork.ValidCandidate(c) {
			return fmt.Errorf("invalid inventory candidate %q", want)
		}
		chosen = append(chosen, c)
		seen[want] = true
	}
	if !*apply {
		if *asJSON {
			return emit(chosen)
		}
		for _, c := range chosen {
			fmt.Printf("Would create private proposed task for %s (%s)\n", c.Ref(), c.RecordPath)
		}
		fmt.Println("Run again with --apply to create these review tasks.")
		return nil
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	var existing struct {
		Tasks []privacy.TaskView `json:"tasks"`
	}
	if err := api.Get(ctx, "/v1/projects/"+ref+"/tasks"+client.Query("limit", "100000"), &existing); err != nil {
		return err
	}
	refs := map[string]bool{}
	for _, t := range existing.Tasks {
		refs[t.ExternalRef] = true
	}
	type result struct {
		ExternalRef string `json:"external_ref"`
		TaskRef     string `json:"task_ref,omitempty"`
		Status      string `json:"status"`
	}
	results := make([]result, 0, len(chosen))
	for _, c := range chosen {
		if refs[c.Ref()] {
			results = append(results, result{c.Ref(), "", "already imported"})
			continue
		}
		if _, err := os.Stat(c.RecordPath); err != nil {
			return fmt.Errorf("source for %s is no longer available: %w", c.Ref(), err)
		}
		var view privacy.TaskView
		body := externalTaskBody(c)
		if err := api.Post(ctx, "/v1/projects/"+ref+"/tasks", body, &view); err != nil {
			return fmt.Errorf("import %s: %w", c.Ref(), err)
		}
		refs[c.Ref()] = true
		results = append(results, result{c.Ref(), view.Ref, "created"})
	}
	if *asJSON {
		return emit(results)
	}
	for _, r := range results {
		fmt.Printf("%s: %s %s\n", r.ExternalRef, r.Status, r.TaskRef)
	}
	return nil
}

func externalTaskBody(c externalwork.Candidate) map[string]any {
	title := fmt.Sprintf("Review %s session %s", strings.ToUpper(c.Source[:1])+c.Source[1:], c.ID[:8])
	return map[string]any{"title": title, "objective": "Review the source session locally and define the remaining work before dispatch.", "external_ref": c.Ref(), "visibility": "private", "status": domain.TaskProposed, "scopes": []domain.ScopeRequest{}}
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

	// Portability: a record says how to reopen a conversation on this machine; a checkpoint
	// carries the conversation itself, so it can also continue on another one. Taken for
	// every saved session, skipped when nothing changed; off with CONDUCTOR_CHECKPOINT=off.
	var checkpoints []captureOutcome
	if len(selected) > 0 && !checkpoint.Disabled(os.Getenv) {
		checkpoints = captureSelectedSessions(ctx, selected, checkpoint.ReasonExit)
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
	if taken := countTaken(checkpoints); taken > 0 {
		fmt.Printf("Checkpointed %d of them as well; `conductor checkpoint list` shows what can continue on another machine or login.\n", taken)
	}
	return nil
}

// captureSelectedSessions checkpoints the given records, quietly.
func captureSelectedSessions(ctx context.Context, records []localstate.Record, reason string) []captureOutcome {
	var out []captureOutcome
	for _, rec := range records {
		res, err := checkpoint.Capture(ctx, checkpoint.Request{
			Harness: rec.Harness, Cwd: rec.Cwd, PID: rec.PID, Since: rec.StartedAt.Add(-time.Minute),
			Reason: reason, Conductor: checkpoint.ConductorRef{Project: rec.Project, SessionID: rec.SessionID},
		})
		o := captureOutcome{Harness: rec.Harness, Cwd: rec.Cwd, Result: res}
		if err != nil {
			o.Error = err.Error()
		}
		out = append(out, o)
	}
	return out
}

func countTaken(outcomes []captureOutcome) int {
	n := 0
	for _, o := range outcomes {
		if o.Error == "" {
			n++
		}
	}
	return n
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
	// Owner-only: the export names machines, worktree paths, and who ran what, which is the
	// same class of data the control plane gates by role. A shared machine's other users
	// have no business reading it.
	if err := writePrivateFile(path, append(data, '\n')); err != nil {
		return err
	}
	if *asJSON {
		return emit(map[string]any{"output": path, "count": len(selected)})
	}
	fmt.Printf("Wrote %d session(s) to %s\n", len(selected), path)
	return nil
}

// writePrivateFile writes data with mode 0600, tightening a file that already existed with a
// looser mode (os.WriteFile keeps an existing file's permissions).
func writePrivateFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
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
