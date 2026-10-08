//go:build unix

package pgarchive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakePGBin is a directory holding a pg_basebackup stand-in. It streams a small tar to stdout and
// reports the WAL range on stderr, as the real tool does, and fails when FAIL_BASEBACKUP is set.
func fakePGBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
if [ -n "$FAIL_BASEBACKUP" ]; then echo "pg_basebackup: could not connect" >&2; exit 1; fi
printf 'tar-of-the-cluster'
echo "pg_basebackup: write-ahead log start point: 0/2000028 on timeline 1" >&2
echo "pg_basebackup: write-ahead log end point: 0/2000100" >&2
`
	if err := os.WriteFile(filepath.Join(dir, "pg_basebackup"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Base backups have one-second IDs. Two runs in the same second used to share a base.tar and
// manifest name, so the second overwrote the first, and a failed second run deleted the first's
// archive.
func TestBaseBackupsStartedInTheSameSecondDoNotCollide(t *testing.T) {
	a, _, _ := fakeArchiver(t, false)
	at := time.Date(2026, 10, 8, 2, 45, 22, 0, time.UTC)
	a.cfg.Now = func() time.Time { return at }
	opts := BaseBackupOptions{PGBin: fakePGBin(t), Info: ClusterInfo{SystemID: "7001", Version: "16.14", WALSegmentSize: 16 << 20}}
	ctx := context.Background()

	first, err := a.BaseBackup(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.BaseBackup(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("two runs in the same second got the same backup ID %s", first.ID)
	}
	backups, err := a.Backups(ctx)
	if err != nil || len(backups) != 2 {
		t.Fatalf("catalog has %d backups (%v); want both runs", len(backups), err)
	}
}

func TestAFailedBaseBackupLeavesAnotherRunsBackupIntact(t *testing.T) {
	a, fake, _ := fakeArchiver(t, false)
	at := time.Date(2026, 10, 8, 2, 45, 22, 0, time.UTC)
	a.cfg.Now = func() time.Time { return at }
	opts := BaseBackupOptions{PGBin: fakePGBin(t), Info: ClusterInfo{SystemID: "7001", Version: "16.14", WALSegmentSize: 16 << 20}}
	ctx := context.Background()

	good, err := a.BaseBackup(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAIL_BASEBACKUP", "1")
	if _, err := a.BaseBackup(ctx, opts); err == nil {
		t.Fatal("a run whose pg_basebackup failed reported success")
	}
	if _, ok := fake.Object(a.baseTarKey(good.ID, false)); !ok {
		t.Fatal("the failed run deleted the archive of the backup it shared a second with")
	}
	if _, ok := fake.Object(a.manifestKey(good.ID)); !ok {
		t.Fatal("the failed run deleted the manifest of another backup")
	}
	backups, err := a.Backups(ctx)
	if err != nil || len(backups) != 1 || backups[0].ID != good.ID {
		t.Fatalf("catalog after the failed run: %v (%v); want only %s", backups, err, good.ID)
	}
}
