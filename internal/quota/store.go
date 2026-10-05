package quota

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The local store is where readings that only exist for a moment are kept: a status line
// payload is gone as soon as the shim exits, and a Cursor reading is too expensive to take
// on every tick. Layout, under $CONDUCTOR_STATE_DIR/quota or ~/.conductor/quota (0700):
//
//	snapshots/<hash>.json   one file per window per login; replaced whole, never edited,
//	                        so concurrent status line shims cannot tear each other's writes
//	alerts.json             the levels already raised per window (once per window per level)
//	alerts.lock             flock guarding alerts.json across wrap sidecars
//
// Nothing here is sent anywhere as-is; the state directory each reading came from stays in
// these files so this machine can build a resume command.

// Dir is this machine's quota store.
func Dir() (string, error) {
	if v := os.Getenv("CONDUCTOR_STATE_DIR"); v != "" {
		return filepath.Join(v, "quota"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "quota"), nil
}

// storeRetention drops readings nobody has refreshed in longer than any window lasts.
const storeRetention = 35 * 24 * time.Hour

type storedSnapshot struct {
	Snapshot
	StateDir string `json:"state_dir,omitempty"`
}

func snapshotFile(dir string, s Snapshot) string {
	sum := sha256.Sum256([]byte(s.Key()))
	return filepath.Join(dir, "snapshots", hex.EncodeToString(sum[:12])+".json")
}

// Save records readings, each replacing the previous reading of its window unless that one
// is newer.
func Save(snaps []Snapshot) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "snapshots"), 0o700); err != nil {
		return err
	}
	for _, s := range snaps {
		path := snapshotFile(dir, s)
		if prev, ok := readSnapshot(path); ok && prev.ObservedAt.After(s.ObservedAt) {
			continue
		}
		body, err := json.Marshal(storedSnapshot{Snapshot: s, StateDir: s.StateDir})
		if err != nil {
			return err
		}
		if err := writeAtomic(path, body); err != nil {
			return err
		}
	}
	return nil
}

func readSnapshot(path string) (Snapshot, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, false
	}
	var st storedSnapshot
	if json.Unmarshal(body, &st) != nil || st.Harness == "" || st.Window == "" {
		return Snapshot{}, false
	}
	s := st.Snapshot
	s.StateDir = st.StateDir
	return s, true
}

// Load returns every stored reading younger than the retention period.
func Load(now time.Time) []Snapshot {
	dir, err := Dir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(dir, "snapshots"))
	if err != nil {
		return nil
	}
	var out []Snapshot
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, "snapshots", e.Name())
		s, ok := readSnapshot(path)
		if !ok {
			continue
		}
		if now.Sub(s.ObservedAt) > storeRetention {
			_ = os.Remove(path)
			continue
		}
		out = append(out, s)
	}
	return Latest(out)
}

func writeAtomic(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// alertBook is alerts.json: window key → level → the mark raised.
type alertBook map[string]map[Level]*Mark

// RaiseLocal decides which readings cross a level for the first time in their window on
// this machine, records the marks, and returns the alerts to act on. It holds a file lock
// for the read-modify-write so two wrap sidecars watching the same login do not both
// notify.
func RaiseLocal(snaps []Snapshot, t Thresholds, now time.Time) ([]Alert, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "alerts.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	path := filepath.Join(dir, "alerts.json")
	book := alertBook{}
	if body, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(body, &book) // a torn or foreign file starts the book over
	}
	alerts, changed := raise(book, snaps, t, now)
	if !changed {
		return alerts, nil
	}
	// Forget marks for windows that ended long ago, so the book does not grow forever.
	for key, marks := range book {
		for lvl, m := range marks {
			if m == nil || now.Sub(m.At) > storeRetention {
				delete(marks, lvl)
			}
		}
		if len(marks) == 0 {
			delete(book, key)
		}
	}
	body, err := json.Marshal(book)
	if err != nil {
		return nil, err
	}
	return alerts, writeAtomic(path, body)
}

// raise is RaiseLocal's decision, separated from the file so it can be tested directly.
func raise(book alertBook, snaps []Snapshot, t Thresholds, now time.Time) (alerts []Alert, changed bool) {
	for _, s := range snaps {
		key := s.Key()
		lvl, mark := Decide(book[key], s, t, now)
		if len(mark) == 0 {
			continue
		}
		if book[key] == nil {
			book[key] = map[Level]*Mark{}
		}
		for _, l := range mark {
			book[key][l] = &Mark{At: now.UTC(), ResetsAt: s.ResetsAt}
		}
		changed = true
		if lvl != "" {
			alerts = append(alerts, Alert{Level: lvl, Snapshot: s})
		}
	}
	return alerts, changed
}
