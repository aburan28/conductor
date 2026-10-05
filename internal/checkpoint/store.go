package checkpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The local store is a flat directory of bundles with a sidecar manifest beside each:
//
//	$CONDUCTOR_STATE_DIR/checkpoints/<id>.ckpt   the bundle (gzip tar, never encrypted here)
//	$CONDUCTOR_STATE_DIR/checkpoints/<id>.json   its manifest, for listing without opening
//
// Same directory root and the same 0700/0600 posture as the session records next door.
// Keeping the local copy in the clear is deliberate: the transcript it holds is already on
// this disk in the harness's own store, in the clear, owned by the same user.

const (
	BundleExt   = ".ckpt"
	ManifestExt = ".json"
)

// Dir is where this machine keeps checkpoints.
func Dir() (string, error) {
	if v := os.Getenv("CONDUCTOR_STATE_DIR"); v != "" {
		return filepath.Join(v, "checkpoints"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "checkpoints"), nil
}

// Put writes a sealed-by-Builder archive and its manifest atomically into the store and
// returns the bundle's path.
func Put(m Manifest, archive []byte) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := atomicWrite(filepath.Join(dir, m.ID+BundleExt), archive); err != nil {
		return "", err
	}
	side, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := atomicWrite(filepath.Join(dir, m.ID+ManifestExt), side); err != nil {
		return "", err
	}
	return filepath.Join(dir, m.ID+BundleExt), nil
}

func atomicWrite(p string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".ck-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, p)
}

// List returns every checkpoint in the store, newest first.
func List() ([]Manifest, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ManifestExt) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m Manifest
		if json.Unmarshal(data, &m) != nil || m.Validate() != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, m.ID+BundleExt)); err != nil {
			continue // a manifest whose bundle is gone describes nothing
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// Latest returns the newest checkpoint of a session, if any.
func Latest(harness, sessionID string) (Manifest, bool, error) {
	all, err := List()
	if err != nil {
		return Manifest{}, false, err
	}
	for _, m := range all {
		if m.Harness == harness && m.SessionID == sessionID {
			return m, true, nil
		}
	}
	return Manifest{}, false, nil
}

// Path is where a checkpoint's bundle lives in the store.
func Path(id string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+BundleExt), nil
}

// Resolve turns what a person typed — a full id, a unique prefix, the random tail, a
// session id (its newest checkpoint), or `latest` — into one checkpoint.
func Resolve(ref string) (Manifest, error) {
	all, err := List()
	if err != nil {
		return Manifest{}, err
	}
	if len(all) == 0 {
		return Manifest{}, errors.New("no checkpoints on this machine")
	}
	if ref == "" || ref == "latest" {
		return all[0], nil
	}
	var hits []Manifest
	for _, m := range all {
		switch {
		case m.ID == ref:
			return m, nil
		case strings.HasPrefix(m.ID, ref), ShortID(m.ID) == ref, strings.HasPrefix(ShortID(m.ID), ref) && len(ref) >= 3:
			hits = append(hits, m)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		return Manifest{}, fmt.Errorf("%q matches %d checkpoints; give more of the id", ref, len(hits))
	}
	// A session id, or a prefix of one: its newest checkpoint.
	for _, m := range all {
		if m.SessionID == ref || (len(ref) >= 6 && strings.HasPrefix(m.SessionID, ref)) || SessionKey(m.Harness, m.SessionID) == ref {
			return m, nil
		}
	}
	return Manifest{}, fmt.Errorf("no checkpoint matches %q", ref)
}

// Remove deletes a checkpoint and its manifest.
func Remove(id string) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	var first error
	for _, p := range []string{filepath.Join(dir, id+BundleExt), filepath.Join(dir, id+ManifestExt)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	return first
}

// Prune keeps the newest `keep` checkpoints of every session and removes the rest. It never
// touches a checkpoint younger than minAge, so a burst of captures around a shutdown is
// not thinned before anyone could have used it. It returns what it removed.
func Prune(keep int, minAge time.Duration, now time.Time) ([]Manifest, error) {
	if keep < 1 {
		keep = 1
	}
	all, err := List()
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	var removed []Manifest
	for _, m := range all { // newest first
		key := SessionKey(m.Harness, m.SessionID)
		seen[key]++
		if seen[key] <= keep || now.Sub(m.CreatedAt) < minAge {
			continue
		}
		if err := Remove(m.ID); err != nil {
			return removed, err
		}
		removed = append(removed, m)
	}
	return removed, nil
}

// Load opens a stored checkpoint by manifest.
func Load(m Manifest) (*Bundle, error) {
	p, err := Path(m.ID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return Open(bytes.NewReader(data))
}
