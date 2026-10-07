// Package pgarchive makes a local Postgres durable in an S3 bucket: every WAL segment is
// archived as Postgres finishes it (archive_command), base backups are streamed on a
// schedule (pg_basebackup), and a lost machine is rebuilt on another from the newest base
// backup plus the archived WAL, to the last segment or to a chosen moment.
//
// Objects are namespaced by the cluster's system identifier:
//
//	<prefix>/db/<system id>/key.json                     the sealed data key (when sealing)
//	<prefix>/db/<system id>/wal/<segment>[.sealed]
//	<prefix>/db/<system id>/base/<id>/base.tar[.sealed]
//	<prefix>/db/<system id>/base/<id>/manifest.json      written last: a backup exists once this does
//
// docs/STORAGE.md is the contract with the macOS app.
package pgarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/backup"
)

// Config is what an Archiver needs.
type Config struct {
	S3     *backup.S3
	Prefix string
	// Seal encrypts objects before upload. Objects already in the bucket are read either way.
	Seal       bool
	Passphrase func(ctx context.Context) string
	// StateDir holds the key cache and the archive status (the CLI's state directory).
	StateDir string
	// Location describes the bucket for messages ("s3://bucket/prefix").
	Location string
	Now      func() time.Time
}

// Archiver archives one cluster.
type Archiver struct {
	cfg      Config
	s3       *backup.S3
	systemID string
	key      *DataKey
}

// New returns an Archiver for the cluster with this system identifier.
func New(cfg Config, systemID string) *Archiver {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "conductor"
	}
	return &Archiver{cfg: cfg, s3: cfg.S3, systemID: systemID}
}

// SystemID is the cluster this Archiver serves.
func (a *Archiver) SystemID() string { return a.systemID }

func (a *Archiver) base() string    { return a.cfg.Prefix + "/db/" + a.systemID }
func (a *Archiver) keyKey() string  { return a.base() + "/key.json" }
func (a *Archiver) walDir() string  { return a.base() + "/wal/" }
func (a *Archiver) baseDir() string { return a.base() + "/base/" }

func (a *Archiver) walKey(name string, sealed bool) string {
	if sealed {
		return a.walDir() + name + ".sealed"
	}
	return a.walDir() + name
}

// encode returns body as stored: sealed under the data key, or as is.
func (a *Archiver) encode(ctx context.Context, body []byte) ([]byte, error) {
	if !a.cfg.Seal {
		return body, nil
	}
	key, err := a.dataKey(ctx, true)
	if err != nil {
		return nil, err
	}
	r, err := Seal(bytes.NewReader(body), key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// decode opens a stored object, sealed or not.
func (a *Archiver) decode(ctx context.Context, stored []byte, sealed bool) ([]byte, error) {
	if !sealed {
		return stored, nil
	}
	key, err := a.dataKey(ctx, false)
	if err != nil {
		return nil, err
	}
	r, err := Open(bytes.NewReader(stored), key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// variants lists the two key forms of an object name, the configured one first.
func (a *Archiver) variants() []bool {
	if a.cfg.Seal {
		return []bool{true, false}
	}
	return []bool{false, true}
}

// ArchiveWAL is archive_command: copy the file at path (as Postgres names it, relative to
// the data directory) to the bucket under name. It is idempotent. A segment already in the
// bucket with identical content is success, which is what lets Postgres retry safely after
// a crash between the upload and its own bookkeeping. Different content under the same
// name is an error. Postgres keeps the segment and retries, and neither copy is lost.
func (a *Archiver) ArchiveWAL(ctx context.Context, path, name string) (err error) {
	defer func() { a.recordArchive(name, err) }()
	if !validArchiveName(name) {
		return fmt.Errorf("refusing to archive %q: not a WAL file name", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, sealed := range a.variants() {
		same, found, err := a.sameAs(ctx, a.walKey(name, sealed), sealed, data)
		if err != nil {
			return err
		}
		if found {
			if same {
				return nil
			}
			return fmt.Errorf("%s is already archived with different content; not overwriting it", name)
		}
	}
	body, err := a.encode(ctx, data)
	if err != nil {
		return err
	}
	key := a.walKey(name, a.cfg.Seal)
	switch err := a.s3.PutIfAbsent(ctx, key, body, "application/octet-stream"); {
	case errors.Is(err, backup.ErrExists):
		same, _, cerr := a.sameAs(ctx, key, a.cfg.Seal, data)
		if cerr != nil {
			return cerr
		}
		if !same {
			return fmt.Errorf("%s appeared in the bucket with different content while archiving", name)
		}
		return nil
	case err != nil:
		return err
	}
	return nil
}

// sameAs compares the object at key with data. found is false when there is no object.
func (a *Archiver) sameAs(ctx context.Context, key string, sealed bool, data []byte) (same, found bool, err error) {
	stored, err := a.s3.Get(ctx, key)
	if errors.Is(err, backup.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	plain, err := a.decode(ctx, stored, sealed)
	if err != nil {
		return false, true, fmt.Errorf("reading back %s: %w", key, err)
	}
	return bytes.Equal(plain, data), true, nil
}

// ErrNotArchived is returned by FetchWAL when the bucket has no such file, which is how
// recovery learns it has reached the end of the archive.
var ErrNotArchived = errors.New("not in the archive")

// FetchWAL is restore_command: write the archived file name to dest.
func (a *Archiver) FetchWAL(ctx context.Context, name, dest string) error {
	if !validArchiveName(name) {
		return fmt.Errorf("refusing to fetch %q: not a WAL file name", name)
	}
	for _, sealed := range a.variants() {
		stored, err := a.s3.Get(ctx, a.walKey(name, sealed))
		if errors.Is(err, backup.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		plain, err := a.decode(ctx, stored, sealed)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return writeFileAtomic(dest, plain, 0o600)
	}
	return ErrNotArchived
}

func writeFileAtomic(dest string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
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
	return os.Rename(tmp.Name(), dest)
}

// ---------------------------------------------------------------------------
// Local status
// ---------------------------------------------------------------------------

// Status is what this machine knows about its archiving, kept in
// <state>/db-archive/<system id>.json and updated by every archive and base backup.
type Status struct {
	SystemID           string `json:"system_id"`
	Archived           int64  `json:"archived"`
	LastWAL            string `json:"last_wal,omitempty"`
	LastAt             string `json:"last_at,omitempty"`
	LastError          string `json:"last_error,omitempty"`
	LastErrorWAL       string `json:"last_error_wal,omitempty"`
	LastErrorAt        string `json:"last_error_at,omitempty"`
	LastBaseBackup     string `json:"last_base_backup,omitempty"`
	LastBaseBackupAt   string `json:"last_base_backup_at,omitempty"`
	LastBaseBackupFail string `json:"last_base_backup_error,omitempty"`
}

func (a *Archiver) statusPath() string {
	return filepath.Join(a.cfg.StateDir, "db-archive", a.systemID+".json")
}

// LoadStatus reads this machine's archive status for the cluster.
func (a *Archiver) LoadStatus() Status {
	s := Status{SystemID: a.systemID}
	if b, err := os.ReadFile(a.statusPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	s.SystemID = a.systemID
	return s
}

func (a *Archiver) saveStatus(s Status) {
	path := a.statusPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	_ = writeFileAtomic(path, append(b, '\n'), 0o600)
}

func (a *Archiver) recordArchive(name string, err error) {
	if a.cfg.StateDir == "" {
		return
	}
	s := a.LoadStatus()
	now := a.cfg.Now().UTC().Format(time.RFC3339)
	if err != nil {
		s.LastError, s.LastErrorWAL, s.LastErrorAt = oneLine(err.Error()), name, now
	} else {
		s.Archived++
		s.LastWAL, s.LastAt = name, now
		if s.LastErrorWAL == name {
			s.LastError, s.LastErrorWAL, s.LastErrorAt = "", "", ""
		}
	}
	a.saveStatus(s)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}
