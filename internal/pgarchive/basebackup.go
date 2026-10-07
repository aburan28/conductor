package pgarchive

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Manifest describes one base backup. It is written after the backup itself, so a backup
// counts only once its manifest exists.
type Manifest struct {
	Version        int    `json:"version"`
	ID             string `json:"id"`
	SystemID       string `json:"system_id"`
	PGVersion      string `json:"pg_version"`
	StartedAt      string `json:"started_at"`
	FinishedAt     string `json:"finished_at"`
	StartLSN       string `json:"start_lsn"`
	EndLSN         string `json:"end_lsn"`
	Timeline       uint32 `json:"timeline"`
	StartWAL       string `json:"start_wal"`
	WALSegmentSize uint64 `json:"wal_segment_size"`
	Size           int64  `json:"size"`
	StoredSize     int64  `json:"stored_size"`
	Sealed         bool   `json:"sealed"`
	KeyID          string `json:"key_id,omitempty"`
}

// ClusterInfo is what base-backup learns from the server before it starts.
type ClusterInfo struct {
	SystemID       string
	Version        string
	WALSegmentSize uint64
}

// FindPGBin locates a directory holding pg_basebackup and psql: dir if given, else PATH,
// else the usual install locations (Debian/Ubuntu, Homebrew, Postgres.app, EDB).
func FindPGBin(dir string) (string, error) {
	has := func(d string) bool {
		for _, tool := range []string{"pg_basebackup", "psql"} {
			if _, err := os.Stat(filepath.Join(d, tool)); err != nil {
				return false
			}
		}
		return true
	}
	if dir != "" {
		if has(dir) {
			return dir, nil
		}
		return "", fmt.Errorf("%s does not contain pg_basebackup and psql", dir)
	}
	if p, err := exec.LookPath("pg_basebackup"); err == nil && has(filepath.Dir(p)) {
		return filepath.Dir(p), nil
	}
	var candidates []string
	for _, pattern := range []string{"/usr/lib/postgresql/*/bin", "/opt/homebrew/opt/postgresql@*/bin",
		"/usr/local/opt/postgresql@*/bin", "/Applications/Postgres.app/Contents/Versions/*/bin", "/Library/PostgreSQL/*/bin"} {
		m, _ := filepath.Glob(pattern)
		candidates = append(candidates, m...)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(candidates))) // newest major version first
	for _, d := range candidates {
		if has(d) {
			return d, nil
		}
	}
	return "", errors.New("pg_basebackup and psql were not found; pass --pg-bin or set CONDUCTOR_PG_BIN")
}

// QueryClusterInfo asks the server (through psql) for its system identifier, version, and
// WAL segment size.
func QueryClusterInfo(ctx context.Context, pgBin, dsn string) (ClusterInfo, error) {
	const q = `select (select system_identifier from pg_control_system()), current_setting('server_version'), ` +
		`(select setting from pg_settings where name = 'wal_segment_size')`
	out, err := exec.CommandContext(ctx, filepath.Join(pgBin, "psql"), "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1",
		"-d", dsn, "-c", q).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ClusterInfo{}, fmt.Errorf("psql: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return ClusterInfo{}, err
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(fields) != 3 {
		return ClusterInfo{}, fmt.Errorf("unexpected psql output %q", out)
	}
	seg, err := strconv.ParseUint(fields[2], 10, 64)
	if err != nil || seg == 0 {
		return ClusterInfo{}, fmt.Errorf("unexpected wal_segment_size %q", fields[2])
	}
	return ClusterInfo{SystemID: fields[0], Version: fields[1], WALSegmentSize: seg}, nil
}

// BaseBackupOptions configures one base backup.
type BaseBackupOptions struct {
	PGBin string
	DSN   string
	Info  ClusterInfo
	// PartSize is the multipart part size; 0 means 64 MiB.
	PartSize int
}

var (
	startRE = regexp.MustCompile(`write-ahead log start point: ([0-9A-F]+/[0-9A-F]+) on timeline (\d+)`)
	endRE   = regexp.MustCompile(`write-ahead log end point: ([0-9A-F]+/[0-9A-F]+)`)
)

// BaseBackup streams pg_basebackup (tar format, with the WAL it needs) to the bucket,
// sealing it on the way when sealing is on, then writes the manifest.
func (a *Archiver) BaseBackup(ctx context.Context, opts BaseBackupOptions) (m Manifest, err error) {
	defer func() { a.recordBaseBackup(m, err) }()
	if opts.Info.SystemID != a.systemID {
		return m, fmt.Errorf("the server's system identifier %s is not this archive's %s", opts.Info.SystemID, a.systemID)
	}
	partSize := opts.PartSize
	if partSize == 0 {
		partSize = 64 << 20
	}
	started := a.cfg.Now().UTC()
	m = Manifest{Version: 1, ID: started.Format("20060102T150405Z"), SystemID: a.systemID, PGVersion: opts.Info.Version,
		StartedAt: started.Format(time.RFC3339), WALSegmentSize: opts.Info.WALSegmentSize, Sealed: a.cfg.Seal}

	var key *DataKey
	if a.cfg.Seal {
		if key, err = a.dataKey(ctx, true); err != nil {
			return m, err
		}
		m.KeyID = key.ID()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(opts.PGBin, "pg_basebackup"), "-d", opts.DSN,
		"-D", "-", "-F", "t", "-X", "fetch", "--checkpoint=fast", "-v")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return m, err
	}
	if err := cmd.Start(); err != nil {
		return m, fmt.Errorf("pg_basebackup: %w", err)
	}

	plain := &countingReader{r: bufio.NewReaderSize(stdout, 1<<20)}
	var body io.Reader = plain
	if key != nil {
		if body, err = Seal(plain, key); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return m, err
		}
	}
	objectKey := a.baseTarKey(m.ID, a.cfg.Seal)
	stored, putErr := a.s3.PutStream(ctx, objectKey, body, partSize, "application/x-tar")
	if putErr != nil {
		cancel() // stop pg_basebackup; nothing will read its output
	}
	waitErr := cmd.Wait()
	switch {
	case putErr != nil:
		return m, fmt.Errorf("uploading the base backup: %w", putErr)
	case waitErr != nil:
		// The upload saw the stream end early and stored a short object; it has no
		// manifest, so it is not a backup, but remove it.
		_ = a.s3.Delete(context.WithoutCancel(ctx), objectKey)
		return m, fmt.Errorf("pg_basebackup: %v: %s", waitErr, lastLines(stderr.String(), 3))
	}

	log := stderr.String()
	sm := startRE.FindStringSubmatch(log)
	em := endRE.FindStringSubmatch(log)
	if sm == nil || em == nil {
		_ = a.s3.Delete(context.WithoutCancel(ctx), objectKey)
		return m, fmt.Errorf("pg_basebackup did not report its WAL range: %s", lastLines(log, 3))
	}
	tli, _ := strconv.ParseUint(sm[2], 10, 32)
	m.StartLSN, m.EndLSN, m.Timeline = sm[1], em[1], uint32(tli)
	if m.StartWAL, err = SegmentForLSN(m.Timeline, m.StartLSN, m.WALSegmentSize); err != nil {
		return m, err
	}
	m.Size, m.StoredSize = plain.n.Load(), stored
	m.FinishedAt = a.cfg.Now().UTC().Format(time.RFC3339)
	manifest, _ := json.MarshalIndent(m, "", "  ")
	if err := a.s3.Put(ctx, a.manifestKey(m.ID), manifest, "application/json"); err != nil {
		return m, fmt.Errorf("writing the manifest: %w", err)
	}
	return m, nil
}

func (a *Archiver) baseTarKey(id string, sealed bool) string {
	if sealed {
		return a.baseDir() + id + "/base.tar.sealed"
	}
	return a.baseDir() + id + "/base.tar"
}

func (a *Archiver) manifestKey(id string) string { return a.baseDir() + id + "/manifest.json" }

func (a *Archiver) recordBaseBackup(m Manifest, err error) {
	if a.cfg.StateDir == "" {
		return
	}
	s := a.LoadStatus()
	if err != nil {
		s.LastBaseBackupFail = oneLine(err.Error())
	} else {
		s.LastBaseBackup, s.LastBaseBackupAt, s.LastBaseBackupFail = m.ID, m.FinishedAt, ""
	}
	a.saveStatus(s)
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// ---------------------------------------------------------------------------
// Catalog
// ---------------------------------------------------------------------------

// Backups lists the base backups with a manifest, oldest first.
func (a *Archiver) Backups(ctx context.Context) ([]Manifest, error) {
	objs, err := a.s3.ListAll(ctx, a.baseDir())
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, "/manifest.json") {
			continue
		}
		body, err := a.s3.Get(ctx, o.Key)
		if err != nil {
			return nil, err
		}
		var m Manifest
		if err := json.Unmarshal(body, &m); err != nil {
			continue // a damaged manifest is not a usable backup
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// WALSummary describes the archived WAL.
type WALSummary struct {
	Segments int    `json:"segments"`
	Bytes    int64  `json:"bytes"`
	First    string `json:"first,omitempty"`
	Last     string `json:"last,omitempty"`
}

// WAL summarises the archived WAL segments.
func (a *Archiver) WAL(ctx context.Context) (WALSummary, error) {
	objs, err := a.s3.ListAll(ctx, a.walDir())
	if err != nil {
		return WALSummary{}, err
	}
	var s WALSummary
	for _, o := range objs {
		name := strings.TrimSuffix(strings.TrimPrefix(o.Key, a.walDir()), ".sealed")
		if !isSegmentName(name) {
			continue
		}
		s.Segments++
		s.Bytes += o.Size
		if s.First == "" || name < s.First {
			s.First = name
		}
		if name > s.Last {
			s.Last = name
		}
	}
	return s, nil
}

// PruneResult reports what Prune removed.
type PruneResult struct {
	RemovedBackups []string `json:"removed_backups"`
	RemovedWAL     int      `json:"removed_wal"`
	Kept           []string `json:"kept"`
}

// Prune keeps the newest keep base backups and deletes older ones, then deletes WAL that
// only the deleted backups needed: segments before the oldest kept backup's starting
// segment (by log position, as pg_archivecleanup decides). Timeline history files are kept.
// With no base backup at all, nothing is deleted.
func (a *Archiver) Prune(ctx context.Context, keep int) (PruneResult, error) {
	if keep < 1 {
		return PruneResult{}, errors.New("keep at least one base backup")
	}
	backups, err := a.Backups(ctx)
	if err != nil {
		return PruneResult{}, err
	}
	var res PruneResult
	if len(backups) == 0 {
		return res, nil
	}
	cut := 0
	if len(backups) > keep {
		cut = len(backups) - keep
	}
	for _, m := range backups[cut:] {
		res.Kept = append(res.Kept, m.ID)
	}
	for _, m := range backups[:cut] {
		// Manifest first, so a half-removed backup never looks complete.
		if err := a.s3.Delete(ctx, a.manifestKey(m.ID)); err != nil {
			return res, err
		}
		for _, sealed := range []bool{true, false} {
			if err := a.s3.Delete(ctx, a.baseTarKey(m.ID, sealed)); err != nil {
				return res, err
			}
		}
		res.RemovedBackups = append(res.RemovedBackups, m.ID)
	}
	oldest, ok := segmentPosition(backups[cut].StartWAL)
	if !ok {
		return res, nil
	}
	objs, err := a.s3.ListAll(ctx, a.walDir())
	if err != nil {
		return res, err
	}
	for _, o := range objs {
		name := strings.TrimSuffix(strings.TrimPrefix(o.Key, a.walDir()), ".sealed")
		if strings.HasSuffix(name, ".history") {
			continue
		}
		pos, ok := segmentPosition(name)
		if !ok || pos >= oldest {
			continue
		}
		if err := a.s3.Delete(ctx, o.Key); err != nil {
			return res, err
		}
		res.RemovedWAL++
	}
	return res, nil
}

// Clusters lists the system identifiers that have an archive under prefix.
func Clusters(ctx context.Context, cfg Config) ([]string, error) {
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = "conductor"
	}
	objs, err := cfg.S3.ListAll(ctx, prefix+"/db/")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, o := range objs {
		rest := strings.TrimPrefix(o.Key, prefix+"/db/")
		id, _, ok := strings.Cut(rest, "/")
		if ok && id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}
