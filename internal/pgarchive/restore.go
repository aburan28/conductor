package pgarchive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/backup"
)

// RestoreOptions configures a restore.
type RestoreOptions struct {
	// DataDir is where the cluster is rebuilt. It must not exist or be empty.
	DataDir string
	// Backup is a base backup ID, or "" / "latest" for the newest.
	Backup string
	// TargetTime, when set (RFC3339), stops recovery at that moment instead of at the end of
	// the archive.
	TargetTime string
	// RestoreCommand is the restore_command Postgres runs for each archived WAL file; it
	// must contain %f and %p ("conductor db fetch-wal %f %p").
	RestoreCommand string
}

// Restore rebuilds a data directory from a base backup and configures recovery to replay
// the archived WAL. Postgres does the replay when it is next started: it fetches each
// segment through RestoreCommand, stops at the end of the archive (or at TargetTime),
// and promotes itself to a normal server on a new timeline.
func (a *Archiver) Restore(ctx context.Context, opts RestoreOptions) (Manifest, error) {
	if !strings.Contains(opts.RestoreCommand, "%f") || !strings.Contains(opts.RestoreCommand, "%p") {
		return Manifest{}, errors.New("the restore command must contain %f and %p")
	}
	if opts.TargetTime != "" {
		if _, err := time.Parse(time.RFC3339, opts.TargetTime); err != nil {
			return Manifest{}, fmt.Errorf("--target-time %q is not RFC3339 (2026-10-07T12:00:00Z)", opts.TargetTime)
		}
	}
	if err := emptyDir(opts.DataDir); err != nil {
		return Manifest{}, err
	}
	backups, err := a.Backups(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if len(backups) == 0 {
		return Manifest{}, fmt.Errorf("cluster %s has no base backup in the bucket", a.systemID)
	}
	m := backups[len(backups)-1]
	if opts.Backup != "" && opts.Backup != "latest" {
		found := false
		for _, b := range backups {
			if b.ID == opts.Backup {
				m, found = b, true
			}
		}
		if !found {
			return Manifest{}, fmt.Errorf("no base backup %q (have %s)", opts.Backup, idsOf(backups))
		}
	}
	if opts.TargetTime != "" {
		t, _ := time.Parse(time.RFC3339, opts.TargetTime)
		if fin, err := time.Parse(time.RFC3339, m.FinishedAt); err == nil && t.Before(fin) {
			return Manifest{}, fmt.Errorf("--target-time %s is before backup %s finished (%s); choose an older backup",
				opts.TargetTime, m.ID, m.FinishedAt)
		}
	}

	// The key first: a wrong passphrase should fail before a multi-gigabyte download starts.
	var key *DataKey
	if m.Sealed {
		if key, err = a.dataKey(ctx, false); err != nil {
			return Manifest{}, err
		}
	}
	body, err := a.s3.GetReader(ctx, a.baseTarKey(m.ID, m.Sealed))
	if errors.Is(err, backup.ErrNotFound) {
		return Manifest{}, fmt.Errorf("backup %s's archive is missing from the bucket", m.ID)
	}
	if err != nil {
		return Manifest{}, err
	}
	defer body.Close()
	var r io.Reader = body
	if key != nil {
		if r, err = Open(body, key); err != nil {
			return Manifest{}, err
		}
	}
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return Manifest{}, err
	}
	if err := os.Chmod(opts.DataDir, 0o700); err != nil {
		return Manifest{}, err
	}
	if err := extractTar(r, opts.DataDir); err != nil {
		return Manifest{}, fmt.Errorf("unpacking backup %s: %w", m.ID, err)
	}
	if got, err := SystemIdentifier(opts.DataDir); err != nil || got != a.systemID {
		return Manifest{}, fmt.Errorf("the restored data directory is not cluster %s (%s %v)", a.systemID, got, err)
	}

	conf := fmt.Sprintf("\n# Added by `conductor db restore` from base backup %s (%s).\n", m.ID, time.Now().UTC().Format(time.RFC3339))
	conf += "restore_command = " + pgQuote(opts.RestoreCommand) + "\n"
	if opts.TargetTime != "" {
		// Postgres's recovery_target_time rejects ISO 8601's "T…Z" form; give it its own.
		t, _ := time.Parse(time.RFC3339, opts.TargetTime)
		conf += "recovery_target_time = " + pgQuote(t.UTC().Format("2006-01-02 15:04:05.999999")+"+00") + "\n"
		conf += "recovery_target_action = 'promote'\n"
	}
	f, err := os.OpenFile(filepath.Join(opts.DataDir, "postgresql.auto.conf"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := f.WriteString(conf); err != nil {
		f.Close()
		return Manifest{}, err
	}
	if err := f.Close(); err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(opts.DataDir, "recovery.signal"), nil, 0o600); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func idsOf(ms []Manifest) string {
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, ", ")
}

// pgQuote quotes a value for postgresql.conf.
func pgQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func emptyDir(dir string) error {
	if dir == "" {
		return errors.New("a data directory is required")
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty; restore into a new directory", dir)
	}
	return nil
}

// extractTar unpacks a pg_basebackup tar into dir. It accepts directories and regular files
// only: paths that would land outside dir, links, and devices are refused, since the archive
// came from a bucket other principals may be able to write.
func extractTar(r io.Reader, dir string) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	tr := tar.NewReader(r)
	files := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing entry %q: outside the data directory", hdr.Name)
		}
		target := filepath.Join(root, name)
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o640
			if mode&0o600 != 0o600 {
				mode |= 0o600
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			files++
		default:
			return fmt.Errorf("refusing entry %q: type %c is not a file or directory (tablespaces are not supported)",
				hdr.Name, hdr.Typeflag)
		}
	}
	if files == 0 {
		return errors.New("the backup archive is empty")
	}
	return nil
}
