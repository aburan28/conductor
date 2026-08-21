// Package localstate tracks the interactive harness sessions running on this machine, so
// `conductor pause` can freeze a wall of agent terminals and `conductor resume` can bring it
// back — waking each process where its terminal survived, reopening a terminal where it did
// not.
//
// This is per-user, per-machine state: not project policy (which lives in the repository) and
// not coordination state (which lives in the control plane). It records process facts — pids,
// process groups, ttys, working directories, argument vectors — and never a word of
// conversation. One caution follows from that: an argv can contain a prompt (`claude "fix the
// login bug"`), and a prompt is owner-private, so records are written 0600 in a 0700
// directory, the same posture as the credentials file next to them.
package localstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Record statuses. There are only two: a session this machine believes is live, and a session
// deliberately frozen by `conductor pause`. Everything else — crashed, exited, terminal
// closed mid-run — is expressed by removing the record, not by another status.
const (
	StatusRunning = "running"
	StatusPaused  = "paused"
)

// Record is one interactive harness session on this machine.
type Record struct {
	ID      string `json:"id"`
	Harness string `json:"harness"`
	// Command and Args are how the harness was invoked, kept for display and as the fallback
	// relaunch when a harness has no known resume invocation.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// ResumeArgs is the argv tail that reopens this harness's most recent conversation. It is
	// computed when the record is written, not when it is used, so a resume months later (or
	// by a newer Conductor) replays exactly what was decided at save time.
	ResumeArgs []string `json:"resume_args,omitempty"`
	// WrapFlags are Conductor's own flags from a `conductor wrap` invocation (--model,
	// --effort, …), replayed on relaunch so a revived session advertises the same
	// capabilities it registered with.
	WrapFlags []string `json:"wrap_flags,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`

	// PID and PGID identify the harness process itself; WrapPID the `conductor wrap` sidecar
	// when there is one. Pids are recycled, so nothing signals these without re-identifying
	// the process first (VerifyHarness, VerifyWrap).
	PID     int    `json:"pid"`
	PGID    int    `json:"pgid,omitempty"`
	WrapPID int    `json:"wrap_pid,omitempty"`
	TTY     string `json:"tty,omitempty"`
	// TermProgram names the terminal application the session was running in, as its
	// TERM_PROGRAM variable reported it ("vscode", "iTerm.app", …). Resume uses it to reopen
	// a session where it actually lived: a session from a VS Code integrated terminal should
	// come back in VS Code, not in whatever emulator happens to be installed.
	TermProgram string `json:"term_program,omitempty"`

	// SessionID and Project tie a wrapped session back to the control plane; discovered bare
	// sessions have neither.
	SessionID string `json:"session_id,omitempty"`
	Project   string `json:"project,omitempty"`
	Wrapped   bool   `json:"wrapped,omitempty"`

	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	PausedAt  time.Time `json:"paused_at,omitzero"`
}

// Dir is where records live: ~/.conductor/sessions, beside the credentials file, or under
// CONDUCTOR_STATE_DIR when set (tests use the override; so can a machine with an unusual
// home).
func Dir() (string, error) {
	if v := os.Getenv("CONDUCTOR_STATE_DIR"); v != "" {
		return filepath.Join(v, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "sessions"), nil
}

// Save writes a record atomically: temp file, then rename. Pause and resume both rewrite
// records while a wrap sidecar may be about to reread them; a half-written JSON file must
// never be observable.
func Save(r Record) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".rec-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, fileName(r.ID)))
}

// Remove deletes a record. Removing a record that is already gone is not an error: wrap
// sidecars and the pause CLI can race to clean up the same session, and both winning is fine.
func Remove(id string) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, fileName(id)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// List returns every record, oldest first. Corrupt files are skipped rather than fatal — one
// damaged record must not make every session on the machine unpausable.
func List() ([]Record, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(body, &r) != nil || r.ID == "" {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Prune drops records for sessions that ended without cleaning up after themselves and
// returns what remains. A running record whose processes are gone is debris from a crash or a
// closed terminal. A paused record is kept even when its processes are dead: a paused session
// whose terminal was closed is exactly the one `conductor resume` needs to reopen.
func Prune() ([]Record, error) {
	records, err := List()
	if err != nil {
		return nil, err
	}
	live := records[:0]
	for _, r := range records {
		if r.Status == StatusRunning && !r.processAlive() {
			_ = Remove(r.ID)
			continue
		}
		live = append(live, r)
	}
	return live, nil
}

func (r Record) processAlive() bool {
	return Alive(r.PID) || (r.Wrapped && Alive(r.WrapPID))
}

// ResumeInvocation returns the argv tail that reopens a harness's most recent conversation.
// Every harness keeps its transcript in its own local state, so resuming is the harness's
// business — Conductor only remembers which incantation to use.
//
// `claude --continue` and `opencode --continue` reopen the latest conversation for the
// working directory, which is exactly right when every session has its own worktree.
// `codex resume --last` reopens Codex's most recent conversation regardless of directory —
// the closest it offers — so two paused Codex sessions revived on the same machine can land
// on the same conversation; the second one is a keystroke away from the picker (`codex
// resume`).
func ResumeInvocation(harness string) []string {
	switch harness {
	case "claude", "opencode":
		return []string{"--continue"}
	case "codex":
		return []string{"resume", "--last"}
	}
	return nil
}

// fileName scrubs an id into a safe file name. IDs are uuids or "p<pid>", but a record id is
// still an input, and an input must not be able to name a path outside the directory.
func fileName(id string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		}
		return '_'
	}, id)
	return safe + ".json"
}
