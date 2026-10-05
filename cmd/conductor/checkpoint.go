package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/backup"
	"github.com/adamburan/conductor/internal/checkpoint"
	"github.com/adamburan/conductor/internal/localstate"
	"github.com/adamburan/conductor/internal/usage"
)

// ---------------------------------------------------------------------------
// checkpoint — make a running session portable
// ---------------------------------------------------------------------------
//
// `conductor pause` and `conductor sessions save` keep a conversation reopenable on THIS
// machine under THIS account, because that is where the harness keeps the transcript. A
// checkpoint is the conversation itself plus the working tree, in one file, so the work can
// continue somewhere the harness's own store cannot reach: another machine, another
// account when this one's usage limit is spent, or another harness altogether.
//
// Checkpoints hold the conversation, which Conductor's control plane never sees. They are
// written only here, on the user's machine, under ~/.conductor/checkpoints (0600 in 0700),
// and leave it only as a file the user moves or as ciphertext in the user's own bucket.

func cmdCheckpoint(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: conductor checkpoint <capture|list|show|resume|export|import|push|pull|prune>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "capture", "save", "take":
		return checkpointCapture(ctx, rest)
	case "list", "ls":
		return checkpointList(ctx, rest)
	case "show":
		return checkpointShow(ctx, rest)
	case "resume", "restore":
		return checkpointResume(ctx, rest)
	case "export":
		return checkpointExport(ctx, rest)
	case "import":
		return checkpointImport(ctx, rest)
	case "push":
		return checkpointPush(ctx, rest)
	case "pull":
		return checkpointPull(ctx, rest)
	case "prune":
		return checkpointPrune(ctx, rest)
	default:
		return fmt.Errorf("unknown checkpoint subcommand %q", sub)
	}
}

// ---------------------------------------------------------------------------
// capture
// ---------------------------------------------------------------------------

func checkpointCapture(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("checkpoint capture", flag.ExitOnError)
	harness := fs.String("harness", "", "the harness whose session to capture (claude, codex, opencode); default: detect from the live sessions here")
	session := fs.String("session", "", "the harness's own session id (default: the newest session for this directory)")
	pid := fs.Int("pid", 0, "the harness process, to pin the session where the harness records one per process")
	all := fs.Bool("all", false, "capture every live session on this machine")
	note := fs.String("note", "", "a line about where the work stands, carried into the continuation")
	reason := fs.String("reason", checkpoint.ReasonManual, "why (manual, periodic, exit, hook…); shown by list")
	noWorkspace := fs.Bool("no-workspace", false, "leave the working tree out; transcript only")
	force := fs.Bool("force", false, "write even if nothing changed since the last checkpoint")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor checkpoint capture — snapshot a running session so it can continue elsewhere

Bundles the harness's own transcript, the uncommitted working tree (tracked changes,
untracked files, and commits not on any remote), and a harness-neutral CONTINUATION.md
into one file under ~/.conductor/checkpoints/. Nothing is stopped and nothing is sent
anywhere. `+"`conductor wrap`"+` does this every few minutes on its own; this is the by-hand form.

  conductor checkpoint capture                          the newest session in this directory
  conductor checkpoint capture --harness codex          a specific harness
  conductor checkpoint capture --note "tests pass; wiring the CLI next"
  conductor checkpoint capture --all                    every live session on this machine

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all {
		results, err := captureAllSessions(ctx, *reason, *note, *force)
		if err != nil {
			return err
		}
		if *asJSON {
			return emit(results)
		}
		if len(results) == 0 {
			fmt.Println("No live Claude Code, Codex, or OpenCode sessions found on this machine.")
			return nil
		}
		for _, r := range results {
			printCaptureResult(r)
		}
		return nil
	}

	cwd, _ := os.Getwd()
	h := *harness
	if h == "" {
		h = detectHarnessHere(cwd)
		if h == "" {
			return errors.New("no live session found for this directory; name one with --harness")
		}
	}
	req := checkpoint.Request{
		Harness: h, Cwd: cwd, SessionID: *session, PID: *pid, Since: time.Now().Add(-30 * 24 * time.Hour),
		Reason: *reason, Note: *note, SkipWorkspace: *noWorkspace, Force: *force,
		Conductor: conductorRefFromEnv(),
	}
	res, err := checkpoint.Capture(ctx, req)
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(res)
	}
	printCaptureResult(captureOutcome{Harness: h, Cwd: cwd, Result: res})
	return nil
}

// captureOutcome is one row of a multi-session capture.
type captureOutcome struct {
	Harness string            `json:"harness"`
	Cwd     string            `json:"cwd"`
	Result  checkpoint.Result `json:"result"`
	Error   string            `json:"error,omitempty"`
}

func printCaptureResult(o captureOutcome) {
	switch {
	case o.Error != "":
		fmt.Printf("  %-9s %s: %s\n", o.Harness, shortPath(o.Cwd), o.Error)
	case o.Result.Skipped != "":
		fmt.Printf("  %-9s %s: unchanged (%s; latest is %s)\n", o.Harness, shortPath(o.Cwd), o.Result.Skipped, checkpoint.ShortID(o.Result.Manifest.ID))
	default:
		m := o.Result.Manifest
		fmt.Printf("  %-9s %s: checkpoint %s  (%s, %d turns, %s)\n", o.Harness, shortPath(o.Cwd), checkpoint.ShortID(m.ID),
			humanBytes(m.Transcript.Bytes), m.Transcript.UserTurns+m.Transcript.AssistantTurns, describeWorkspace(m))
		fmt.Printf("            resume anywhere: conductor checkpoint resume %s\n", checkpoint.ShortID(m.ID))
	}
}

// captureAllSessions checkpoints every live session this machine knows about: wrapped
// sessions from their records, bare ones from the process table. Shared with
// `sessions save all`, so the shutdown hook captures conversations as well as records.
func captureAllSessions(ctx context.Context, reason, note string, force bool) ([]captureOutcome, error) {
	records, err := localstate.Prune()
	if err != nil {
		return nil, err
	}
	discovered, err := discoverSessions(records)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conductor: cannot scan for unwrapped sessions: %v\n", err)
	}
	records = append(records, discovered...)
	var out []captureOutcome
	for _, rec := range records {
		if rec.Status != localstate.StatusRunning && rec.Status != localstate.StatusPaused {
			continue
		}
		req := checkpoint.Request{
			Harness: rec.Harness, Cwd: rec.Cwd, PID: rec.PID, Since: rec.StartedAt.Add(-time.Minute),
			Reason: reason, Note: note, Force: force,
			Conductor: checkpoint.ConductorRef{Project: rec.Project, SessionID: rec.SessionID},
		}
		res, err := checkpoint.Capture(ctx, req)
		o := captureOutcome{Harness: rec.Harness, Cwd: rec.Cwd, Result: res}
		if err != nil {
			o.Error = err.Error()
		}
		out = append(out, o)
	}
	if out == nil {
		out = []captureOutcome{}
	}
	return out, nil
}

// detectHarnessHere names the harness of a live session in cwd, when exactly one kind is
// running there; otherwise the one with the most recent transcript.
func detectHarnessHere(cwd string) string {
	records, _ := localstate.Prune()
	discovered, _ := discoverSessions(records)
	kinds := map[string]bool{}
	for _, rec := range append(records, discovered...) {
		if rec.Cwd == cwd {
			kinds[checkpoint.NormalizeHarness(rec.Harness)] = true
		}
	}
	if len(kinds) == 1 {
		for k := range kinds {
			return k
		}
	}
	// Fall back to whichever harness wrote to this directory last.
	since := time.Now().Add(-30 * 24 * time.Hour)
	var best string
	var bestAt time.Time
	for _, h := range []string{"claude", "codex"} {
		var files []string
		switch h {
		case "claude":
			if src, err := checkpoint.LocateClaude(usage.ClaudeConfigDir(os.Getenv), cwd, "", since); err == nil {
				files = []string{src.Path}
			}
		case "codex":
			if src, err := checkpoint.LocateCodex(usage.CodexHome(os.Getenv), cwd, "", since); err == nil {
				files = []string{src.Path}
			}
		}
		for _, f := range files {
			if st, err := os.Stat(f); err == nil && st.ModTime().After(bestAt) {
				best, bestAt = h, st.ModTime()
			}
		}
	}
	return best
}

func conductorRefFromEnv() checkpoint.ConductorRef {
	return checkpoint.ConductorRef{
		Project: os.Getenv("CONDUCTOR_PROJECT"), SessionID: os.Getenv("CONDUCTOR_SESSION_ID"),
		Task: firstNonEmptyString(os.Getenv("CONDUCTOR_TASK_REF"), os.Getenv("CONDUCTOR_TASK_ID")),
	}
}

// ---------------------------------------------------------------------------
// list / show
// ---------------------------------------------------------------------------

func checkpointList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("checkpoint list", flag.ExitOnError)
	session := fs.String("session", "", "only checkpoints of this session (id or prefix)")
	allRows := fs.Bool("all", false, "every checkpoint, not just the newest per session")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor checkpoint list — the checkpoints on this machine

  conductor checkpoint list              newest checkpoint of every session
  conductor checkpoint list --all        every checkpoint, newest first
  conductor checkpoint list --session 3f9c

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	all, err := checkpoint.List()
	if err != nil {
		return err
	}
	var rows []checkpoint.Manifest
	seen := map[string]bool{}
	for _, m := range all {
		if *session != "" && !strings.HasPrefix(m.SessionID, *session) {
			continue
		}
		key := checkpoint.SessionKey(m.Harness, m.SessionID)
		if !*allRows && seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, m)
	}
	if *asJSON {
		if rows == nil {
			rows = []checkpoint.Manifest{}
		}
		return emit(rows)
	}
	if len(rows) == 0 {
		fmt.Println("No checkpoints on this machine. `conductor checkpoint capture` takes one; `conductor wrap` takes them automatically.")
		return nil
	}
	fmt.Printf("  %-8s %-9s %-16s %-9s %-7s %-34s %s\n", "ID", "HARNESS", "TAKEN", "REASON", "TURNS", "SESSION", "WHERE")
	now := time.Now()
	for _, m := range rows {
		title := m.Title
		if title == "" {
			title = shortPath(m.Cwd)
		}
		fmt.Printf("  %-8s %-9s %-16s %-9s %-7d %-34s %s\n", checkpoint.ShortID(m.ID), m.Harness, humanAge(now.Sub(m.CreatedAt)),
			orDash(m.Reason), m.Transcript.UserTurns+m.Transcript.AssistantTurns, m.SessionID, title)
	}
	fmt.Printf("\n%d checkpoint(s). `conductor checkpoint resume <id>` continues one here; `--harness` or `--account` continues it elsewhere.\n", len(rows))
	return nil
}

func checkpointShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("checkpoint show", flag.ExitOnError)
	continuation := fs.Bool("continuation", false, "print CONTINUATION.md instead of the manifest")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor checkpoint show <id|file> — what a checkpoint holds

  conductor checkpoint show 9a474a
  conductor checkpoint show latest --continuation       the hand-over document inside it
  conductor checkpoint show ./session.ckpt              a file someone sent you

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	ref := "latest"
	if len(positional) > 0 {
		ref = positional[0]
	}
	b, err := openCheckpointRef(ref)
	if err != nil {
		return err
	}
	m := b.Manifest
	if *continuation {
		data, _ := b.File(checkpoint.ContinuationPath)
		os.Stdout.Write(data)
		return nil
	}
	if *asJSON {
		return emit(m)
	}
	fmt.Printf("Checkpoint %s  (%s)\n\n", m.ID, checkpoint.ShortID(m.ID))
	fmt.Printf("  taken      %s on %s (%s)\n", m.CreatedAt.Local().Format(time.RFC1123), orDash(m.Machine), orDash(m.Reason))
	fmt.Printf("  harness    %s %s, session %s\n", m.Harness, m.HarnessVersion, m.SessionID)
	if m.Title != "" {
		fmt.Printf("  title      %s\n", m.Title)
	}
	fmt.Printf("  cwd        %s\n", m.Cwd)
	if m.Repo.Root != "" {
		fmt.Printf("  repo       %s\n", orDash(m.Repo.Remote))
		fmt.Printf("  branch     %s @ %.12s  (%s)\n", orDash(m.Repo.Branch), m.Repo.Head, describeWorkspace(m))
	}
	fmt.Printf("  transcript %s, %d records, %d user / %d assistant turns, last activity %s\n",
		humanBytes(m.Transcript.Bytes), m.Transcript.Records, m.Transcript.UserTurns, m.Transcript.AssistantTurns, humanAgeAt(m.Transcript.LastActivity))
	if m.Conductor.Project != "" || m.Conductor.Task != "" {
		fmt.Printf("  conductor  project %s, task %s\n", orDash(m.Conductor.Project), orDash(m.Conductor.Task))
	}
	if m.Parent != "" {
		fmt.Printf("  parent     %s\n", m.Parent)
	}
	if m.ResumedFrom != "" {
		fmt.Printf("  resumed    from %s\n", m.ResumedFrom)
	}
	if m.Note != "" {
		fmt.Printf("  note       %s\n", m.Note)
	}
	if m.Workspace.Skipped != "" {
		fmt.Printf("  skipped    %s\n", m.Workspace.Skipped)
	}
	fmt.Println()
	fmt.Printf("  conductor checkpoint resume %s                       continue here, same harness\n", checkpoint.ShortID(m.ID))
	fmt.Printf("  conductor checkpoint resume %s --account work         under another login\n", checkpoint.ShortID(m.ID))
	fmt.Printf("  conductor checkpoint resume %s --harness codex        in a different harness\n", checkpoint.ShortID(m.ID))
	return nil
}

// openCheckpointRef opens a checkpoint named by store id, session id, `latest`, or a path.
func openCheckpointRef(ref string) (*checkpoint.Bundle, error) {
	if strings.ContainsAny(ref, "/\\") || strings.HasSuffix(ref, checkpoint.BundleExt) {
		if _, err := os.Stat(ref); err == nil {
			return checkpoint.OpenFile(ref, func() (string, error) { return checkpointPassphrase(true) })
		}
	}
	m, err := checkpoint.Resolve(ref)
	if err != nil {
		if _, serr := os.Stat(ref); serr == nil {
			return checkpoint.OpenFile(ref, func() (string, error) { return checkpointPassphrase(true) })
		}
		return nil, err
	}
	return checkpoint.Load(m)
}

// ---------------------------------------------------------------------------
// resume
// ---------------------------------------------------------------------------

func checkpointResume(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("checkpoint resume", flag.ExitOnError)
	harness := fs.String("harness", "", "resume in this harness instead of the original (claude, codex, opencode)")
	account := fs.String("account", "", "a named login: ~/.<harness>-<name> if it exists, else ~/.conductor/accounts/<harness>/<name>")
	stateDir := fs.String("state-dir", "", "the harness's state directory to resume under (CLAUDE_CONFIG_DIR / CODEX_HOME / XDG_DATA_HOME)")
	dir := fs.String("dir", "", "the checkout to restore into (default: the current directory)")
	clone := fs.Bool("clone", false, "create --dir by cloning the checkpoint's remote when it does not exist")
	force := fs.Bool("force", false, "restore over uncommitted changes and replace an installed transcript")
	noWorkspace := fs.Bool("no-workspace", false, "leave the working tree alone; only the conversation is restored")
	printOnly := fs.Bool("print", false, "prepare everything and print the launch command instead of running it")
	wrap := fs.Bool("wrap", false, "launch through `conductor wrap`, so the session is registered and keeps checkpointing (default when the checkpoint came from a wrapped session)")
	noWrap := fs.Bool("no-wrap", false, "launch the harness directly")
	asJSON := fs.Bool("json", false, "machine-readable output (implies --print)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor checkpoint resume <id|file> — continue a checkpointed session

Restores the working tree the checkpoint carries into a checkout (the current directory,
or --dir), puts the conversation where the target harness will find it, and launches the
harness on it. The same harness reopens the very same conversation; a different harness
starts from the checkpoint's CONTINUATION.md.

  conductor checkpoint resume latest                       same machine, same harness
  conductor checkpoint resume 9a474a --account work        another login (usage limit hit)
  conductor checkpoint resume 9a474a --harness codex       Claude → Codex
  conductor checkpoint resume ./session.ckpt --dir ~/src/repo --clone     on a new machine
  conductor checkpoint resume 9a474a --print               show what it would run

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return errors.New("usage: conductor checkpoint resume <id|file> [--harness H] [--account NAME] [--dir DIR]")
	}
	b, err := openCheckpointRef(positional[0])
	if err != nil {
		return err
	}
	m := b.Manifest

	target := checkpoint.NormalizeHarness(*harness)
	if target == "" {
		target = checkpoint.NormalizeHarness(m.Harness)
	}
	if *account != "" && *stateDir != "" {
		return errors.New("--account and --state-dir are alternatives")
	}
	state := *stateDir
	if *account != "" {
		state = accountStateDir(target, *account)
	}

	// 1. The working tree.
	cwd := *dir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd, _ = filepath.Abs(cwd)
	var restore checkpoint.RestoreReport
	if !*noWorkspace {
		restore, err = checkpoint.RestoreWorkspace(ctx, b, checkpoint.RestoreOptions{Dir: cwd, Clone: *clone, Force: *force})
		if err != nil {
			return err
		}
		if root := restoredRoot(cwd); root != "" && m.Repo.Subdir != "" {
			cwd = filepath.Join(root, filepath.FromSlash(m.Repo.Subdir))
		}
	} else if _, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("%s does not exist", cwd)
	}

	// 2. The conversation and the launch.
	launch, err := checkpoint.PrepareResume(ctx, b, checkpoint.ResumeOptions{Target: target, Cwd: cwd, StateDir: state, Force: *force})
	if err != nil {
		return err
	}
	useWrap := (*wrap || m.Conductor.Project != "") && !*noWrap
	if useWrap {
		exe, err := os.Executable()
		if err != nil {
			exe = "conductor"
		}
		launch.Argv = append([]string{exe, "wrap", launch.Harness}, launch.Argv[1:]...)
	}
	launch.Env = append(launch.Env, "CONDUCTOR_RESUMED_FROM="+m.ID)

	out := struct {
		Checkpoint checkpoint.Manifest      `json:"checkpoint"`
		Restore    checkpoint.RestoreReport `json:"restore"`
		Launch     checkpoint.Launch        `json:"launch"`
	}{m, restore, launch}
	if *asJSON {
		return emit(out)
	}
	printResumePlan(out.Restore, launch, *noWorkspace)
	if *printOnly {
		return nil
	}
	return runLaunch(ctx, launch)
}

func restoredRoot(dir string) string {
	out, err := checkpoint.Git(context.Background(), dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// accountStateDir maps a login name to a harness state directory. The community convention
// for Claude Code is ~/.claude-<name> (each with its own credentials), so that is honoured
// when it exists; otherwise every harness gets a directory under ~/.conductor/accounts.
func accountStateDir(harness, name string) string {
	home, _ := os.UserHomeDir()
	if p := filepath.Join(home, "."+harness+"-"+name); dirExists(p) {
		return p
	}
	return filepath.Join(home, ".conductor", "accounts", harness, name)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func printResumePlan(r checkpoint.RestoreReport, l checkpoint.Launch, skippedWorkspace bool) {
	switch {
	case skippedWorkspace:
		fmt.Println("Working tree: left alone (--no-workspace).")
	case r.Cloned:
		fmt.Printf("Working tree: cloned into %s", r.Dir)
		if r.CheckedOut != "" {
			fmt.Printf(", %s", r.CheckedOut)
		}
		fmt.Println(".")
	default:
		var parts []string
		if r.FetchedBundle {
			parts = append(parts, "fetched the checkpoint's commits")
		}
		if r.CheckedOut != "" {
			parts = append(parts, r.CheckedOut)
		}
		if r.PatchApplied {
			parts = append(parts, "applied uncommitted changes")
		}
		if r.Untracked > 0 {
			parts = append(parts, fmt.Sprintf("restored %d untracked file(s)", r.Untracked))
		}
		if len(parts) == 0 {
			parts = append(parts, "nothing to restore")
		}
		fmt.Printf("Working tree: %s in %s.\n", strings.Join(parts, ", "), shortPath(r.Dir))
	}
	for _, w := range r.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
	if l.Native {
		fmt.Printf("Conversation: %s reopens session %s", l.Harness, l.SessionID)
		if l.Transcript != "" {
			fmt.Printf(" from %s", shortPath(l.Transcript))
		}
		fmt.Println(".")
	} else {
		fmt.Printf("Conversation: %s starts from %s.\n", l.Harness, shortPath(l.Continuation))
	}
	for _, n := range l.Notes {
		fmt.Printf("  note: %s\n", n)
	}
	fmt.Println()
	var env string
	for _, e := range l.Env {
		if !strings.HasPrefix(e, "CONDUCTOR_RESUMED_FROM=") {
			env += e + " "
		}
	}
	fmt.Printf("  cd %s && %s%s\n\n", shellQuoteArg(l.Dir), env, shellJoin(l.Argv))
}

// runLaunch starts the harness in the foreground with the terminal, and exits with its
// status, exactly as `conductor wrap` would.
func runLaunch(ctx context.Context, l checkpoint.Launch) error {
	if _, err := exec.LookPath(l.Argv[0]); err != nil {
		return fmt.Errorf("%s is not installed here (%v); run the command above where it is", l.Argv[0], err)
	}
	cmd := exec.CommandContext(ctx, l.Argv[0], l.Argv[1:]...)
	cmd.Dir = l.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), l.Env...)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuoteArg(a)
	}
	return strings.Join(parts, " ")
}

func shellQuoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---------------------------------------------------------------------------
// export / import — move a checkpoint as a file
// ---------------------------------------------------------------------------

func checkpointExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("checkpoint export", flag.ExitOnError)
	out := fs.String("out", "", "file to write (default: <id>.ckpt in the current directory)")
	seal := fs.Bool("seal", false, "encrypt with a passphrase (CONDUCTOR_CHECKPOINT_KEY, or prompted)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor checkpoint export <id> — write a checkpoint to a file you can move

A checkpoint holds your conversation. Moving it over anything you do not control, seal it:

  conductor checkpoint export latest --out ~/Desktop/session.ckpt
  conductor checkpoint export 9a474a --seal                 asks for a passphrase
  CONDUCTOR_CHECKPOINT_KEY=… conductor checkpoint export 9a474a --seal

On the other side: conductor checkpoint resume session.ckpt --dir ~/src/repo --clone

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	ref := "latest"
	if len(positional) > 0 {
		ref = positional[0]
	}
	m, err := checkpoint.Resolve(ref)
	if err != nil {
		return err
	}
	src, err := checkpoint.Path(m.ID)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if *seal {
		pass, err := checkpointPassphrase(true)
		if err != nil {
			return err
		}
		if data, err = checkpoint.Seal(data, pass); err != nil {
			return err
		}
	}
	dst := *out
	if dst == "" {
		dst = m.ID + checkpoint.BundleExt
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return err
	}
	if *asJSON {
		return emit(map[string]any{"id": m.ID, "path": dst, "sealed": *seal, "bytes": len(data)})
	}
	state := "in the clear"
	if *seal {
		state = "sealed"
	}
	fmt.Printf("Wrote %s (%s, %s).\n", dst, humanBytes(int64(len(data))), state)
	fmt.Printf("Elsewhere: conductor checkpoint resume %s --dir <checkout> [--clone] [--harness …] [--account …]\n", filepath.Base(dst))
	return nil
}

func checkpointImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("checkpoint import", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor checkpoint import <file> — add a checkpoint file to this machine's store

  conductor checkpoint import ~/Downloads/session.ckpt

A sealed file asks for its passphrase. `+"`conductor checkpoint resume <file>`"+` works directly
on a file too; importing just makes it show up in `+"`list`"+`.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return errors.New("usage: conductor checkpoint import <file>")
	}
	b, err := checkpoint.OpenFile(positional[0], func() (string, error) { return checkpointPassphrase(true) })
	if err != nil {
		return err
	}
	data, err := os.ReadFile(positional[0])
	if err != nil {
		return err
	}
	if checkpoint.IsSealed(data) {
		pass, _ := checkpointPassphrase(true)
		if data, err = checkpoint.Unseal(data, pass); err != nil {
			return err
		}
	}
	path, err := checkpoint.Put(b.Manifest, data)
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(map[string]any{"id": b.Manifest.ID, "path": path})
	}
	fmt.Printf("Imported %s (%s session %s). `conductor checkpoint resume %s` continues it.\n",
		b.Manifest.ID, b.Manifest.Harness, b.Manifest.SessionID, checkpoint.ShortID(b.Manifest.ID))
	return nil
}

// ---------------------------------------------------------------------------
// push / pull — move a checkpoint through the user's own bucket
// ---------------------------------------------------------------------------

func checkpointPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("checkpoint push", flag.ExitOnError)
	all := fs.Bool("all", false, "push every checkpoint not yet in the bucket")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor checkpoint push [id…] — copy checkpoints to the configured S3 bucket, sealed

Uses the same bucket as `+"`conductor backup`"+` (CONDUCTOR_BACKUP_S3_BUCKET and friends) under
<prefix>/checkpoints/, and REQUIRES a passphrase in CONDUCTOR_CHECKPOINT_KEY: a checkpoint
holds your conversation, and it never leaves this machine in the clear.

  CONDUCTOR_CHECKPOINT_KEY=… conductor checkpoint push latest
  CONDUCTOR_CHECKPOINT_KEY=… conductor checkpoint push --all

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	store, err := openBackup()
	if err != nil {
		return err
	}
	pass := os.Getenv("CONDUCTOR_CHECKPOINT_KEY")
	if pass == "" {
		return errors.New("CONDUCTOR_CHECKPOINT_KEY is not set; a checkpoint is pushed only sealed")
	}
	var targets []checkpoint.Manifest
	switch {
	case *all:
		local, err := checkpoint.List()
		if err != nil {
			return err
		}
		remote, err := store.ListCheckpoints(ctx)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, id := range remote {
			have[id] = true
		}
		for _, m := range local {
			if !have[m.ID] {
				targets = append(targets, m)
			}
		}
	case len(positional) == 0:
		m, err := checkpoint.Resolve("latest")
		if err != nil {
			return err
		}
		targets = []checkpoint.Manifest{m}
	default:
		for _, ref := range positional {
			m, err := checkpoint.Resolve(ref)
			if err != nil {
				return err
			}
			targets = append(targets, m)
		}
	}
	var pushed []string
	for _, m := range targets {
		if err := pushCheckpoint(ctx, store, m, pass); err != nil {
			return fmt.Errorf("%s: %w", m.ID, err)
		}
		pushed = append(pushed, m.ID)
	}
	if pushed == nil {
		pushed = []string{}
	}
	if *asJSON {
		return emit(map[string]any{"pushed": pushed, "location": store.CheckpointLocation()})
	}
	fmt.Printf("Pushed %d checkpoint(s), sealed, to %s\n", len(pushed), store.CheckpointLocation())
	for _, id := range pushed {
		fmt.Printf("  %s\n", id)
	}
	if len(pushed) > 0 {
		fmt.Printf("\nElsewhere: CONDUCTOR_CHECKPOINT_KEY=… conductor checkpoint pull %s\n", checkpoint.ShortID(pushed[len(pushed)-1]))
	}
	return nil
}

// pushCheckpoint seals one stored bundle and uploads it with its manifest.
func pushCheckpoint(ctx context.Context, store *backup.Store, m checkpoint.Manifest, pass string) error {
	p, err := checkpoint.Path(m.ID)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	sealed, err := checkpoint.Seal(data, pass)
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(strings.TrimSuffix(p, checkpoint.BundleExt) + checkpoint.ManifestExt)
	if err != nil {
		return err
	}
	return store.PutCheckpoint(ctx, m.ID, manifest, sealed, checkpoint.IsSealed)
}

func checkpointPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("checkpoint pull", flag.ExitOnError)
	list := fs.Bool("list", false, "list what the bucket holds instead of pulling")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor checkpoint pull <id|prefix> — fetch a sealed checkpoint from the bucket

  conductor checkpoint pull --list
  CONDUCTOR_CHECKPOINT_KEY=… conductor checkpoint pull 9a474a
  conductor checkpoint resume 9a474a --dir ~/src/repo --clone

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	store, err := openBackup()
	if err != nil {
		return err
	}
	ids, err := store.ListCheckpoints(ctx)
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	if *list || len(positional) == 0 {
		if *asJSON {
			if ids == nil {
				ids = []string{}
			}
			return emit(map[string]any{"location": store.CheckpointLocation(), "checkpoints": ids})
		}
		if len(ids) == 0 {
			fmt.Printf("No checkpoints in %s\n", store.CheckpointLocation())
			return nil
		}
		fmt.Printf("Checkpoints in %s\n", store.CheckpointLocation())
		for _, id := range ids {
			fmt.Printf("  %s\n", id)
		}
		if len(positional) == 0 && !*list {
			fmt.Println("\nName one: conductor checkpoint pull <id>")
		}
		return nil
	}
	var hits []string
	for _, id := range ids {
		if id == positional[0] || strings.HasPrefix(id, positional[0]) || checkpoint.ShortID(id) == positional[0] {
			hits = append(hits, id)
		}
	}
	if len(hits) == 0 {
		return fmt.Errorf("no checkpoint in the bucket matches %q", positional[0])
	}
	if len(hits) > 1 {
		return fmt.Errorf("%q matches %d checkpoints in the bucket; give more of the id", positional[0], len(hits))
	}
	sealed, err := store.GetCheckpoint(ctx, hits[0])
	if err != nil {
		return err
	}
	pass, err := checkpointPassphrase(true)
	if err != nil {
		return err
	}
	data, err := checkpoint.Unseal(sealed, pass)
	if err != nil {
		return err
	}
	b, err := checkpoint.Open(strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	path, err := checkpoint.Put(b.Manifest, data)
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(map[string]any{"id": b.Manifest.ID, "path": path})
	}
	fmt.Printf("Pulled %s (%s session %s, taken %s on %s).\n", b.Manifest.ID, b.Manifest.Harness, b.Manifest.SessionID,
		humanAge(time.Since(b.Manifest.CreatedAt))+" ago", orDash(b.Manifest.Machine))
	fmt.Printf("Continue it: conductor checkpoint resume %s --dir <checkout> [--clone] [--harness …] [--account …]\n", checkpoint.ShortID(b.Manifest.ID))
	return nil
}

// ---------------------------------------------------------------------------
// prune
// ---------------------------------------------------------------------------

func checkpointPrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("checkpoint prune", flag.ExitOnError)
	keep := fs.Int("keep", checkpoint.KeepFromEnv(os.Getenv), "checkpoints to keep per session")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor checkpoint prune — thin old checkpoints

Keeps the newest N per session (CONDUCTOR_CHECKPOINT_KEEP, default 5). Capture prunes on its
own; this is for a store that grew while captures were off.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	removed, err := checkpoint.Prune(*keep, 0, time.Now())
	if err != nil {
		return err
	}
	if *asJSON {
		if removed == nil {
			removed = []checkpoint.Manifest{}
		}
		return emit(removed)
	}
	fmt.Printf("Removed %d checkpoint(s); kept the newest %d per session.\n", len(removed), *keep)
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// checkpointPassphrase reads the sealing passphrase from CONDUCTOR_CHECKPOINT_KEY, or asks
// on the terminal.
func checkpointPassphrase(required bool) (string, error) {
	if v := os.Getenv("CONDUCTOR_CHECKPOINT_KEY"); v != "" {
		return v, nil
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		if required {
			return "", errors.New("a passphrase is required: set CONDUCTOR_CHECKPOINT_KEY (no terminal to ask on)")
		}
		return "", nil
	}
	defer tty.Close()
	fmt.Fprint(tty, "Checkpoint passphrase: ")
	// Turn echo off for the read where stty is available; fall back to a visible read.
	stty := exec.Command("stty", "-echo")
	stty.Stdin = tty
	echoOff := stty.Run() == nil
	line, err := bufio.NewReader(tty).ReadString('\n')
	if echoOff {
		on := exec.Command("stty", "echo")
		on.Stdin = tty
		_ = on.Run()
		fmt.Fprintln(tty)
	}
	if err != nil && line == "" {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" && required {
		return "", errors.New("empty passphrase")
	}
	return line, nil
}

func describeWorkspace(m checkpoint.Manifest) string {
	if m.Workspace.Skipped != "" && m.Workspace.PatchBytes == 0 && len(m.Workspace.UntrackedFiles) == 0 && !m.Workspace.CommitsBundle {
		return "no working tree"
	}
	var parts []string
	if m.Repo.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d unpushed commit(s)", m.Repo.Ahead))
	}
	if m.Workspace.PatchBytes > 0 {
		parts = append(parts, fmt.Sprintf("%d modified", m.Repo.Modified))
	}
	if n := len(m.Workspace.UntrackedFiles); n > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked", n))
	}
	if len(parts) == 0 {
		return "clean tree"
	}
	return strings.Join(parts, ", ")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func humanAgeAt(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return humanAge(time.Since(t))
}
