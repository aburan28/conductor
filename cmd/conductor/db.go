package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aburan28/conductor/internal/awscreds"
	"github.com/aburan28/conductor/internal/backup"
	"github.com/aburan28/conductor/internal/pgarchive"
	"github.com/aburan28/conductor/internal/storage"
)

// `conductor db` keeps the control-plane database durable in the storage bucket: Postgres
// archives each WAL segment through archive-wal, base backups are streamed on a schedule,
// and restore rebuilds a data directory on another machine. docs/STORAGE.md is the
// contract with the macOS app, which runs these from its launchd agents.

func cmdDB(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return dbStatus(ctx, nil)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "archiving":
		return dbArchiving(rest)
	case "archive-wal":
		return dbArchiveWAL(ctx, rest)
	case "fetch-wal":
		return dbFetchWAL(ctx, rest)
	case "base-backup":
		return dbBaseBackup(ctx, rest)
	case "backups":
		return dbBackups(ctx, rest)
	case "status":
		return dbStatus(ctx, rest)
	case "restore":
		return dbRestore(ctx, rest)
	case "prune":
		return dbPrune(ctx, rest)
	default:
		return fmt.Errorf("unknown db subcommand %q (archiving, archive-wal, fetch-wal, base-backup, backups, status, restore, prune)", sub)
	}
}

// dbStorage is the bucket configuration as the database archive sees it.
type dbStorage struct {
	resolved storage.Resolved
	env      awscreds.Env
	enabled  bool // a bucket is configured and the database use is on
	archive  bool // ... and WAL archiving is on
}

func loadDBStorage() (dbStorage, error) {
	env := awscreds.Default()
	r, err := storage.Resolve(env.Getenv)
	if err != nil {
		return dbStorage{}, err
	}
	d := dbStorage{resolved: r, env: env}
	d.enabled = r.Configured() && r.Uses(storage.UseDatabase)
	d.archive = d.enabled && r.Settings.Database.ArchiveWALOn()
	return d, nil
}

func (d dbStorage) location() string {
	return fmt.Sprintf("s3://%s/%s", d.resolved.Settings.S3.Bucket, d.resolved.Prefix())
}

func (d dbStorage) config() (pgarchive.Config, error) {
	if !d.enabled {
		return pgarchive.Config{}, errors.New("the database is not archived: configure a bucket with `conductor storage set` " +
			"and keep --database on")
	}
	s3cfg, err := d.resolved.S3Config(d.env)
	if err != nil {
		return pgarchive.Config{}, err
	}
	state, err := conductorStateDir()
	if err != nil {
		return pgarchive.Config{}, err
	}
	env := d.env
	return pgarchive.Config{
		S3:         backup.New(s3cfg),
		Prefix:     d.resolved.Prefix(),
		Seal:       d.resolved.Settings.Database.SealOn(),
		Passphrase: func(ctx context.Context) (string, error) { return storage.SealPassphrase(ctx, env) },
		StateDir:   state,
		Location:   d.location(),
	}, nil
}

func conductorStateDir() (string, error) {
	if v := os.Getenv("CONDUCTOR_STATE_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor"), nil
}

// resolveSystemID finds the cluster to act on: --system-id, a data directory's pg_control,
// this machine's archive status files, or the bucket, when exactly one cluster is there.
func resolveSystemID(ctx context.Context, cfg pgarchive.Config, flagID, dataDir string, askBucket bool) (string, error) {
	if flagID != "" {
		return flagID, nil
	}
	if dataDir != "" {
		return pgarchive.SystemIdentifier(dataDir)
	}
	if entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "db-archive")); err == nil {
		var ids []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
			}
		}
		if len(ids) == 1 {
			return ids[0], nil
		}
	}
	if !askBucket {
		return "", errors.New("no cluster identified: pass --data-dir or --system-id")
	}
	ids, err := pgarchive.Clusters(ctx, cfg)
	if err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("%s holds no database archive yet", cfg.Location)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("%s holds several clusters (%s); pass --system-id", cfg.Location, strings.Join(ids, ", "))
}

// archiveCommandPrefix is how Postgres should invoke this binary: by absolute path, with the
// state directory when one is set, since Postgres's environment may not carry it.
func archiveCommandPrefix() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "conductor"
	} else if abs, err := filepath.EvalSymlinks(exe); err == nil {
		exe = abs
	}
	prefix := shellQuote(exe)
	if v := os.Getenv("CONDUCTOR_STATE_DIR"); v != "" {
		prefix = "CONDUCTOR_STATE_DIR=" + shellQuote(v) + " " + prefix
	}
	return prefix
}

// archivingSettings are the postgresql.conf settings that hand WAL to archive-wal. wal_level is
// included only when raiseWALLevel is set (see walLevelIsMinimal): archiving needs replica or
// higher, and a logical setting the cluster already has must not be overridden.
func archivingSettings(d dbStorage, raiseWALLevel bool) [][2]string {
	if !d.archive {
		return [][2]string{{"archive_mode", "off"}}
	}
	settings := [][2]string{
		{"archive_mode", "on"},
		{"archive_command", archiveCommandPrefix() + " db archive-wal %p %f"},
		{"archive_timeout", fmt.Sprintf("%ds", d.resolved.Settings.Database.ArchiveTimeout())},
	}
	if raiseWALLevel {
		settings = append(settings, [2]string{"wal_level", "replica"})
	}
	return settings
}

const autoConfMarker = "# Managed by `conductor db archiving`"

// archiveKeys are the archive settings conductor owns in postgresql.auto.conf wherever they are.
var archiveKeys = map[string]bool{"archive_mode": true, "archive_command": true, "archive_timeout": true}

func dbArchiving(args []string) error {
	fs := flag.NewFlagSet("db archiving", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "the cluster's data directory (required with --write)")
	write := fs.Bool("write", false, "write the settings into the data directory's postgresql.auto.conf")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseStorageFlags(fs, args); err != nil {
		return err
	}
	d, err := loadDBStorage()
	if err != nil {
		return err
	}
	raise := false
	if *dataDir != "" && d.archive {
		if raise, err = walLevelIsMinimal(*dataDir); err != nil {
			return err
		}
	}
	settings := archivingSettings(d, raise)
	if *write {
		if *dataDir == "" {
			return errors.New("--write needs --data-dir")
		}
		if err := writeAutoConf(filepath.Join(*dataDir, "postgresql.auto.conf"), settings); err != nil {
			return err
		}
	}
	if *asJSON {
		m := map[string]string{}
		for _, kv := range settings {
			m[kv[0]] = kv[1]
		}
		return emit(map[string]any{"archiving": d.archive, "settings": m, "written": *write})
	}
	for _, kv := range settings {
		fmt.Printf("%s = %s\n", kv[0], pgarchive.ConfQuote(kv[1]))
	}
	if d.archive && *dataDir == "" {
		fmt.Fprintln(os.Stderr, "wal_level must be replica or higher for archiving; pass --data-dir to check the cluster's value")
	}
	if *write {
		fmt.Fprintf(os.Stderr, "Wrote %s. Restart Postgres if archive_mode changed.\n", filepath.Join(*dataDir, "postgresql.auto.conf"))
	}
	return nil
}

// confLine is one line of a postgresql.conf-style file, classified for conductor's managed block.
type confLine struct {
	text string
	// key is the setting name when the line is "key = value", else "".
	key string
	// owned is set for the settings conductor wrote under its marker. Only the managed settings in
	// the run right after the marker are conductor's: a line added elsewhere, such as one Postgres's
	// ALTER SYSTEM wrote, is not.
	owned bool
}

// scanConfLines splits body into lines and marks conductor's own block.
func scanConfLines(body string) []confLine {
	if body == "" {
		return nil
	}
	var out []confLine
	inBlock := false
	for _, text := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		trimmed := strings.TrimSpace(text)
		line := confLine{text: text}
		if trimmed == autoConfMarker {
			inBlock = true
			out = append(out, line)
			continue
		}
		if k, _, ok := strings.Cut(trimmed, "="); ok {
			line.key = strings.TrimSpace(k)
		}
		line.owned = inBlock && line.key != "" && (archiveKeys[line.key] || line.key == "wal_level")
		inBlock = line.owned
		out = append(out, line)
	}
	return out
}

// confValue is the value of a "key = value" line: a quoted string or a bare word, without any
// trailing comment.
func confValue(text string) string {
	_, v, _ := strings.Cut(text, "=")
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "'") {
		if end := strings.Index(v[1:], "'"); end >= 0 {
			return v[1 : end+1]
		}
		return ""
	}
	if i := strings.IndexAny(v, " \t#"); i >= 0 {
		v = v[:i]
	}
	return v
}

// walLevelIsMinimal reports whether the cluster in dataDir would run with wal_level = minimal, the
// one value below replica, which archiving cannot use. The server reads postgresql.conf and then
// postgresql.auto.conf, and the later assignment wins. Conductor's own lines are left out: the
// question is the cluster's setting, not the one conductor may write. Unset means replica.
func walLevelIsMinimal(dataDir string) (bool, error) {
	level := "replica"
	for _, name := range []string{"postgresql.conf", "postgresql.auto.conf"} {
		body, err := os.ReadFile(filepath.Join(dataDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, l := range scanConfLines(string(body)) {
			if l.key == "wal_level" && !l.owned {
				level = confValue(l.text)
			}
		}
	}
	return strings.EqualFold(level, "minimal"), nil
}

// writeAutoConf sets the archive settings in postgresql.auto.conf, which Postgres itself rewrites
// with ALTER SYSTEM. Archive settings are replaced wherever they are. wal_level is replaced only
// in conductor's own block, so a wal_level set elsewhere is kept, and a stale one that conductor
// wrote is dropped when it is no longer needed. The rest of the file is kept as it is.
func writeAutoConf(path string, settings [][2]string) error {
	return writeAutoConfWith(path, settings, nil)
}

// writeAutoConfWith is writeAutoConf. beforeRename runs once the new contents are complete and
// before they replace the file; a test uses it to interrupt the write.
func writeAutoConfWith(path string, settings [][2]string, beforeRename func() error) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var keep []string
	for _, l := range scanConfLines(string(existing)) {
		if strings.TrimSpace(l.text) == autoConfMarker || archiveKeys[l.key] || (l.owned && l.key == "wal_level") {
			continue
		}
		if l.text != "" || len(keep) > 0 {
			keep = append(keep, l.text)
		}
	}
	keep = append(keep, autoConfMarker)
	for _, kv := range settings {
		keep = append(keep, kv[0]+" = "+pgarchive.ConfQuote(kv[1]))
	}
	return replaceFile(path, []byte(strings.Join(keep, "\n")+"\n"), 0o600, beforeRename)
}

// replaceFile writes data to path through a temporary file in the same directory, synced and then
// renamed over path. An interrupted write therefore leaves the old file whole, never a truncated
// one.
func replaceFile(path string, data []byte, perm os.FileMode, beforeRename func() error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the file has been renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if beforeRename != nil {
		if err := beforeRename(); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // make the rename durable where the platform allows it
		d.Close()
	}
	return nil
}

func dbArchiveWAL(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("db archive-wal", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "the data directory (default: the working directory, as Postgres runs it)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: conductor db archive-wal <path> <name>   (archive_command = '… %p %f')")
	}
	path, name := positional[0], positional[1]
	d, err := loadDBStorage()
	if err != nil {
		return err
	}
	if !d.archive {
		// Succeeding without uploading keeps Postgres from piling up WAL on disk while
		// archiving is turned off; the next base backup re-establishes the archive.
		fmt.Fprintf(os.Stderr, "conductor: database archiving is off; %s was not uploaded\n", name)
		return nil
	}
	cfg, err := d.config()
	if err != nil {
		return err
	}
	dir := *dataDir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	sysID, err := pgarchive.SystemIdentifier(dir)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return pgarchive.New(cfg, sysID).ArchiveWAL(ctx, path, name)
}

// fetchWALAttempts bounds how often fetch-wal tries a failure that is not "not archived".
const fetchWALAttempts = 3

// fetchWALBackoff is the pause before the first retry; each later retry waits one step longer.
const fetchWALBackoff = 500 * time.Millisecond

// fetchWALResult is what restore_command must do with one fetch-wal outcome.
type fetchWALResult int

const (
	// fetchWALDone: the segment was written; exit 0.
	fetchWALDone fetchWALResult = iota
	// fetchWALNotArchived: the bucket has no such segment; exit 1, which Postgres reads as the end
	// of the archive.
	fetchWALNotArchived
	// fetchWALFailed: anything else. It must not exit 1, or Postgres would take it for the end of
	// the archive and promote, and later segments would never be replayed.
	fetchWALFailed
)

// classifyFetchWAL maps the error from a fetch to its result. Only pgarchive.ErrNotArchived, the
// bucket's verdict that a segment does not exist, is the normal end of recovery.
func classifyFetchWAL(err error) fetchWALResult {
	switch {
	case err == nil:
		return fetchWALDone
	case errors.Is(err, pgarchive.ErrNotArchived):
		return fetchWALNotArchived
	default:
		return fetchWALFailed
	}
}

// runFetchWAL runs fetch and retries a fetchWALFailed result, at most attempts times in all,
// waiting backoff, 2×backoff, … between tries. A not-archived result is final on the first try.
// It stops early when ctx is done.
func runFetchWAL(ctx context.Context, fetch func(context.Context) error, attempts int, backoff time.Duration) (fetchWALResult, error) {
	for try := 1; ; try++ {
		err := fetch(ctx)
		res := classifyFetchWAL(err)
		if res != fetchWALFailed || try >= attempts {
			return res, err
		}
		select {
		case <-ctx.Done():
			return res, err
		case <-time.After(backoff * time.Duration(try)):
		}
	}
}

// dbFetchWAL is restore_command. Postgres reads its exit status: 1 means the segment is not
// archived, which ends recovery there and promotes the server. So only that case exits 1.
// Every other failure (a bad command line, missing settings or credentials, an S3 error, a bad
// key) is retried a few times and then ends the process with SIGKILL, which Postgres treats as a
// fatal restore error. Recovery stops and nothing is promoted over the missing segments.
func dbFetchWAL(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("db fetch-wal", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "the data directory (default: the working directory, as Postgres runs it)")
	systemID := fs.String("system-id", "", "the cluster (default: read from the data directory)")
	positional, err := parseFlags(fs, args)
	if err == nil && len(positional) != 2 {
		err = errors.New("usage: conductor db fetch-wal <name> <path>   (restore_command = '… %f %p')")
	}
	if err != nil {
		return failFetchWAL(err)
	}
	name, dest := positional[0], positional[1]
	res, err := runFetchWAL(ctx, func(ctx context.Context) error {
		return fetchWALOnce(ctx, name, dest, *dataDir, *systemID)
	}, fetchWALAttempts, fetchWALBackoff)
	switch res {
	case fetchWALDone:
		return nil
	case fetchWALNotArchived:
		// The normal end of recovery: Postgres asks for the next segment and is told there is
		// none. Exit non-zero without noise in its log.
		os.Exit(1)
	}
	return failFetchWAL(err)
}

// fetchWALOnce makes one attempt: load the settings, identify the cluster, and fetch the segment.
func fetchWALOnce(ctx context.Context, name, dest, dataDir, systemID string) error {
	d, err := loadDBStorage()
	if err != nil {
		return err
	}
	cfg, err := d.config()
	if err != nil {
		return err
	}
	dir := dataDir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	sysID := systemID
	if sysID == "" {
		if sysID, err = pgarchive.SystemIdentifier(dir); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(dir, dest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return pgarchive.New(cfg, sysID).FetchWAL(ctx, name, dest)
}

// failFetchWAL reports a fetch-wal failure on stderr (Postgres logs it) and kills this process
// with SIGKILL, so Postgres sees a fatal restore failure rather than the end of the archive.
func failFetchWAL(err error) error {
	fmt.Fprintf(os.Stderr, "conductor: fetch-wal failed; recovery must stop: %v\n", err)
	if p, perr := os.FindProcess(os.Getpid()); perr == nil {
		_ = p.Signal(syscall.SIGKILL)
	}
	// Only reached when the signal could not be delivered. Any status other than 0 or 1 still
	// stops recovery rather than reading as the end of the archive.
	os.Exit(2)
	return err
}

func dbDSN(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v, nil
	}
	if path, err := dsnPath(); err == nil {
		if body, err := os.ReadFile(path); err == nil {
			if saved := strings.TrimSpace(string(body)); saved != "" {
				return saved, nil
			}
		}
	}
	return "", errors.New("no database to back up: pass --dsn or set DATABASE_URL")
}

func dbBaseBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("db base-backup", flag.ContinueOnError)
	dsnFlag := fs.String("dsn", "", "the database (default: $DATABASE_URL, else the one `conductor up` saved)")
	pgBin := fs.String("pg-bin", os.Getenv("CONDUCTOR_PG_BIN"), "directory holding pg_basebackup and psql")
	noPrune := fs.Bool("no-prune", false, "keep every older base backup and its WAL")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseStorageFlags(fs, args); err != nil {
		return err
	}
	d, err := loadDBStorage()
	if err != nil {
		return err
	}
	cfg, err := d.config()
	if err != nil {
		return err
	}
	dsn, err := dbDSN(*dsnFlag)
	if err != nil {
		return err
	}
	bin, err := pgarchive.FindPGBin(*pgBin)
	if err != nil {
		return err
	}
	info, err := pgarchive.QueryClusterInfo(ctx, bin, dsn)
	if err != nil {
		return err
	}
	a := pgarchive.New(cfg, info.SystemID)
	m, err := a.BaseBackup(ctx, pgarchive.BaseBackupOptions{PGBin: bin, DSN: dsn, Info: info})
	if err != nil {
		return err
	}
	var pruned *pgarchive.PruneResult
	if !*noPrune {
		res, err := a.Prune(ctx, d.resolved.Settings.Database.Keep())
		if err != nil {
			fmt.Fprintf(os.Stderr, "conductor: the backup succeeded, but pruning older ones failed: %v\n", err)
		} else {
			pruned = &res
		}
	}
	if *asJSON {
		return emit(map[string]any{"backup": m, "pruned": pruned, "location": cfg.Location})
	}
	fmt.Printf("Base backup %s of cluster %s: %s (%s stored%s) in %s\n", m.ID, m.SystemID, humanBytes(m.Size),
		humanBytes(m.StoredSize), map[bool]string{true: ", sealed", false: ""}[m.Sealed], cfg.Location)
	if pruned != nil && (len(pruned.RemovedBackups) > 0 || pruned.RemovedWAL > 0) {
		fmt.Printf("Pruned %d older base backup(s) and %d WAL segment(s); keeping %d.\n",
			len(pruned.RemovedBackups), pruned.RemovedWAL, len(pruned.Kept))
	}
	return nil
}

func dbBackups(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("db backups", flag.ContinueOnError)
	systemID := fs.String("system-id", "", "the cluster (default: the only one, or this machine's)")
	dataDir := fs.String("data-dir", "", "read the cluster from this data directory")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseStorageFlags(fs, args); err != nil {
		return err
	}
	d, err := loadDBStorage()
	if err != nil {
		return err
	}
	cfg, err := d.config()
	if err != nil {
		return err
	}
	sysID, err := resolveSystemID(ctx, cfg, *systemID, *dataDir, true)
	if err != nil {
		if *asJSON {
			return emit(map[string]any{"system_id": "", "backups": []pgarchive.Manifest{}, "error": err.Error()})
		}
		return err
	}
	a := pgarchive.New(cfg, sysID)
	backups, err := a.Backups(ctx)
	if err != nil {
		return err
	}
	wal, err := a.WAL(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		if backups == nil {
			backups = []pgarchive.Manifest{}
		}
		return emit(map[string]any{"system_id": sysID, "location": cfg.Location, "backups": backups, "wal": wal})
	}
	fmt.Printf("Cluster %s in %s\n", sysID, cfg.Location)
	if len(backups) == 0 {
		fmt.Println("  no base backups yet (conductor db base-backup)")
	}
	for _, m := range backups {
		fmt.Printf("  %s  %9s  from %s  %s\n", m.ID, humanBytes(m.Size), m.StartWAL,
			map[bool]string{true: "sealed", false: ""}[m.Sealed])
	}
	fmt.Printf("  WAL: %d segment(s), %s", wal.Segments, humanBytes(wal.Bytes))
	if wal.Segments > 0 {
		fmt.Printf(", %s … %s", wal.First, wal.Last)
	}
	fmt.Println()
	return nil
}

// dbStatusView is `conductor db status --json`.
type dbStatusView struct {
	Configured  bool                  `json:"configured"`
	Enabled     bool                  `json:"enabled"`
	Archiving   bool                  `json:"archiving"`
	Location    string                `json:"location,omitempty"`
	Sealed      bool                  `json:"sealed"`
	SystemID    string                `json:"system_id,omitempty"`
	Archive     *pgarchive.Status     `json:"archive,omitempty"`
	LagSeconds  *int64                `json:"lag_seconds,omitempty"`
	Failing     bool                  `json:"failing"`
	BaseBackups *dbStatusBackups      `json:"base_backups,omitempty"`
	WAL         *pgarchive.WALSummary `json:"wal,omitempty"`
	Error       string                `json:"error,omitempty"`
}

type dbStatusBackups struct {
	Count    int    `json:"count"`
	LatestID string `json:"latest_id,omitempty"`
	LatestAt string `json:"latest_at,omitempty"`
}

func dbStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("db status", flag.ContinueOnError)
	systemID := fs.String("system-id", "", "the cluster")
	dataDir := fs.String("data-dir", "", "read the cluster from this data directory")
	local := fs.Bool("local", false, "report only what this machine recorded; do not contact the bucket")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseStorageFlags(fs, args); err != nil {
		return err
	}
	d, err := loadDBStorage()
	if err != nil {
		return err
	}
	v := dbStatusView{Configured: d.resolved.Configured(), Enabled: d.enabled, Archiving: d.archive,
		Sealed: d.resolved.Settings.Database.SealOn()}
	if d.enabled {
		v.Location = d.location()
		cfg, err := d.config()
		if err != nil {
			v.Error = err.Error()
		} else if sysID, err := resolveSystemID(ctx, cfg, *systemID, *dataDir, !*local); err != nil {
			v.Error = err.Error()
		} else {
			v.SystemID = sysID
			a := pgarchive.New(cfg, sysID)
			st := a.LoadStatus()
			v.Archive = &st
			if t, err := time.Parse(time.RFC3339, st.LastAt); err == nil {
				lag := int64(time.Since(t).Seconds())
				v.LagSeconds = &lag
			}
			v.Failing = st.LastError != ""
			if !*local {
				ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				if backups, err := a.Backups(ctx); err != nil {
					v.Error = err.Error()
				} else {
					b := &dbStatusBackups{Count: len(backups)}
					if len(backups) > 0 {
						last := backups[len(backups)-1]
						b.LatestID, b.LatestAt = last.ID, last.FinishedAt
					}
					v.BaseBackups = b
					if wal, err := a.WAL(ctx); err == nil {
						v.WAL = &wal
					}
				}
			}
		}
	}
	if *asJSON {
		return emit(v)
	}
	switch {
	case !v.Configured:
		fmt.Println("Database archive: no bucket configured (conductor storage set).")
		return nil
	case !v.Enabled:
		fmt.Println("Database archive: off for this bucket (conductor storage set --database=true).")
		return nil
	}
	fmt.Printf("Database archive in %s", v.Location)
	if v.SystemID != "" {
		fmt.Printf(" (cluster %s)", v.SystemID)
	}
	fmt.Println()
	if !v.Archiving {
		fmt.Println("  WAL archiving is off; only base backups are taken.")
	}
	if a := v.Archive; a != nil {
		if a.LastWAL != "" {
			fmt.Printf("  last archived   %s at %s (%d this machine)\n", a.LastWAL, a.LastAt, a.Archived)
		} else {
			fmt.Println("  last archived   nothing yet from this machine")
		}
		if a.LastError != "" {
			fmt.Printf("  FAILING         %s at %s: %s\n", a.LastErrorWAL, a.LastErrorAt, a.LastError)
		}
	}
	if b := v.BaseBackups; b != nil {
		if b.Count == 0 {
			fmt.Println("  base backups    none yet (conductor db base-backup)")
		} else {
			fmt.Printf("  base backups    %d, newest %s\n", b.Count, b.LatestID)
		}
	}
	if v.Error != "" {
		fmt.Printf("  error           %s\n", v.Error)
	}
	return nil
}

func dbRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("db restore", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "the new data directory (must not exist, or be empty)")
	backupID := fs.String("backup", "latest", "base backup ID, or latest")
	target := fs.String("target-time", "", "stop recovery at this moment (RFC3339) instead of the end of the archive")
	systemID := fs.String("system-id", "", "the cluster (default: the only one in the bucket)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseStorageFlags(fs, args); err != nil {
		return err
	}
	if *dataDir == "" {
		return errors.New("--data-dir is required")
	}
	d, err := loadDBStorage()
	if err != nil {
		return err
	}
	cfg, err := d.config()
	if err != nil {
		return err
	}
	sysID, err := resolveSystemID(ctx, cfg, *systemID, "", true)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	a := pgarchive.New(cfg, sysID)
	m, err := a.Restore(ctx, pgarchive.RestoreOptions{
		DataDir: abs, Backup: *backupID, TargetTime: *target,
		RestoreCommand: archiveCommandPrefix() + " db fetch-wal %f %p",
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(map[string]any{"restored": m, "data_dir": abs})
	}
	fmt.Printf("Restored base backup %s of cluster %s into %s.\n", m.ID, sysID, abs)
	fmt.Println("Start Postgres on it: it replays the archived WAL")
	if *target != "" {
		fmt.Printf("up to %s, then opens for writes.\n", *target)
	} else {
		fmt.Println("to the last archived segment, then opens for writes.")
	}
	fmt.Println("Then point archiving at it again: conductor db archiving --data-dir " + shellQuote(abs) + " --write")
	return nil
}

func dbPrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("db prune", flag.ContinueOnError)
	keep := fs.Int("keep", 0, "base backups to keep (default: the storage setting)")
	systemID := fs.String("system-id", "", "the cluster")
	dataDir := fs.String("data-dir", "", "read the cluster from this data directory")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseStorageFlags(fs, args); err != nil {
		return err
	}
	d, err := loadDBStorage()
	if err != nil {
		return err
	}
	cfg, err := d.config()
	if err != nil {
		return err
	}
	sysID, err := resolveSystemID(ctx, cfg, *systemID, *dataDir, true)
	if err != nil {
		return err
	}
	n := *keep
	if n == 0 {
		n = d.resolved.Settings.Database.Keep()
	}
	res, err := pgarchive.New(cfg, sysID).Prune(ctx, n)
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(res)
	}
	fmt.Printf("Kept %d base backup(s); removed %d and %d WAL segment(s) only they needed.\n",
		len(res.Kept), len(res.RemovedBackups), res.RemovedWAL)
	return nil
}
