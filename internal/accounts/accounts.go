// Package accounts manages named harness login profiles — one Claude Code account,
// one Codex account, one OpenCode account per file — so a machine can move off an
// exhausted account and onto a fresh one without redoing each harness's OAuth dance.
//
// A profile is a copy of the harness's live credential file, stored beside
// Conductor's other machine-local state (0600, like the credentials themselves):
// ~/.conductor/accounts/<harness>/<name>.json. Which account is live is read from
// the harness's own credential file, never tracked separately: the active profile
// is whichever one's bytes match, so switching is a plain file copy and no state
// can drift.
//
// Exhaustion is a marker beside the profile (<name>.exhausted, RFC 3339 inside),
// because a quota window is only knowable by the account that hit it. Whatever
// notices — the user, a wrap sidecar watching usage-limit errors — writes the
// marker; EnsureUsable then prefers any profile without one.
package accounts

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Known harnesses and where each keeps the credentials of the account it runs as.
// Anything else can still be managed by name; it just cannot be switched into a
// live location automatically.
var livePaths = map[string]string{
	"claude":   ".claude/.credentials.json",
	"codex":    ".codex/auth.json",
	"opencode": ".local/share/opencode/auth.json",
}

// Profile is one named account for one harness.
type Profile struct {
	Harness     string     `json:"harness"`
	Name        string     `json:"name"`
	ExhaustedAt *time.Time `json:"exhausted_at,omitempty"` // when the quota window was hit; nil while usable
}

// Harnesses lists the harnesses whose account locations Conductor knows.
func Harnesses() []string {
	return []string{"claude", "codex", "opencode"}
}

// Root is where profiles live: ~/.conductor/accounts, or under
// CONDUCTOR_STATE_DIR when set (tests use the override).
func Root() (string, error) {
	if v := os.Getenv("CONDUCTOR_STATE_DIR"); v != "" {
		return filepath.Join(v, "accounts"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "accounts"), nil
}

// LivePath returns the harness's own credential file — the file the running
// agent actually reads. CONDUCTOR_LIVE_<HARNESS> overrides it (tests, and
// machines with unusual homes). False when the harness has no known location.
func LivePath(harness string) (string, bool) {
	if v := os.Getenv("CONDUCTOR_LIVE_" + strings.ToUpper(harness)); v != "" {
		return v, true
	}
	rel, ok := livePaths[harness]
	if !ok {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, rel), true
}

// List returns a harness's profiles, ordered by name.
func List(harness string) ([]Profile, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, harness))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Profile
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		if name == e.Name() || e.IsDir() {
			continue
		}
		p := Profile{Harness: harness, Name: name}
		if at, err := readExhausted(root, harness, name); err == nil && at != nil {
			p.ExhaustedAt = at
		}
		out = append(out, p)
	}
	return out, nil
}

// Add snapshots the harness's current live credentials as a named profile. It is
// how an account gets into the rotation: log in with the harness as usual, then
// `conductor accounts add <harness> <name>`.
func Add(harness, name string) error {
	if isAlias(harness, name) {
		return fmt.Errorf("%q is an alias for %s; pick another name or `conductor accounts unalias %s %s`", name, harness, harness, name)
	}
	live, ok := LivePath(harness)
	if !ok {
		return fmt.Errorf("no known credential file for harness %q", harness)
	}
	body, err := os.ReadFile(live)
	if err != nil {
		return fmt.Errorf("read %s: %w (log in with the harness first)", live, err)
	}
	root, err := Root()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, harness)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, name+".json"), body)
}

// Switch makes a profile (or an alias for one) the harness's live account by copying its credentials
// over the harness's own file. Harnesses read that file on startup, so switch
// before resuming sessions for them to come up under the new account.
func Switch(harness, name string) error {
	name, err := canonical(harness, name)
	if err != nil {
		return err
	}
	body, err := readProfile(harness, name)
	if err != nil {
		return err
	}
	live, ok := LivePath(harness)
	if !ok {
		return fmt.Errorf("no known credential file for harness %q", harness)
	}
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		return err
	}
	if err := writeFile(live, body); err != nil {
		return err
	}
	// A quota marker belongs to the account, not the file slot; it travels with
	// the profile it was written for.
	return nil
}

// Active names the profile whose bytes match the harness's live credential file —
// the account the harness is actually running as. Empty when none match.
func Active(harness string) string {
	live, ok := LivePath(harness)
	if !ok {
		return ""
	}
	body, err := os.ReadFile(live)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	profiles, err := List(harness)
	if err != nil {
		return ""
	}
	for _, p := range profiles {
		body, err := readProfile(harness, p.Name)
		if err != nil {
			continue
		}
		if other := sha256.Sum256(body); other == sum {
			return p.Name
		}
	}
	return ""
}

// MarkExhausted records that this account's quota window ran out. Until the
// marker is cleared, resume-all will not bring sessions back under it.
func MarkExhausted(harness, name string) error {
	name, err := canonical(harness, name)
	if err != nil {
		return err
	}
	root, err := Root()
	if err != nil {
		return err
	}
	if _, err := readProfile(harness, name); err != nil {
		return err
	}
	at := time.Now().UTC()
	body := []byte(at.Format(time.RFC3339))
	return writeFile(markerPath(root, harness, name), body)
}

// ClearExhausted forgets the marker: the account's window has rolled over, or
// the marker was a false alarm. Clearing an account with no marker is a no-op.
func ClearExhausted(harness, name string) error {
	name, err := canonical(harness, name)
	if err != nil {
		return err
	}
	root, err := Root()
	if err != nil {
		return err
	}
	err = os.Remove(markerPath(root, harness, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// EnsureUsable is the resume-time half of switching: if the harness's active
// account is marked exhausted and another profile is not, switch to the first
// usable one (alphabetically — a stable, predictable order) and return its name.
// An empty return means nothing needed to change.
func EnsureUsable(harness string) (string, error) {
	profiles, err := List(harness)
	if err != nil || len(profiles) < 2 {
		return "", err
	}
	active := Active(harness)
	if active == "" {
		return "", nil
	}
	exhausted := false
	for _, p := range profiles {
		if p.Name == active {
			exhausted = p.ExhaustedAt != nil
			break
		}
	}
	if !exhausted {
		return "", nil
	}
	for _, p := range profiles {
		if p.Name == active || p.ExhaustedAt != nil {
			continue
		}
		if err := Switch(harness, p.Name); err != nil {
			return "", err
		}
		return p.Name, nil
	}
	return "", nil
}

func readProfile(harness, name string) ([]byte, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(root, harness, name+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no account %q for %s (see `conductor accounts list`)", name, harness)
		}
		return nil, err
	}
	return body, nil
}

func markerPath(root, harness, name string) string {
	return filepath.Join(root, harness, name+".exhausted")
}

func readExhausted(root, harness, name string) (*time.Time, error) {
	body, err := os.ReadFile(markerPath(root, harness, name))
	if err != nil {
		return nil, err
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(body)))
	if err != nil {
		return nil, err
	}
	return &at, nil
}

// writeFile stores credentials atomically and owner-only: a credential file
// half-written or world-readable is worse than one that is merely missing.
func writeFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".account-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
