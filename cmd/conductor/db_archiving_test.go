package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readConfFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// archiveOn is a dbStorage with archiving on; the settings do not depend on anything else here.
var archiveOn = dbStorage{archive: true}

// A logical wal_level in postgresql.conf must not be overridden: auto.conf is read after it.
func TestArchivingLeavesALogicalWALLevelAlone(t *testing.T) {
	dir := t.TempDir()
	writeConfFile(t, filepath.Join(dir, "postgresql.conf"), "wal_level = logical\n")
	raise, err := walLevelIsMinimal(dir)
	if err != nil || raise {
		t.Fatalf("logical: raise=%v err=%v; want no raise", raise, err)
	}
	auto := filepath.Join(dir, "postgresql.auto.conf")
	if err := writeAutoConf(auto, archivingSettings(archiveOn, raise)); err != nil {
		t.Fatal(err)
	}
	body := readConfFile(t, auto)
	if strings.Contains(body, "wal_level") {
		t.Fatalf("archiving wrote a wal_level that overrides the cluster's logical setting:\n%s", body)
	}
	if !strings.Contains(body, "archive_mode = 'on'") {
		t.Fatalf("archive_mode missing:\n%s", body)
	}
}

// minimal cannot archive, so it is raised, and only in conductor's block.
func TestArchivingRaisesMinimalWALLevelToReplica(t *testing.T) {
	dir := t.TempDir()
	writeConfFile(t, filepath.Join(dir, "postgresql.conf"), "wal_level = 'minimal'  # set by hand\n")
	raise, err := walLevelIsMinimal(dir)
	if err != nil || !raise {
		t.Fatalf("minimal: raise=%v err=%v; want a raise", raise, err)
	}
	auto := filepath.Join(dir, "postgresql.auto.conf")
	if err := writeAutoConf(auto, archivingSettings(archiveOn, raise)); err != nil {
		t.Fatal(err)
	}
	if body := readConfFile(t, auto); !strings.Contains(body, "wal_level = 'replica'") {
		t.Fatalf("minimal was not raised to replica:\n%s", body)
	}
}

// An earlier conductor wrote replica under its marker, which overrode a logical setting. Now that
// it is not needed, that line is removed so the cluster's logical setting applies again.
func TestArchivingDropsItsOwnStaleWALLevel(t *testing.T) {
	dir := t.TempDir()
	writeConfFile(t, filepath.Join(dir, "postgresql.conf"), "wal_level = logical\n")
	auto := filepath.Join(dir, "postgresql.auto.conf")
	writeConfFile(t, auto, autoConfMarker+"\narchive_mode = 'on'\nwal_level = 'replica'\n")
	raise, err := walLevelIsMinimal(dir)
	if err != nil || raise {
		t.Fatalf("raise=%v err=%v; the stale replica must not count as the cluster's setting", raise, err)
	}
	if err := writeAutoConf(auto, archivingSettings(archiveOn, raise)); err != nil {
		t.Fatal(err)
	}
	if body := readConfFile(t, auto); strings.Contains(body, "wal_level") {
		t.Fatalf("the stale wal_level survived:\n%s", body)
	}
	if raise, err := walLevelIsMinimal(dir); err != nil || raise {
		t.Fatalf("after the write: raise=%v err=%v; the cluster's logical setting must apply", raise, err)
	}
}

// A wal_level a person set with ALTER SYSTEM, outside conductor's block, is theirs to keep.
func TestArchivingKeepsAWALLevelSetOutsideItsBlock(t *testing.T) {
	dir := t.TempDir()
	auto := filepath.Join(dir, "postgresql.auto.conf")
	writeConfFile(t, auto, "wal_level = 'logical'\n")
	raise, err := walLevelIsMinimal(dir)
	if err != nil || raise {
		t.Fatalf("raise=%v err=%v", raise, err)
	}
	if err := writeAutoConf(auto, archivingSettings(archiveOn, raise)); err != nil {
		t.Fatal(err)
	}
	if body := readConfFile(t, auto); !strings.Contains(body, "wal_level = 'logical'") {
		t.Fatalf("a wal_level set with ALTER SYSTEM was removed:\n%s", body)
	}
}

// The write goes through a temporary file, so an interruption before the rename leaves the old
// file whole and no temporary file behind.
func TestAnInterruptedAutoConfWriteLeavesTheOldFileIntact(t *testing.T) {
	dir := t.TempDir()
	auto := filepath.Join(dir, "postgresql.auto.conf")
	old := "listen_addresses = 'localhost'\n"
	writeConfFile(t, auto, old)

	err := writeAutoConfWith(auto, archivingSettings(archiveOn, true), func() error {
		return errors.New("interrupted")
	})
	if err == nil {
		t.Fatal("an interrupted write reported success")
	}
	if got := readConfFile(t, auto); got != old {
		t.Fatalf("the interrupted write changed the file:\n%s", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temporary files left behind: %v", names)
	}
}
