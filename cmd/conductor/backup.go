package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/adamburan/conductor/internal/backup"
	"github.com/adamburan/conductor/internal/localstate"
)

// Off-host backup of this machine's resume records.
//
// `conductor sessions save` already keeps a session resumable past a closed terminal or a
// reboot — but only while the machine's disk survives. On an ephemeral cloud instance the disk
// (and ~/.conductor/sessions with it) is gone when the instance is terminated, so "resume
// after a reboot" needs the records to live somewhere else. This pushes them to S3, keyed by
// machine, and pulls them back onto a fresh instance so `conductor resume` can reopen the
// conversations there. Only coordination metadata travels — how to reopen a session: harness,
// working directory, and resume invocation. The harness argv is redacted before upload
// (redactForBackup) because it can carry a first-turn prompt, and a transcript never leaves
// the harness's own local store in the first place.

// backupManifest is what is written to S3: the machine's session records plus enough context
// to know whose they are and when they were captured.
type backupManifest struct {
	Machine    string              `json:"machine"`
	CapturedAt time.Time           `json:"captured_at"`
	Count      int                 `json:"count"`
	Records    []localstate.Record `json:"records"`
}

func cmdBackup(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: conductor backup <push|pull|status>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "push":
		return backupPush(ctx, rest)
	case "pull":
		return backupPull(ctx, rest)
	case "status":
		return backupStatus(ctx, rest)
	default:
		return fmt.Errorf("unknown backup subcommand %q", sub)
	}
}

// openBackup builds the configured Store, or explains that backup is not configured.
func openBackup() (*backup.Store, error) {
	store, enabled, err := backup.FromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, errors.New("off-host backup is not configured. Set CONDUCTOR_BACKUP_S3_BUCKET " +
			"(and AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, plus CONDUCTOR_BACKUP_S3_REGION) to enable it")
	}
	return store, nil
}

func backupPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup push", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor backup push — copy this machine's resume records to S3

Bundles every saved/paused/running session record under ~/.conductor/sessions and uploads it,
keyed by machine, plus a timestamped snapshot. Configure with CONDUCTOR_BACKUP_S3_BUCKET and
the AWS_* credentials. Only resume metadata is uploaded — the harness argv (which can carry a
prompt) is stripped first, and a transcript never leaves the harness.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := openBackup()
	if err != nil {
		return err
	}
	n, err := pushRecords(ctx, store, time.Now().UTC())
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(map[string]any{"pushed": n, "location": store.Location()})
	}
	fmt.Printf("Pushed %d session record(s) to %s\n", n, store.Location())
	return nil
}

// pushRecords bundles the machine's current records and uploads them. Shared by the CLI, the
// auto-push in `sessions save`, and the shutdown hook.
func pushRecords(ctx context.Context, store *backup.Store, at time.Time) (int, error) {
	records, err := localstate.Prune()
	if err != nil {
		return 0, err
	}
	records = redactForBackup(records)
	manifest := backupManifest{Machine: store.Machine(), CapturedAt: at, Count: len(records), Records: records}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := store.PutSessions(ctx, data, at); err != nil {
		return 0, err
	}
	return len(records), nil
}

// redactForBackup strips the fields that could carry private content before a record leaves
// the machine. The harness argv (Args) and command line can contain a user's first-turn
// prompt — the localstate package writes records 0600 for exactly that reason — and resume
// does not need them: it relaunches from ResumeArgs (`--continue`) plus the wrap flags. What
// remains is coordination metadata only: harness, cwd, ids, and how to reopen the session.
// A harness with no known resume invocation loses its fallback argv when restored off-host,
// which is the right trade against uploading a prompt to a shared bucket.
func redactForBackup(records []localstate.Record) []localstate.Record {
	out := make([]localstate.Record, len(records))
	for i, r := range records {
		r.Args = nil
		r.Command = ""
		out[i] = r
	}
	return out
}

func backupPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup pull", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite local records that already exist")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor backup pull — restore this machine's resume records from S3

Downloads the records this machine last pushed and writes them under ~/.conductor/sessions, so
`+"`conductor resume`"+` can reopen the conversations here — the point being a fresh instance that
replaced a terminated one. Existing local records are left alone unless --force. To restore
another machine's sessions, set CONDUCTOR_MACHINE_ID to that machine's id before pulling.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := openBackup()
	if err != nil {
		return err
	}
	restored, err := pullRecords(ctx, store, *force)
	if errors.Is(err, backup.ErrNotFound) {
		fmt.Println("Nothing to restore: this machine has no records in S3 yet.")
		return nil
	}
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(map[string]any{"restored": restored})
	}
	fmt.Printf("Restored %d session record(s). `conductor resume` reopens them.\n", restored)
	return nil
}

// pullRecords downloads the manifest and writes each record locally. A record whose id already
// exists locally is skipped unless force, so a pull cannot clobber a live session's record.
func pullRecords(ctx context.Context, store *backup.Store, force bool) (int, error) {
	data, err := store.GetSessions(ctx)
	if err != nil {
		return 0, err
	}
	var manifest backupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return 0, fmt.Errorf("backup: manifest is corrupt: %w", err)
	}
	restored := 0
	for _, r := range manifest.Records {
		if !force {
			if _, exists := localstate.Get(r.ID); exists {
				continue
			}
		}
		// A restored record's process is gone (it was on another machine), so it comes back as
		// saved — exactly the state `conductor resume` reopens.
		if err := localstate.KeepForResume(r); err != nil {
			return restored, err
		}
		restored++
	}
	return restored, nil
}

func backupStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, enabled, err := backup.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	if !enabled {
		if *asJSON {
			return emit(map[string]any{"enabled": false})
		}
		fmt.Println("Off-host backup: not configured.")
		fmt.Println("Enable it by setting CONDUCTOR_BACKUP_S3_BUCKET, CONDUCTOR_BACKUP_S3_REGION,")
		fmt.Println("and AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY. Then `conductor backup push`.")
		return nil
	}
	snaps, err := store.ListSnapshots(ctx)
	if err != nil {
		return fmt.Errorf("reaching S3: %w", err)
	}
	last := "never"
	if len(snaps) > 0 {
		last = snaps[len(snaps)-1]
	}
	if *asJSON {
		return emit(map[string]any{
			"enabled": true, "location": store.Location(), "machine": store.Machine(),
			"snapshots": len(snaps), "latest_snapshot": last,
		})
	}
	fmt.Printf("Off-host backup: %s\n", store.Describe())
	fmt.Printf("  manifest   %s\n", store.Location())
	fmt.Printf("  snapshots  %d (latest: %s)\n", len(snaps), last)
	return nil
}

// backupOnShutdown pushes the machine's records to S3 when a wrapped session is going down,
// so even a terminated cloud instance leaves its sessions resumable elsewhere. It is
// best-effort and tightly bounded: a shutdown must not wait on the network, and a machine
// with no backup configured does nothing at all.
func backupOnShutdown(_ localstate.Record) {
	store, enabled, err := backup.FromEnv(os.Getenv)
	if err != nil || !enabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pushRecords(ctx, store, time.Now().UTC()); err != nil {
		fmt.Fprintf(os.Stderr, "conductor: off-host backup on shutdown failed: %v\n", err)
	}
}

// maybeBackupAfterSave pushes records to S3 after a `sessions save`, when backup is configured.
// A failure is reported but never fails the save: the local records are already written, which
// is what makes the machine itself resumable.
func maybeBackupAfterSave(ctx context.Context) {
	store, enabled, err := backup.FromEnv(os.Getenv)
	if err != nil || !enabled {
		return
	}
	if n, err := pushRecords(ctx, store, time.Now().UTC()); err != nil {
		fmt.Fprintf(os.Stderr, "conductor: saved locally, but off-host backup failed: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "Also pushed %d record(s) to %s\n", n, store.Location())
	}
}

// maybeRestoreBeforeResume pulls records from S3 when this machine has none locally and backup
// is configured — the fresh-instance case, where resume would otherwise find nothing.
func maybeRestoreBeforeResume(ctx context.Context) {
	local, err := localstate.List()
	if err != nil || len(local) > 0 {
		return
	}
	store, enabled, err := backup.FromEnv(os.Getenv)
	if err != nil || !enabled {
		return
	}
	n, err := pullRecords(ctx, store, false)
	switch {
	case err == nil && n > 0:
		fmt.Fprintf(os.Stderr, "Restored %d session record(s) from %s.\n", n, store.Location())
	case errors.Is(err, backup.ErrNotFound):
		// A replacement instance usually has a new hostname, so its records sit under the old
		// machine's key. Say so, since the whole point of auto-restore is this moment.
		fmt.Fprintf(os.Stderr, "conductor: no backup found for machine %q. If this host replaced another, "+
			"restore it with CONDUCTOR_MACHINE_ID=<old-id> conductor backup pull\n", store.Machine())
	case err != nil:
		fmt.Fprintf(os.Stderr, "conductor: could not restore from backup: %v\n", err)
	}
}
