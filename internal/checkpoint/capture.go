package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/usage"
)

// Request says which session to checkpoint and why.
type Request struct {
	Harness string
	Cwd     string
	// SessionID names the harness session exactly. Without it the newest transcript for Cwd
	// written since Since is taken, and PID (the harness process, when known) is used to
	// pin the session where the harness records one per process.
	SessionID string
	PID       int
	Since     time.Time

	Reason      string
	Note        string
	Conductor   ConductorRef
	ResumedFrom string

	// SkipWorkspace leaves the working tree out (the transcript alone is still useful).
	SkipWorkspace bool
	// Force writes a checkpoint even when nothing changed since the last one.
	Force bool
	// MinInterval skips a capture when the session's newest checkpoint is younger than
	// this, unless Force is set. It keeps a per-turn hook from writing a bundle per turn.
	MinInterval time.Duration
	// Keep is how many checkpoints of this session to retain after writing (0: default).
	Keep int

	Getenv func(string) string
	Now    func() time.Time
}

// Result is what a capture produced.
type Result struct {
	Manifest Manifest `json:"manifest"`
	Path     string   `json:"path,omitempty"`
	// Skipped says why no bundle was written: nothing changed, or the last one is recent.
	Skipped string `json:"skipped,omitempty"`
	Pruned  int    `json:"pruned,omitempty"`
}

// Defaults.
const (
	DefaultKeep        = 5
	DefaultMinInterval = 30 * time.Second
	pruneMinAge        = 10 * time.Minute
)

// KeepFromEnv reads CONDUCTOR_CHECKPOINT_KEEP.
func KeepFromEnv(getenv func(string) string) int {
	if n, err := strconv.Atoi(getenv("CONDUCTOR_CHECKPOINT_KEEP")); err == nil && n > 0 {
		return n
	}
	return DefaultKeep
}

// Disabled reports whether the operator turned checkpoints off (CONDUCTOR_CHECKPOINT=off).
func Disabled(getenv func(string) string) bool {
	switch strings.ToLower(getenv("CONDUCTOR_CHECKPOINT")) {
	case "off", "0", "false", "no":
		return true
	}
	return false
}

// IntervalFromEnv reads CONDUCTOR_CHECKPOINT_INTERVAL for the periodic capture in
// `conductor wrap`; the default is two minutes.
func IntervalFromEnv(getenv func(string) string) time.Duration {
	if d, err := time.ParseDuration(getenv("CONDUCTOR_CHECKPOINT_INTERVAL")); err == nil && d >= 10*time.Second {
		return d
	}
	return 2 * time.Minute
}

// NormalizeHarness folds the spellings Conductor accepts for a harness into one.
func NormalizeHarness(h string) string {
	switch strings.ToLower(strings.TrimSpace(h)) {
	case "claude", "claude-code", "claudecode":
		return "claude"
	case "codex", "codex-cli":
		return "codex"
	case "opencode", "open-code":
		return "opencode"
	}
	return strings.ToLower(strings.TrimSpace(h))
}

// Capture takes a checkpoint of one session.
func Capture(ctx context.Context, req Request) (Result, error) {
	if req.Getenv == nil {
		req.Getenv = os.Getenv
	}
	if req.Now == nil {
		req.Now = time.Now
	}
	req.Harness = NormalizeHarness(req.Harness)
	if req.Cwd == "" {
		req.Cwd, _ = os.Getwd()
	}
	if req.Reason == "" {
		req.Reason = ReasonManual
	}
	if req.Keep <= 0 {
		req.Keep = KeepFromEnv(req.Getenv)
	}
	now := req.Now().UTC()

	// 1. The native transcript.
	var (
		transcript []byte
		nativePath string // inside the bundle
		relPath    string
		extras     = map[string][]byte{}
		title      string
		conv       *Conversation
	)
	switch req.Harness {
	case "claude":
		cfg := usage.ClaudeConfigDir(req.Getenv)
		if req.SessionID == "" && req.PID > 0 {
			req.SessionID, _ = ClaudeSessionForPID(cfg, req.PID)
		}
		src, err := LocateClaude(cfg, req.Cwd, req.SessionID, req.Since)
		if err != nil {
			return Result{}, err
		}
		data, err := os.ReadFile(src.Path)
		if err != nil {
			return Result{}, err
		}
		transcript = data
		req.SessionID = src.SessionID
		nativePath = NativeDir + "/claude/" + src.SessionID + ".jsonl"
		if rel, err := filepath.Rel(cfg, src.Path); err == nil {
			relPath = filepath.ToSlash(rel)
		}
		side := strings.TrimSuffix(src.Path, ".jsonl")
		for _, p := range src.Extras {
			rel, err := filepath.Rel(side, p)
			if err != nil {
				continue
			}
			if data, err := os.ReadFile(p); err == nil {
				extras[NativeDir+"/claude/"+src.SessionID+"/"+filepath.ToSlash(rel)] = data
			}
		}
		conv = ParseClaude(transcript)
		title = conv.Title
	case "codex":
		home := usage.CodexHome(req.Getenv)
		src, err := LocateCodex(home, req.Cwd, req.SessionID, req.Since)
		if err != nil {
			return Result{}, err
		}
		data, err := os.ReadFile(src.Path)
		if err != nil {
			return Result{}, err
		}
		transcript = data
		req.SessionID = src.SessionID
		nativePath = NativeDir + "/codex/" + filepath.Base(src.Path)
		relPath = src.RelPath
		conv = ParseCodex(transcript)
	case "opencode":
		src, err := LocateOpenCode(ctx, req.Cwd, req.SessionID, req.Since)
		if err != nil {
			return Result{}, err
		}
		transcript = src.Export
		req.SessionID = src.SessionID
		nativePath = NativeDir + "/opencode/" + src.SessionID + ".json"
		conv = ParseOpenCode(transcript)
		title = src.Title
	default:
		return Result{}, fmt.Errorf("checkpoints are not supported for harness %q (claude, codex, opencode)", req.Harness)
	}
	if len(bytes.TrimSpace(transcript)) == 0 {
		return Result{}, errors.New("the session's transcript is empty")
	}

	// 2. Rate limit and change detection against the previous checkpoint.
	prev, havePrev, err := Latest(req.Harness, req.SessionID)
	if err != nil {
		return Result{}, err
	}
	if havePrev && !req.Force {
		min := req.MinInterval
		if min == 0 {
			min = DefaultMinInterval
		}
		automatic := strings.HasPrefix(req.Reason, ReasonPeriodic) || strings.HasPrefix(req.Reason, ReasonHook)
		if automatic && now.Sub(prev.CreatedAt) < min {
			return Result{Manifest: prev, Skipped: "a checkpoint was taken " + now.Sub(prev.CreatedAt).Round(time.Second).String() + " ago"}, nil
		}
	}

	// 3. Stage the bundle.
	var b Builder
	if err := b.Add(nativePath, transcript); err != nil {
		return Result{}, err
	}
	for p, data := range extras {
		_ = b.Add(p, data)
	}
	repo, _ := InspectRepo(ctx, req.Cwd)
	var ws Workspace
	if req.SkipWorkspace {
		ws.Skipped = "skipped by request"
	} else {
		ws = CaptureWorkspace(ctx, repo, &b)
	}

	// Everything staged so far is content; the fingerprint covers all of it, so a tree
	// change with no new transcript line (or the reverse) still counts as a change.
	fp := fingerprintOf(&b, repo.Head)
	if havePrev && !req.Force && prev.Fingerprint == fp {
		return Result{Manifest: prev, Skipped: "nothing has changed since " + prev.ID}, nil
	}

	users, assistants := conv.Counts()
	m := Manifest{
		Schema: Schema, ID: NewID(now), CreatedAt: now,
		Machine: machineID(req.Getenv), Reason: req.Reason, Note: strings.TrimSpace(req.Note),
		Harness: req.Harness, HarnessVersion: conv.Version, SessionID: req.SessionID, Title: title,
		Cwd: req.Cwd, Repo: repo, Conductor: req.Conductor, ResumedFrom: req.ResumedFrom,
		Transcript: Transcript{
			Path: nativePath, Bytes: int64(len(transcript)), Records: conv.Records,
			UserTurns: users, AssistantTurns: assistants, LastActivity: conv.Last,
			NativeRelPath: relPath, Extras: len(extras),
		},
		Workspace: ws, Fingerprint: fp,
	}
	if havePrev {
		m.Parent = prev.ID
	}
	var filesNow []string
	for _, f := range ws.UntrackedFiles {
		filesNow = append(filesNow, f+" (new)")
	}
	if ws.PatchBytes > 0 {
		filesNow = append(filesNow, patchFiles(&b)...)
	}
	if err := b.Add(ContinuationPath, RenderContinuation(m, conv, filesNow)); err != nil {
		return Result{}, err
	}

	// 4. Seal, store, prune.
	var archive bytes.Buffer
	if err := b.Seal(&m, &archive); err != nil {
		return Result{}, err
	}
	path, err := Put(m, archive.Bytes())
	if err != nil {
		return Result{}, err
	}
	res := Result{Manifest: m, Path: path}
	if removed, err := Prune(req.Keep, pruneMinAge, now); err == nil {
		res.Pruned = len(removed)
	}
	return res, nil
}

func fingerprintOf(b *Builder, head string) string {
	parts := [][]byte{[]byte(head)}
	for _, m := range b.members {
		parts = append(parts, []byte(m.path), m.data)
	}
	return Fingerprint(parts...)
}

// patchFiles lists the paths a staged patch touches, for the continuation's file list.
func patchFiles(b *Builder) []string {
	var out []string
	for _, m := range b.members {
		if m.path != TrackedPatchPath {
			continue
		}
		for _, line := range bytes.Split(m.data, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("+++ b/")) {
				out = append(out, string(bytes.TrimPrefix(line, []byte("+++ b/")))+" (modified)")
			}
		}
	}
	return out
}

func machineID(getenv func(string) string) string {
	if v := getenv("CONDUCTOR_MACHINE_ID"); v != "" {
		return v
	}
	h, _ := os.Hostname()
	return h
}
