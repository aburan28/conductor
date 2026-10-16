package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/checkpoint"
	"github.com/aburan28/conductor/internal/localstate"
	"github.com/aburan28/conductor/internal/usage"
)

type migrateStrings []string

func (v *migrateStrings) String() string { return strings.Join(*v, ",") }
func (v *migrateStrings) Set(s string) error {
	*v = append(*v, s)
	return nil
}

// migrationReceipt stays on the owner's machine. It contains identifiers and hashes,
// never chat text; the native transcript remains in the owner-only checkpoint bundle.
type migrationReceipt struct {
	Schema                  int                           `json:"schema"`
	Source                  checkpoint.ClaudeInventoryRow `json:"source"`
	SourceStatus            string                        `json:"source_status"`
	CheckpointID            string                        `json:"checkpoint_id,omitempty"`
	CheckpointPath          string                        `json:"checkpoint_path,omitempty"`
	CheckpointSHA           string                        `json:"checkpoint_sha256,omitempty"`
	Workspace               checkpoint.Workspace          `json:"workspace"`
	Conductor               checkpoint.ConductorRef       `json:"conductor,omitempty"`
	TargetHarness           string                        `json:"target_harness"`
	TargetDir               string                        `json:"target_dir,omitempty"`
	TargetThreadID          string                        `json:"target_thread_id,omitempty"`
	PreviousTargetThreadIDs []string                      `json:"previous_target_thread_ids,omitempty"`
	Continuation            string                        `json:"continuation,omitempty"`
	State                   string                        `json:"state"` // checkpointed, thread_created, ready, failed
	Error                   string                        `json:"error,omitempty"`
	PreviousErrors          []string                      `json:"previous_errors,omitempty"`
	CapturedAt              time.Time                     `json:"captured_at,omitzero"`
	ReadyAt                 time.Time                     `json:"ready_at,omitzero"`
	TranscriptOnly          bool                          `json:"transcript_only"`
	NativeAvailable         bool                          `json:"native_available"`
}

func checkpointMigrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("checkpoint migrate", flag.ContinueOnError)
	var roots, maps migrateStrings
	fs.Var(&roots, "source-state-dir", "Claude configuration directory to scan (repeatable; default: ~/.claude and ~/.claude-*)")
	fs.Var(&maps, "map", "map a source cwd prefix to a local target prefix: OLD=NEW (repeatable)")
	session := fs.String("session", "", "exact Claude Code session UUID to select")
	all := fs.Bool("all", false, "select every matching source conversation")
	since := fs.String("since", "", "only transcripts active on or after YYYY-MM-DD or RFC3339 time")
	target := fs.String("to", "codex", "target harness (currently codex)")
	dir := fs.String("dir", "", "local target checkout for one --session")
	codexHome := fs.String("codex-home", "", "Codex state directory for created threads (default: $CODEX_HOME or ~/.codex)")
	execute := fs.Bool("execute", false, "checkpoint and start a verified Codex thread for each selected chat")
	captureOnly := fs.Bool("capture-only", false, "capture exact source snapshots now; create Codex threads on a later --execute")
	transcriptOnly := fs.Bool("transcript-only", false, "explicitly omit workspace snapshot (needed for an inaccessible remote checkout)")
	asJSON := fs.Bool("json", false, "machine-readable inventory or receipt rows")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor checkpoint migrate — inventory Claude chats and move selected chats to Codex

Without --execute this is read-only inventory. Desktop remote-control mirrors under
projects/ssh-*/ are included. A migration leaves the Claude session running, captures
its exact transcript, and starts one read-only Codex orientation turn. The Codex thread
is then ready for an interactive continuation. Use --map or --dir when the source cwd
is on another host; --transcript-only explicitly records the missing workspace snapshot.

  conductor checkpoint migrate --all --since 2026-10-16 --json
  conductor checkpoint migrate --session UUID --dir /path/to/checkout --transcript-only --execute
  conductor checkpoint migrate --all --map /home/me/src=/local/src --execute

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) != 0 || (!*all && *session == "") || (*all && *session != "") {
		return errors.New("choose exactly one of --session UUID or --all")
	}
	if *target != "codex" {
		return fmt.Errorf("migration target %q is unsupported; use codex", *target)
	}
	if *dir != "" && *all {
		return errors.New("--dir is for one --session; use --map for --all")
	}
	cutoff, err := migrationCutoff(*since)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		roots = defaultClaudeRoots()
	}
	rows, err := migrationInventory(roots, *session, cutoff)
	if err != nil {
		return err
	}
	records, _ := localstate.List()
	for i := range rows {
		rows[i].Status, _ = migrationSourceStatus(rows[i], records)
	}
	if !*execute && !*captureOnly {
		if *asJSON {
			if err := emit(rows); err != nil {
				return err
			}
		} else {
			for _, row := range rows {
				fmt.Printf("%-36s %-7s %s  %s\n", row.SessionID, row.Status,
					row.LastActivity.Local().Format("2006-01-02 15:04"), row.Title)
				if row.Problem != "" {
					fmt.Printf("  problem: %s\n", row.Problem)
				}
			}
			fmt.Printf("%d Claude conversation(s); use --execute to create Codex continuations.\n", len(rows))
		}
		for _, row := range rows {
			if row.Problem != "" {
				return errors.New("inventory contains transcripts that could not be validated; inspect problem rows")
			}
		}
		return nil
	}
	if len(rows) == 0 {
		return errors.New("no source conversations matched")
	}
	if *codexHome != "" && *execute {
		resolved, err := filepath.Abs(*codexHome)
		if err != nil {
			return err
		}
		if st, err := os.Stat(resolved); err != nil || !st.IsDir() {
			return fmt.Errorf("Codex home %s is unavailable", resolved)
		}
		previous, hadPrevious := os.LookupEnv("CODEX_HOME")
		if err := os.Setenv("CODEX_HOME", resolved); err != nil {
			return err
		}
		defer func() {
			if hadPrevious {
				_ = os.Setenv("CODEX_HOME", previous)
			} else {
				_ = os.Unsetenv("CODEX_HOME")
			}
		}()
	}
	mapping, err := parseMigrationMaps(maps)
	if err != nil {
		return err
	}
	var receipts []migrationReceipt
	var failed int
	for _, row := range rows {
		cwd := migrationTargetDir(row.Cwd, *dir, mapping)
		var result migrationReceipt
		var err error
		if row.Problem != "" {
			result = migrationReceipt{Schema: 1, Source: row, SourceStatus: "unknown", TargetHarness: "codex"}
			err = fmt.Errorf("invalid source transcript: %s", row.Problem)
		} else {
			result, err = migrateClaudeRow(ctx, row, cwd, *transcriptOnly, *captureOnly, records)
		}
		if err != nil {
			result.State, result.Error = "failed", err.Error()
			failed++
		}
		receipts = append(receipts, result)
		if !*asJSON {
			fmt.Printf("%-36s %-14s %s", row.SessionID, result.State, result.TargetThreadID)
			if err != nil {
				fmt.Printf("  %v", err)
			}
			fmt.Println()
		}
	}
	if *asJSON {
		if err := emit(receipts); err != nil {
			return err
		}
	}
	if failed > 0 {
		if *captureOnly {
			return fmt.Errorf("%d of %d Claude conversations could not be checkpointed", failed, len(rows))
		}
		return fmt.Errorf("%d of %d Claude conversations did not reach a verified Codex thread", failed, len(rows))
	}
	return nil
}

func migrationCutoff(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (use YYYY-MM-DD or RFC3339)", s)
}

func defaultClaudeRoots() migrateStrings {
	main := usage.ClaudeConfigDir(os.Getenv)
	out := migrateStrings{main}
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	paths, _ := filepath.Glob(filepath.Join(home, ".claude-*"))
	for _, p := range paths {
		if p != main {
			if st, err := os.Stat(filepath.Join(p, "projects")); err == nil && st.IsDir() {
				out = append(out, p)
			}
		}
	}
	return out
}

func migrationInventory(roots []string, session string, since time.Time) ([]checkpoint.ClaudeInventoryRow, error) {
	seen := make(map[string]bool)
	var rows []checkpoint.ClaudeInventoryRow
	for _, root := range roots {
		root, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		part, err := checkpoint.InventoryClaude(root, session)
		if err != nil {
			return nil, err
		}
		for _, row := range part {
			if (session == "" || row.SessionID == session) && (since.IsZero() || !row.LastActivity.Before(since)) {
				rows = append(rows, row)
			}
		}
	}
	if session != "" && len(rows) != 1 {
		return nil, fmt.Errorf("Claude session %s matched %d transcripts; choose one source-state-dir", session, len(rows))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return rows, nil
}

type migrationMap struct{ old, new string }

func parseMigrationMaps(values []string) ([]migrationMap, error) {
	var out []migrationMap
	for _, value := range values {
		old, target, ok := strings.Cut(value, "=")
		if !ok || !filepath.IsAbs(old) || !filepath.IsAbs(target) {
			return nil, fmt.Errorf("invalid --map %q: both sides must be absolute paths", value)
		}
		out = append(out, migrationMap{filepath.Clean(old), filepath.Clean(target)})
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].old) > len(out[j].old) })
	return out, nil
}

func migrationTargetDir(source, explicit string, mappings []migrationMap) string {
	if explicit != "" {
		p, _ := filepath.Abs(explicit)
		return p
	}
	for _, m := range mappings {
		if source == m.old {
			return m.new
		}
		if strings.HasPrefix(source, m.old+string(filepath.Separator)) {
			return filepath.Join(m.new, strings.TrimPrefix(source, m.old+string(filepath.Separator)))
		}
	}
	return source
}

func migrateClaudeRow(ctx context.Context, row checkpoint.ClaudeInventoryRow, targetDir string, transcriptOnly, captureOnly bool, records []localstate.Record) (migrationReceipt, error) {
	status, ref := migrationSourceStatus(row, records)
	receipt := migrationReceipt{Schema: 1, Source: row, SourceStatus: status,
		TargetHarness: "codex", TargetDir: targetDir, TranscriptOnly: transcriptOnly, Conductor: ref}
	file, err := migrationReceiptPath(row)
	if err != nil {
		return receipt, err
	}
	if old, err := readMigrationReceipt(file); err == nil {
		if old.State == "ready" {
			if captureOnly {
				return old, nil
			}
			if digest, err := migrationHashFile(old.CheckpointPath); err != nil || digest != old.CheckpointSHA {
				return old, errors.New("previous migration checkpoint is missing or changed")
			}
			verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := checkpoint.VerifyCodexThread(verifyCtx, old.TargetThreadID, old.TargetDir); err != nil {
				return old, fmt.Errorf("previous Codex thread is no longer resumable: %w", err)
			}
			return old, nil
		}
		receipt = old
	}
	receipt.TargetDir = targetDir
	if receipt.CheckpointID == "" {
		if !transcriptOnly {
			if st, err := os.Stat(row.Cwd); err != nil || !st.IsDir() {
				return receipt, fmt.Errorf("source workspace %s is unavailable; run on its host or explicitly use --transcript-only", row.Cwd)
			}
		}
		receipt.TranscriptOnly = transcriptOnly
		result, err := checkpoint.Capture(ctx, checkpoint.Request{
			Harness: "claude", SessionID: row.SessionID, Cwd: row.Cwd,
			ClaudePath: row.Path, ClaudeConfigDir: row.ConfigDir,
			Reason: "migration", SkipWorkspace: transcriptOnly, Force: true,
			Conductor: ref,
		})
		if err != nil {
			return receipt, err
		}
		if got := transcriptHash(result.Manifest); got != row.SHA256 {
			return receipt, errors.New("source transcript changed before checkpoint completed; retry")
		}
		if got, count := checkpoint.ClaudeManifestExtrasDigest(result.Manifest, row.SessionID); got != row.ExtrasSHA256 || count != row.Extras {
			return receipt, errors.New("Claude session sidecars changed or were omitted during checkpoint; retry")
		}
		receipt.CheckpointID = result.Manifest.ID
		receipt.CheckpointPath = result.Path
		receipt.CheckpointSHA, err = migrationHashFile(result.Path)
		if err != nil {
			return receipt, err
		}
		receipt.Workspace = result.Manifest.Workspace
		receipt.CapturedAt = result.Manifest.CreatedAt
		receipt.NativeAvailable = true
		receipt.State = "checkpointed"
		if err := saveMigrationReceipt(file, receipt); err != nil {
			return receipt, err
		}
	}
	if captureOnly {
		if receipt.State == "failed" && receipt.CheckpointID != "" {
			if receipt.Error != "" {
				receipt.PreviousErrors = append(receipt.PreviousErrors, receipt.Error)
			}
			receipt.State, receipt.Error = "checkpointed", ""
			if err := saveMigrationReceipt(file, receipt); err != nil {
				return receipt, err
			}
		}
		return receipt, nil
	}
	st, err := os.Stat(targetDir)
	if err != nil || !st.IsDir() {
		return receipt, fmt.Errorf("target checkout %s is unavailable; use --dir or --map", targetDir)
	}
	if digest, err := migrationHashFile(row.Path); err != nil || digest != row.SHA256 {
		return receipt, errors.New("source transcript changed after checkpoint; retry with a new snapshot")
	}
	if row.Extras > 0 {
		src, err := checkpoint.LocateClaudePath(row.ConfigDir, row.Path, row.SessionID)
		if err != nil {
			return receipt, err
		}
		digest, err := checkpoint.ClaudeExtrasDigest(src.Extras)
		if err != nil || digest != row.ExtrasSHA256 {
			return receipt, errors.New("Claude session sidecars changed after checkpoint; retry with a new snapshot")
		}
	}
	if digest, err := migrationHashFile(receipt.CheckpointPath); err != nil || digest != receipt.CheckpointSHA {
		return receipt, errors.New("checkpoint bytes changed after capture")
	}
	bundle, err := checkpoint.OpenFile(receipt.CheckpointPath, nil)
	if err != nil {
		return receipt, err
	}
	data, ok := bundle.File(checkpoint.ContinuationPath)
	if !ok {
		return receipt, errors.New("checkpoint has no continuation")
	}
	continuation := filepath.Join(filepath.Dir(file), strings.TrimSuffix(filepath.Base(file), ".json")+".md")
	if err := writeMigrationPrivate(continuation, data); err != nil {
		return receipt, err
	}
	receipt.Continuation = continuation
	if err := saveMigrationReceipt(file, receipt); err != nil {
		return receipt, err
	}
	prompt := fmt.Sprintf("This is a Conductor migration of Claude Code session %s. Read %s and the current checkout. The full source transcript is at %s; the checkpoint is %s. In this first turn, report the original objective, verified current status, and next action. Do not edit files, launch experiments, or claim completion. The source may have continued after the snapshot at %s; flag any uncertainty. Stop after the status report.",
		row.SessionID, continuation, row.Path, receipt.CheckpointPath, receipt.CapturedAt.Format(time.RFC3339))
	turnCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	id, err := checkpoint.CodexBootstrap(turnCtx, targetDir, row.Title, prompt, receipt.TargetThreadID, func(id string) error {
		if receipt.TargetThreadID != "" && receipt.TargetThreadID != id {
			receipt.PreviousTargetThreadIDs = append(receipt.PreviousTargetThreadIDs, receipt.TargetThreadID)
		}
		receipt.TargetThreadID, receipt.State = id, "thread_created"
		return saveMigrationReceipt(file, receipt)
	})
	if id != "" {
		receipt.TargetThreadID = id
	}
	if err != nil {
		receipt.Error, receipt.State = err.Error(), "failed"
		_ = saveMigrationReceipt(file, receipt)
		return receipt, err
	}
	if err := checkpoint.VerifyCodexThread(turnCtx, id, targetDir); err != nil {
		receipt.Error, receipt.State = err.Error(), "failed"
		_ = saveMigrationReceipt(file, receipt)
		return receipt, err
	}
	receipt.State, receipt.Error, receipt.ReadyAt = "ready", "", time.Now().UTC()
	if err := saveMigrationReceipt(file, receipt); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func migrationSourceStatus(row checkpoint.ClaudeInventoryRow, records []localstate.Record) (string, checkpoint.ConductorRef) {
	for _, rec := range records {
		if checkpoint.NormalizeHarness(rec.Harness) != "claude" || rec.PID <= 0 {
			continue
		}
		if id, ok := checkpoint.ClaudeSessionForPID(row.ConfigDir, rec.PID); ok && id == row.SessionID {
			return string(rec.Status), checkpoint.ConductorRef{Project: rec.Project, SessionID: rec.SessionID}
		}
	}
	return "unknown", checkpoint.ConductorRef{}
}

func transcriptHash(m checkpoint.Manifest) string {
	for _, f := range m.Files {
		if f.Path == m.Transcript.Path {
			return f.SHA256
		}
	}
	return ""
}

func migrationHashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func migrationReceiptPath(row checkpoint.ClaudeInventoryRow) (string, error) {
	dir, err := checkpoint.Dir()
	if err != nil {
		return "", err
	}
	key := row.ConfigDir + "\x00" + row.Path + "\x00" + row.SHA256
	if row.ExtrasSHA256 != "" {
		key += "\x00" + row.ExtrasSHA256
	}
	h := sha256.Sum256([]byte(key))
	return filepath.Join(filepath.Dir(dir), "migrations", hex.EncodeToString(h[:16])+".json"), nil
}

func readMigrationReceipt(path string) (migrationReceipt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return migrationReceipt{}, err
	}
	var r migrationReceipt
	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	return r, nil
}

func saveMigrationReceipt(path string, r migrationReceipt) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeMigrationPrivate(path, append(data, '\n'))
}

func writeMigrationPrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".migration-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
