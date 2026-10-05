package checkpoint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aburan28/conductor/internal/usage"
)

// ResumeOptions says where and in what a checkpoint should come back to life.
type ResumeOptions struct {
	// Target is the harness to resume in. Empty means the one the checkpoint came from.
	Target string
	// Cwd is the working directory the session resumes in — the restored checkout.
	Cwd string
	// StateDir overrides the target harness's state directory: CLAUDE_CONFIG_DIR for
	// Claude Code, CODEX_HOME for Codex, XDG_DATA_HOME for OpenCode. This is the handle for
	// "a different account": each directory holds its own login and its own sessions.
	StateDir string
	Force    bool
	Getenv   func(string) string
}

// Launch is how to start the resumed session. The caller runs it (or prints it).
type Launch struct {
	Harness string   `json:"harness"`
	Argv    []string `json:"argv"`
	Env     []string `json:"env,omitempty"` // KEY=value additions
	Dir     string   `json:"dir"`
	// Native is true when the harness reopens its own transcript; false when it starts
	// from the continuation prompt because the harness changed.
	Native       bool     `json:"native"`
	SessionID    string   `json:"session_id,omitempty"`
	Transcript   string   `json:"transcript,omitempty"`   // where the native transcript was installed
	Continuation string   `json:"continuation,omitempty"` // where CONTINUATION.md was written
	Notes        []string `json:"notes,omitempty"`
}

// PrepareResume installs what the target harness needs and returns the command that
// opens the session. It does not touch the working tree; RestoreWorkspace does that.
func PrepareResume(ctx context.Context, b *Bundle, opts ResumeOptions) (Launch, error) {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	m := b.Manifest
	target := NormalizeHarness(opts.Target)
	if target == "" {
		target = NormalizeHarness(m.Harness)
	}
	if opts.Cwd == "" {
		opts.Cwd, _ = os.Getwd()
	}
	l := Launch{Harness: target, Dir: opts.Cwd}

	if target == NormalizeHarness(m.Harness) {
		l.Native = true
		switch target {
		case "claude":
			cfg := opts.StateDir
			if cfg == "" {
				cfg = usage.ClaudeConfigDir(opts.Getenv)
			}
			inst, err := InstallClaude(b, cfg, opts.Cwd, opts.Force)
			if err != nil {
				return l, err
			}
			// Resume by path, not id: an id is looked up across every project directory and
			// fails when the same session exists in two of them, which a hand-copied
			// transcript on the original machine would be.
			l.Argv = []string{"claude", "--resume", inst.Path}
			l.SessionID, l.Transcript = inst.SessionID, inst.Path
			if opts.StateDir != "" {
				l.Env = append(l.Env, "CLAUDE_CONFIG_DIR="+cfg)
				l.Notes = append(l.Notes, "using the login in "+cfg)
			}
		case "codex":
			home := opts.StateDir
			if home == "" {
				home = usage.CodexHome(opts.Getenv)
			}
			inst, err := InstallCodex(b, home, opts.Cwd, opts.Force)
			if err != nil {
				return l, err
			}
			l.Argv = []string{"codex", "resume", inst.SessionID}
			l.SessionID, l.Transcript = inst.SessionID, inst.Path
			if opts.StateDir != "" {
				l.Env = append(l.Env, "CODEX_HOME="+home)
				l.Notes = append(l.Notes, "using the login in "+home)
			}
		case "opencode":
			if opts.StateDir != "" {
				l.Env = append(l.Env, "XDG_DATA_HOME="+opts.StateDir)
				l.Notes = append(l.Notes, "using the OpenCode data in "+opts.StateDir)
			}
			id, err := InstallOpenCode(ctx, b, opts.Cwd)
			if err != nil {
				return l, err
			}
			l.Argv = []string{"opencode", "--session", id}
			l.SessionID = id
		default:
			return l, fmt.Errorf("cannot resume a %s session natively", target)
		}
		return l, nil
	}

	// A different harness cannot read the original's transcript; it starts from the
	// continuation, which is the whole reason the continuation exists.
	cont, err := WriteContinuation(b, opts.Cwd)
	if err != nil {
		return l, err
	}
	l.Continuation = cont
	prompt := ContinuationPrompt(cont)
	switch target {
	case "claude":
		l.Argv = []string{"claude", prompt}
	case "codex":
		l.Argv = []string{"codex", prompt}
	case "opencode":
		l.Argv = []string{"opencode", "--prompt", prompt}
	default:
		return l, fmt.Errorf("cannot resume into harness %q (claude, codex, opencode)", target)
	}
	switch target {
	case "claude":
		if opts.StateDir != "" {
			l.Env = append(l.Env, "CLAUDE_CONFIG_DIR="+opts.StateDir)
		}
	case "codex":
		if opts.StateDir != "" {
			l.Env = append(l.Env, "CODEX_HOME="+opts.StateDir)
		}
	case "opencode":
		if opts.StateDir != "" {
			l.Env = append(l.Env, "XDG_DATA_HOME="+opts.StateDir)
		}
	}
	l.Notes = append(l.Notes, fmt.Sprintf("%s cannot read a %s transcript; it starts from %s", harnessTitle(target), harnessTitle(m.Harness), cont))
	return l, nil
}

// WriteContinuation writes the bundle's CONTINUATION.md where the resumed agent can read
// it: under .conductor/generated/ in the repository (which is gitignored by `conductor
// init`), or beside the checkpoint store when there is no repository.
func WriteContinuation(b *Bundle, cwd string) (string, error) {
	data, ok := b.File(ContinuationPath)
	if !ok {
		return "", fmt.Errorf("checkpoint %s carries no continuation", b.Manifest.ID)
	}
	var dir string
	if root := gitLine(context.Background(), cwd, "rev-parse", "--show-toplevel"); root != "" {
		dir = filepath.Join(root, ".conductor", "generated", "continuations")
	} else {
		base, err := Dir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "continuations")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, b.Manifest.ID+".md")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// ContinuationPrompt is the first message a cross-harness resume sends. It points at the
// file rather than inlining it: a continuation can be larger than an argv, and a file the
// agent reads itself lands in its context the way its own tools present files.
func ContinuationPrompt(path string) string {
	return strings.TrimSpace(fmt.Sprintf(`You are resuming a coding session that another agent had to stop. Everything you need is in %s: read that file in full before doing anything else, then continue the work from its "Where the work stands" section. Do not start over.`, path))
}
