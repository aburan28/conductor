// Package checkpoint makes a running agent session portable.
//
// A harness keeps its conversation in its own local store, keyed to one machine, one
// account, and one working directory: Claude Code under ~/.claude/projects, Codex under
// ~/.codex/sessions, OpenCode in its database. When that account hits a usage limit, the
// machine goes away, or the work should continue in a different tool, the conversation is
// stranded exactly where it is. A checkpoint is a self-contained bundle of everything needed
// to pick the work up somewhere else:
//
//   - the harness's own transcript, verbatim, so the SAME harness can resume the SAME
//     conversation under another account or on another machine;
//   - the working tree — uncommitted changes, untracked files, and any commits not yet on a
//     remote — so the code the conversation refers to travels with it;
//   - a harness-neutral CONTINUATION.md distilled from the transcript, so a DIFFERENT
//     harness (Claude → Codex, Codex → OpenCode) can take the work over from a prompt.
//
// Where this sits in Conductor's privacy model matters. A checkpoint contains the
// conversation, which is the one thing the control plane must never hold (DESIGN.md §12.4,
// project.yaml `privacy.transcriptStorage: local_only`). So checkpoints are written by the
// CLI on the user's own machine, under the same 0700 directory as their credentials, and
// leave it only as a file the user moves themselves or as ciphertext in the user's own
// bucket. No type in this package is ever sent to the control plane, and nothing in
// internal/domain or internal/api imports it.
package checkpoint

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Schema is the manifest format version written by this build.
const Schema = 1

// Reasons a checkpoint was taken. Free text is allowed; these are the ones Conductor itself
// uses, so `conductor checkpoint list` can explain a row.
const (
	ReasonManual   = "manual"
	ReasonPeriodic = "periodic"
	ReasonExit     = "exit"
	ReasonSignal   = "signal"
	ReasonHook     = "hook"
	ReasonAgent    = "agent"
)

// Manifest describes one checkpoint. It is the first entry of every bundle and is also
// written beside the bundle in the local store so listing never has to open an archive.
type Manifest struct {
	Schema    int       `json:"schema"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Machine   string    `json:"machine,omitempty"`
	// Reason is why this checkpoint exists (periodic tick, exit, a hook, an agent's own
	// request); Note is a human- or agent-written line about where the work stands.
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`

	Harness        string `json:"harness"`
	HarnessVersion string `json:"harness_version,omitempty"`
	// SessionID is the harness's own conversation id: the Claude Code session uuid, the Codex
	// rollout id, the OpenCode session id. It is what a same-harness resume reopens.
	SessionID string `json:"session_id"`
	// Title is the harness's own label for the conversation where it keeps one.
	Title string `json:"title,omitempty"`
	Cwd   string `json:"cwd"`

	Repo      Repo         `json:"repo"`
	Conductor ConductorRef `json:"conductor,omitempty"`

	// Parent is the previous checkpoint of the same session on this machine; ResumedFrom is
	// the checkpoint a session was started from, when it was. Together they let a lineage be
	// followed across machines, accounts, and harnesses.
	Parent      string `json:"parent,omitempty"`
	ResumedFrom string `json:"resumed_from,omitempty"`

	Transcript Transcript `json:"transcript"`
	Workspace  Workspace  `json:"workspace"`
	// Fingerprint summarises the transcript and workspace content; a capture whose
	// fingerprint equals the previous checkpoint's writes nothing.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Files lists every entry in the bundle with its size and sha256, so a bundle can be
	// verified before it is trusted.
	Files []FileEntry `json:"files"`
}

// Repo is the git state the conversation was working against.
type Repo struct {
	Root      string `json:"root,omitempty"`
	Remote    string `json:"remote,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Head      string `json:"head,omitempty"`
	Dirty     bool   `json:"dirty"`
	Subdir    string `json:"subdir,omitempty"` // cwd relative to the root, when they differ
	Ahead     int    `json:"ahead,omitempty"`  // commits in HEAD not on any remote ref
	Untracked int    `json:"untracked,omitempty"`
	Modified  int    `json:"modified,omitempty"`
}

// ConductorRef ties the checkpoint to coordination state when the session was wrapped or
// had claimed a task. Identifiers only; the task's content lives in the control plane.
type ConductorRef struct {
	Project   string `json:"project,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Task      string `json:"task,omitempty"`
	Principal string `json:"principal,omitempty"`
}

// Transcript describes the native transcript carried in the bundle.
type Transcript struct {
	Path           string    `json:"path"` // within the bundle
	Bytes          int64     `json:"bytes"`
	Records        int       `json:"records,omitempty"`
	UserTurns      int       `json:"user_turns,omitempty"`
	AssistantTurns int       `json:"assistant_turns,omitempty"`
	LastActivity   time.Time `json:"last_activity,omitzero"`
	// NativeRelPath is where the transcript lived relative to the harness's state root
	// (e.g. sessions/2026/10/05/rollout-….jsonl for Codex), so a resume can put it back
	// where the harness expects to find it.
	NativeRelPath string `json:"native_rel_path,omitempty"`
	// Extras are additional native files (Claude's per-session directory of subagent
	// transcripts and tool results), relative to the bundle's native/ directory.
	Extras int `json:"extras,omitempty"`
}

// Workspace describes the working-tree state carried in the bundle.
type Workspace struct {
	PatchBytes     int64    `json:"patch_bytes,omitempty"`
	UntrackedFiles []string `json:"untracked_files,omitempty"`
	CommitsBundle  bool     `json:"commits_bundle,omitempty"`
	Skipped        string   `json:"skipped,omitempty"` // why no workspace was captured
}

// IndexEntry is what may sit beside a sealed checkpoint in a bucket, in the clear: enough to
// list and pick a checkpoint, and nothing about the conversation or the machine. The full
// manifest carries the harness's own conversation title, the note, the working directory,
// the repository, and the machine name — all of which say what someone was working on — so
// it travels only inside the sealed bundle.
type IndexEntry struct {
	Schema      int       `json:"schema"`
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"created_at"`
	Harness     string    `json:"harness"`
	Sealed      bool      `json:"sealed"`
	SealedBytes int64     `json:"sealed_bytes"`
}

// Index builds the bucket-side entry for this manifest's sealed bundle.
func (m Manifest) Index(sealedBytes int64) IndexEntry {
	return IndexEntry{Schema: m.Schema, ID: m.ID, CreatedAt: m.CreatedAt, Harness: m.Harness,
		Sealed: true, SealedBytes: sealedBytes}
}

// FileEntry is one archive member.
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Bundle paths. Everything a resume needs is addressed by these.
const (
	ManifestPath     = "manifest.json"
	ContinuationPath = "CONTINUATION.md"
	NativeDir        = "native"
	WorkspaceDir     = "workspace"
	TrackedPatchPath = WorkspaceDir + "/tracked.patch"
	UntrackedDir     = WorkspaceDir + "/untracked"
	CommitsBundle    = WorkspaceDir + "/commits.bundle"
)

// NewID mints a checkpoint id: sortable by time, unique by six random bytes.
func NewID(now time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "ck-" + now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// SessionKey names a session across harnesses, for grouping checkpoints.
func SessionKey(harness, sessionID string) string {
	return harness + ":" + sessionID
}

// ShortID is the suffix people type: the random tail is unique on one machine, and
// `conductor checkpoint` accepts any unambiguous prefix of the full id as well.
func ShortID(id string) string {
	if i := strings.LastIndex(id, "-"); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

// Validate checks the invariants every manifest must hold before anything acts on it.
//
// A manifest can arrive from anywhere — a file someone sent, a bucket someone else can write
// — and its identifiers become file names (the session id names the installed transcript, the
// id names the continuation file) and command-line arguments (`codex resume <session>`). So
// they are held to a plain identifier alphabet: nothing that could climb out of a directory,
// name an absolute path, or be read as an option.
func (m Manifest) Validate() error {
	switch {
	case m.Schema != Schema:
		return fmt.Errorf("checkpoint schema %d is not supported by this build (wants %d)", m.Schema, Schema)
	case m.ID == "":
		return fmt.Errorf("checkpoint has no id")
	case !SafeIdentifier(m.ID):
		return fmt.Errorf("checkpoint id %q is not a plain identifier", m.ID)
	case m.Harness == "":
		return fmt.Errorf("checkpoint %s names no harness", m.ID)
	case !SafeIdentifier(m.Harness):
		return fmt.Errorf("checkpoint %s names harness %q, which is not a plain identifier", m.ID, m.Harness)
	case m.SessionID == "":
		return fmt.Errorf("checkpoint %s names no session", m.ID)
	case !SafeIdentifier(m.SessionID):
		return fmt.Errorf("checkpoint %s names session %q, which is not a plain identifier", m.ID, m.SessionID)
	case m.Transcript.NativeRelPath != "" && !SafeRelPath(m.Transcript.NativeRelPath):
		return fmt.Errorf("checkpoint %s records a transcript path that leaves its directory", m.ID)
	}
	return nil
}

// SafeIdentifier reports whether s is a plain identifier: letters, digits, '.', '_' and '-',
// starting with a letter or digit, at most 200 bytes, and never "..".
func SafeIdentifier(s string) bool {
	if s == "" || len(s) > 200 || strings.Contains(s, "..") {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// SafeRelPath reports whether p is a relative, forward-slash path that stays inside the
// directory it is joined to: no absolute path, no ".." component, no backslash, no NUL.
func SafeRelPath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
