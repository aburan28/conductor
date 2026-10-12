package externalwork

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLatestOwnerOnlyAndAtomic(t *testing.T) {
	dir := t.TempDir()
	path := InventoryPath(dir, filepath.Join(dir, "state"))
	first := Inventory{Version: 1, ScannedAt: time.Now(), Candidates: []Candidate{{Source: "codex", ID: sampleID}}}
	if err := SaveLatest(path, first); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("inventory mode %v", st.Mode().Perm())
	}
	dst, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("inventory directory mode %v", dst.Mode().Perm())
	}
	second := Inventory{Version: 1, ScannedAt: time.Now(), Candidates: []Candidate{}}
	if err := SaveLatest(path, second); err != nil {
		t.Fatal(err)
	}
	got, err := LoadLatest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates) != 0 {
		t.Fatalf("stale inventory: %#v", got.Candidates)
	}
	if names, err := os.ReadDir(filepath.Dir(path)); err != nil || len(names) != 1 {
		t.Fatalf("left temporary file: %v %v", names, err)
	}
}
