package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/transfer"
)

// ---------------------------------------------------------------------------
// sessions transfer
// ---------------------------------------------------------------------------
//
// `conductor sessions transfer` is the multi-session counterpart to `conductor checkpoint`:
// it carries every conversation of a project in one file, and imports only what is new.
// Nothing here talks to the control plane; a bundle is a local file the user moves.

func cmdTransfer(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(transferUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "export":
		return transferExport(rest)
	case "import":
		return transferImport(rest)
	case "inspect":
		return transferInspect(rest)
	case "scan":
		return transferScan(rest)
	case "serve":
		return transferServe(ctx, rest)
	case "connect":
		return transferConnect(ctx, rest)
	default:
		return fmt.Errorf("unknown sessions transfer subcommand %q\n\n%s", sub, transferUsage)
	}
}

const transferUsage = `usage: conductor sessions transfer <export|import|inspect|scan|serve|connect>

  export    write a project's sessions to one .codexbundle
  import    verify and copy a bundle's sessions into this machine's agent home
  inspect   show what a bundle contains, read-only
  scan      check a bundle (or local sessions) for likely secrets
  serve     offer this machine's sessions to a paired device on the LAN
  connect   pull, push, or exchange sessions with a device running serve

Run ` + "`conductor sessions transfer <subcommand> -h`" + ` for its flags.`

// ---------------------------------------------------------------------------
// export
// ---------------------------------------------------------------------------

func transferExport(args []string) error {
	fs := flag.NewFlagSet("sessions transfer export", flag.ExitOnError)
	tool := fs.String("tool", "", "which agent: claude or codex (default: auto-detect)")
	project := fs.String("project", "", "export sessions recorded in this project directory (default: the current directory)")
	all := fs.Bool("all", false, "export every session of the tool, regardless of project")
	sessions := &stringList{}
	fs.Var(sessions, "session", "export only this session id (unique prefix; repeatable)")
	since := fs.String("since", "", "only sessions updated at/after a date (YYYY-MM-DD) or duration (7d, 48h)")
	match := fs.String("match", "", "only sessions whose text contains this substring")
	includeArchived := fs.Bool("include-archived", false, "include Codex's archived_sessions")
	claudeHome := fs.String("claude-home", "", "Claude Code configuration directory (default: $CLAUDE_CONFIG_DIR or ~/.claude)")
	codexHome := fs.String("codex-home", "", "Codex home (default: $CODEX_HOME or ~/.codex)")
	output := fs.String("output", "", "bundle path (default: <project>-sessions-<date>.codexbundle)")
	redact := fs.Bool("redact", false, "mask likely secrets instead of refusing to export them")
	allowSecrets := fs.Bool("allow-secrets", false, "export even though a likely secret was detected")
	encryptTo := &stringList{}
	fs.Var(encryptTo, "encrypt-to", "age recipient (age1… / ssh-ed25519 …; repeatable)")
	recipientsFile := fs.String("recipients-file", "", "file listing age recipients")
	passphrase := fs.Bool("passphrase", false, "encrypt with a passphrase (prompted by age)")
	dryRun := fs.Bool("dry-run", false, "report what would be written, then delete it")
	asJSON := fs.Bool("json", false, "machine-readable summary")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor sessions transfer export — one file with a project's conversations

Selects a tool's sessions (Claude Code by default when installed, else Codex), verifies
them, and writes a .codexbundle that cct can also read. The bundle holds the harness's own
transcripts verbatim and a manifest with a SHA-256 for every member.

  conductor sessions transfer export                      the current project, auto-detected tool
  conductor sessions transfer export --all                every session of that tool
  conductor sessions transfer export --tool claude --project ~/src/app --output app.codexbundle
  conductor sessions transfer export --session 3f9c2a1e  just one conversation
  conductor sessions transfer export --redact             mask API keys instead of refusing

A likely secret stops the export unless --redact or --allow-secrets is given. --encrypt-to
writes <output>.age (via the optional age tool) and removes the plaintext.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all && len(*sessions) > 0 {
		return errors.New("--all and --session are mutually exclusive")
	}
	if *recipientsFile != "" && len(*encryptTo) == 0 {
		// age accepts a recipients file on its own.
	} else if *recipientsFile != "" && len(*encryptTo) > 0 {
		return errors.New("use either --encrypt-to or --recipients-file, not both")
	}
	sinceAt, err := parseSince(*since)
	if err != nil {
		return err
	}
	projectPath := ""
	if !*all && len(*sessions) == 0 {
		projectPath = *project
		if projectPath == "" {
			projectPath = "."
		}
		abs, err := filepath.Abs(projectPath)
		if err != nil {
			return err
		}
		projectPath = abs
	} else if *project != "" {
		abs, err := filepath.Abs(*project)
		if err != nil {
			return err
		}
		projectPath = abs
	}

	out, err := defaultBundlePath(*output, projectPath, *tool)
	if err != nil {
		return err
	}
	if *dryRun {
		tmp, err := os.CreateTemp("", "conductor-transfer-dryrun-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpName)
		out = tmpName
	}

	dir := filepath.Dir(out)
	tmp, err := os.CreateTemp(dir, ".conductor-transfer-*")
	if err != nil {
		return fmt.Errorf("cannot create the bundle beside %s: %w", out, err)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			os.Remove(tmpName)
		}
	}()

	rep, err := transfer.Export(tmp, transfer.ExportOptions{
		Tool:            *tool,
		Project:         projectPath,
		All:             *all,
		Sessions:        *sessions,
		Since:           sinceAt,
		Match:           *match,
		IncludeArchived: *includeArchived,
		ClaudeHome:      *claudeHome,
		CodexHome:       *codexHome,
		Redact:          *redact,
		AllowSecrets:    *allowSecrets,
		Source:          "export",
		Context:         context.Background(),
	})
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmpName, out); err != nil {
		return err
	}
	removeTmp = false

	final := out
	if !*dryRun && (*passphrase || len(*encryptTo) > 0 || *recipientsFile != "") {
		sealed, err := transfer.EncryptFile(out, *encryptTo, *recipientsFile, *passphrase)
		if err != nil {
			return err
		}
		final = sealed
	}

	if *asJSON {
		return emit(struct {
			transfer.Report
			Output string `json:"output"`
		}{rep, final})
	}
	if *dryRun {
		fmt.Printf("Would write %d session(s) (%s) from %s\n", rep.Sessions, humanSize(rep.Bytes), rep.Home)
	} else {
		fmt.Printf("Wrote %d session(s) (%s) from %s to %s\n", rep.Sessions, humanSize(rep.Bytes), rep.Home, final)
	}
	if rep.Redacted > 0 {
		fmt.Printf("Masked %d likely secret(s) with --redact.\n", rep.Redacted)
	}
	for _, w := range rep.Warnings {
		fmt.Printf("  note: %s\n", w)
	}
	if *dryRun {
		os.Remove(final)
	}
	return nil
}

// ---------------------------------------------------------------------------
// import
// ---------------------------------------------------------------------------

func transferImport(args []string) error {
	fs := flag.NewFlagSet("sessions transfer import", flag.ExitOnError)
	tool := fs.String("tool", "", "override the bundle's recorded tool (claude or codex)")
	project := fs.String("project", "", "the project's path on this machine (only warns on a mismatch)")
	sessions := &stringList{}
	fs.Var(sessions, "session", "import only this session id (repeatable)")
	merge := fs.Bool("merge", false, "append the new messages when the local copy is behind (lossless, idempotent)")
	replace := fs.Bool("replace-with-backup", false, "on a conflict, back up the local file and overwrite it")
	asCopy := fs.Bool("import-as-copy", false, "on a conflict, import the bundle's version as a new session")
	mapCwd := &stringList{}
	fs.Var(mapCwd, "map-cwd", "rewrite a recorded working directory: OLD=NEW (repeatable)")
	mapHere := fs.Bool("map-cwd-here", false, "map the bundle's project to the current directory")
	claudeHome := fs.String("claude-home", "", "Claude Code configuration directory")
	codexHome := fs.String("codex-home", "", "Codex home")
	identity := fs.String("identity", "", "age identity file, for an encrypted bundle")
	passphrase := fs.Bool("passphrase", false, "the bundle is passphrase-encrypted (age prompts)")
	dryRun := fs.Bool("dry-run", false, "validate and print the plan; write nothing")
	to := fs.String("to", "", "translate into another harness instead: claude, codex, or opencode")
	cwd := fs.String("cwd", "", "target working directory for a translation (default: the recorded cwd)")
	force := fs.Bool("force", false, "replace an existing translated session whose bytes differ")
	asJSON := fs.Bool("json", false, "machine-readable plan/result")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor sessions transfer import — copy a bundle's sessions into this machine

Verifies every checksum before writing anything. Each session is classified: new,
identical, grown (the local copy is a prefix), already ahead, or diverged. Nothing is
overwritten by default; a diverged session is reported as a conflict.

  conductor sessions transfer import app.codexbundle
  conductor sessions transfer import app.codexbundle --dry-run
  conductor sessions transfer import app.codexbundle --merge          catch up a growing chat
  conductor sessions transfer import app.codexbundle --map-cwd-here   re-home the project here
  conductor sessions transfer import app.codexbundle --map-cwd /old/app=/new/app

Cross-harness: --to renders each conversation into the target's own session format, so it
opens there as a real session. Text is preserved; tool calls become one-line notes and no
model or runtime state crosses over.

  conductor sessions transfer import app.codexbundle --to codex
  conductor sessions transfer import codex.codexbundle --to claude --cwd ~/src/app
  conductor sessions transfer import claude.codexbundle --to opencode --cwd ~/src/app

Flags:
`)
		fs.PrintDefaults()
	}
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: conductor sessions transfer import <bundle> [flags]")
	}
	if *replace && *asCopy {
		return errors.New("--replace-with-backup and --import-as-copy are mutually exclusive")
	}
	if *to != "" && (*merge || *replace || *asCopy) {
		return errors.New("--to translates into a new session; --merge, --replace-with-backup, and --import-as-copy do not apply")
	}
	var mappings []transfer.Mapping
	for _, raw := range *mapCwd {
		i := strings.Index(raw, "=")
		if i <= 0 || i == len(raw)-1 {
			return fmt.Errorf("--map-cwd needs OLD=NEW, got %q", raw)
		}
		mappings = append(mappings, transfer.Mapping{Old: raw[:i], New: raw[i+1:]})
	}

	b, cleanup, err := transfer.OpenFile(pos[0], transfer.OpenOptions{Identity: *identity, Passphrase: *passphrase})
	if err != nil {
		return err
	}
	defer cleanup()
	defer b.Close()

	if *to != "" {
		tres, err := transfer.TranslateImport(context.Background(), b, transfer.TranslateOptions{
			Target:     *to,
			Cwd:        *cwd,
			ClaudeHome: *claudeHome,
			CodexHome:  *codexHome,
			Sessions:   *sessions,
			DryRun:     *dryRun,
			Force:      *force,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return emit(tres)
		}
		printTranslateResult(tres)
		return nil
	}

	res, err := transfer.Import(b, transfer.ImportOptions{
		Tool:              *tool,
		ClaudeHome:        *claudeHome,
		CodexHome:         *codexHome,
		Project:           *project,
		Sessions:          *sessions,
		Merge:             *merge,
		ReplaceWithBackup: *replace,
		ImportAsCopy:      *asCopy,
		MapCwd:            mappings,
		MapCwdHere:        *mapHere,
		DryRun:            *dryRun,
		Context:           context.Background(),
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(res)
	}
	printImportResult(res)
	return nil
}

func printImportResult(res transfer.ImportResult) {
	if res.DryRun {
		fmt.Printf("Dry run: %s\n", res.Bundle)
	} else {
		fmt.Printf("Imported from %s\n", res.Bundle)
	}
	groups := map[transfer.Action][]transfer.Item{}
	for _, it := range res.Items {
		groups[it.Action] = append(groups[it.Action], it)
	}
	order := []transfer.Action{
		transfer.ActionImport, transfer.ActionUpdate, transfer.ActionReplace, transfer.ActionImportCopy,
		transfer.ActionSkipAhead, transfer.ActionSkipIdentical, transfer.ActionSkipDeselected, transfer.ActionConflict,
	}
	labels := map[transfer.Action]string{
		transfer.ActionImport:         "New",
		transfer.ActionUpdate:         "Updated (new messages appended)",
		transfer.ActionReplace:        "Replaced (backup kept)",
		transfer.ActionImportCopy:     "Imported as a copy",
		transfer.ActionSkipAhead:      "Already up to date",
		transfer.ActionSkipIdentical:  "Identical",
		transfer.ActionSkipDeselected: "Not selected",
		transfer.ActionConflict:       "Conflicts (skipped)",
	}
	for _, a := range order {
		items := groups[a]
		if len(items) == 0 {
			continue
		}
		extra := ""
		if a == transfer.ActionUpdate {
			lines := 0
			for _, it := range items {
				lines += it.LinesAdded
			}
			extra = fmt.Sprintf(" (+%d lines)", lines)
		}
		fmt.Printf("%s: %d%s\n", labels[a], len(items), extra)
		if a == transfer.ActionConflict || a == transfer.ActionImportCopy || a == transfer.ActionReplace {
			for _, it := range items {
				if it.Note != "" {
					fmt.Printf("  - %s: %s\n", it.BundlePath, it.Note)
				}
			}
		}
	}
	if res.DryRun {
		fmt.Println("\nNothing was written. Re-run without --dry-run to apply.")
	} else {
		fmt.Printf("\n%d session(s) now available; restart the agent so it re-discovers them.\n", len(res.Items)-res.Skipped)
	}
}

func printTranslateResult(res transfer.TranslateResult) {
	verb := "Translated"
	if res.DryRun {
		verb = "Would translate"
	}
	fmt.Printf("%s %s -> %s\n", verb, res.SourceTool, res.TargetTool)
	for _, it := range res.Items {
		switch it.Action {
		case "translated":
			fmt.Printf("  + %s -> %s  (%d turns)\n", it.SourceID, it.DestPath, it.Turns)
		case "conflict":
			fmt.Printf("  ! %s: %s\n", it.BundlePath, it.Reason)
		case "skip":
			fmt.Printf("  - %s: %s\n", it.BundlePath, it.Reason)
		}
	}
	fmt.Printf("\n%d translated, %d skipped.\n", res.Translated, res.Skipped)
	if res.DryRun {
		fmt.Println("Nothing was written. Re-run without --dry-run to apply.")
	} else if res.Translated > 0 {
		fmt.Println("The target harness re-discovers the session on its next start; the conversation opens with a handoff note.")
	}
}

// ---------------------------------------------------------------------------
// inspect / scan
// ---------------------------------------------------------------------------

func transferInspect(args []string) error {
	fs := flag.NewFlagSet("sessions transfer inspect", flag.ExitOnError)
	scan := fs.Bool("scan", false, "also scan the transcripts for likely secrets")
	identity := fs.String("identity", "", "age identity file, for an encrypted bundle")
	passphrase := fs.Bool("passphrase", false, "the bundle is passphrase-encrypted (age prompts)")
	asJSON := fs.Bool("json", false, "machine-readable report")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: conductor sessions transfer inspect <bundle> [--scan]")
	}
	b, cleanup, err := transfer.OpenFile(pos[0], transfer.OpenOptions{Identity: *identity, Passphrase: *passphrase})
	if err != nil {
		return err
	}
	defer cleanup()
	defer b.Close()
	rep, err := transfer.Inspect(b, *scan)
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(rep)
	}
	fmt.Printf("%s\n", rep.Summary())
	fmt.Printf("  created  %s on %s (%s)\n", rep.CreatedAt, orDash(rep.Device), rep.FormatVersion)
	fmt.Printf("  source   %s\n", orDash(rep.SourceHome))
	for _, s := range rep.Sessions {
		fmt.Printf("  %-8s %-36s %s  %s\n", s.ThreadID, s.BundlePath, humanSize(s.SizeBytes), orDash(s.Preview))
	}
	if rep.SecretsFound > 0 {
		fmt.Printf("\n%d likely secret(s) found; do not share this bundle until they are removed.\n", rep.SecretsFound)
	}
	for _, p := range rep.MissingProjects {
		fmt.Printf("\nProject %s does not exist here. To re-home it:\n  --map-cwd %s=%s\n", p, p, filepath.Join(mustGetwd(), filepath.Base(p)))
	}
	return nil
}

func transferScan(args []string) error {
	fs := flag.NewFlagSet("sessions transfer scan", flag.ExitOnError)
	identity := fs.String("identity", "", "age identity file, for an encrypted bundle")
	passphrase := fs.Bool("passphrase", false, "the bundle is passphrase-encrypted (age prompts)")
	asJSON := fs.Bool("json", false, "machine-readable report")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: conductor sessions transfer scan <bundle>")
	}
	b, cleanup, err := transfer.OpenFile(pos[0], transfer.OpenOptions{Identity: *identity, Passphrase: *passphrase})
	if err != nil {
		return err
	}
	defer cleanup()
	defer b.Close()

	type result struct {
		Bundle  string `json:"bundle"`
		Secrets int    `json:"secrets"`
	}
	n, err := transfer.ScanBundle(b)
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(result{Bundle: pos[0], Secrets: n})
	}
	if n == 0 {
		fmt.Printf("%s: no likely secrets found.\n", pos[0])
		return nil
	}
	fmt.Printf("%s: %d likely secret(s) found. Do not share it without redacting them.\n", pos[0], n)
	return nil
}

// ---------------------------------------------------------------------------
// LAN sync
// ---------------------------------------------------------------------------

func transferServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions transfer serve", flag.ExitOnError)
	addr := fs.String("addr", "0.0.0.0:0", "address to listen on (local network)")
	tool := fs.String("tool", "", "which agent to offer (default: auto-detect)")
	project := fs.String("project", "", "offer only this project's sessions")
	since := fs.String("since", "", "offer only sessions updated since a date or duration")
	claudeHome := fs.String("claude-home", "", "Claude Code configuration directory")
	codexHome := fs.String("codex-home", "", "Codex home")
	dryRun := fs.Bool("dry-run", false, "receive and report without writing")
	understand := fs.Bool("i-understand", false, "acknowledge that this sends session data over the local network")
	asJSON := fs.Bool("json", false, "machine-readable summary")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor sessions transfer serve — offer sessions to one paired device

This is the only transfer command that sends session data off the machine. It is
peer-to-peer, TLS, local-network only, and gated by a one-time pairing code. It refuses to
talk to a non-private address.

  conductor sessions transfer serve --i-understand
  conductor sessions transfer serve --i-understand --project ~/src/app

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*understand {
		return errors.New("LAN sync sends session data over your network; re-run with --i-understand")
	}
	sinceAt, err := parseSince(*since)
	if err != nil {
		return err
	}
	out := io.Writer(os.Stdout)
	if *asJSON {
		out = io.Discard
	}
	rep, err := transfer.ServeSync(ctx, *addr, transfer.SyncOptions{
		Tool: *tool, Project: *project, Since: sinceAt,
		ClaudeHome: *claudeHome, CodexHome: *codexHome, DryRun: *dryRun,
	}, out)
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(rep)
	}
	fmt.Printf("Sent %d session(s), received %d. Conflicts: %d\n", rep.Sent, rep.Received, rep.Import.Conflicts)
	for _, w := range rep.Warnings {
		fmt.Printf("  note: %s\n", w)
	}
	return nil
}

func transferConnect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions transfer connect", flag.ExitOnError)
	code := fs.String("code", "", "pairing code (default: prompt, so it stays out of shell history)")
	direction := fs.String("direction", "both", "both, pull (receive only), or push (send only)")
	tool := fs.String("tool", "", "which agent to offer (default: auto-detect)")
	project := fs.String("project", "", "offer only this project's sessions")
	since := fs.String("since", "", "offer only sessions updated since a date or duration")
	claudeHome := fs.String("claude-home", "", "Claude Code configuration directory")
	codexHome := fs.String("codex-home", "", "Codex home")
	dryRun := fs.Bool("dry-run", false, "receive and report without writing")
	understand := fs.Bool("i-understand", false, "acknowledge that this sends session data over the local network")
	asJSON := fs.Bool("json", false, "machine-readable summary")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: conductor sessions transfer connect HOST:PORT --i-understand [--direction both|pull|push]")
	}
	if !*understand {
		return errors.New("LAN sync sends session data over your network; re-run with --i-understand")
	}
	var dir transfer.Direction
	switch *direction {
	case "both", "":
		dir = transfer.SyncBoth
	case "pull":
		dir = transfer.SyncPull
	case "push":
		dir = transfer.SyncPush
	default:
		return fmt.Errorf("--direction must be both, pull, or push, got %q", *direction)
	}
	sinceAt, err := parseSince(*since)
	if err != nil {
		return err
	}
	rep, err := transfer.ConnectSync(ctx, pos[0], *code, transfer.SyncOptions{
		Tool: *tool, Project: *project, Since: sinceAt,
		ClaudeHome: *claudeHome, CodexHome: *codexHome, DryRun: *dryRun, Direction: dir,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(rep)
	}
	fmt.Printf("Sent %d session(s), received %d. Conflicts: %d\n", rep.Sent, rep.Received, rep.Import.Conflicts)
	for _, w := range rep.Warnings {
		fmt.Printf("  note: %s\n", w)
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseSince(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// time.ParseDuration has no day or week unit; accept the forms people actually type.
	if n := len(s); n > 1 {
		if d, err := strconv.ParseFloat(s[:n-1], 64); err == nil {
			switch s[n-1] {
			case 'd':
				return time.Now().Add(-time.Duration(d * 24 * float64(time.Hour))), nil
			case 'w':
				return time.Now().Add(-time.Duration(d * 7 * 24 * float64(time.Hour))), nil
			}
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("--since: give a date (2026-11-01), a duration (48h), or days/weeks (7d, 2w)")
}

func defaultBundlePath(output, project, tool string) (string, error) {
	if output != "" {
		return output, nil
	}
	base := tool
	if base == "" {
		base = "codex"
	}
	if project != "" {
		if b := filepath.Base(project); b != "" && b != "." && b != string(filepath.Separator) {
			base = b
		}
	}
	name := fmt.Sprintf("%s-sessions-%s.codexbundle", strings.ReplaceAll(base, " ", "-"), time.Now().Format("20060102"))
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, name), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
