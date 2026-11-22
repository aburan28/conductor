package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/checkpoint"
	"github.com/aburan28/conductor/internal/usage"
)

// ExportOptions selects what leaves the machine and how it is treated on the way out.
type ExportOptions struct {
	// Tool is "claude" or "codex"; empty auto-detects from the installed homes.
	Tool string
	// Project keeps only sessions whose recorded working directory is this path.
	Project string
	// All keeps every session of the tool, regardless of project.
	All bool
	// Sessions keeps only these session ids (unique prefixes are enough).
	Sessions []string
	// Since keeps sessions updated at or after this time.
	Since time.Time
	// Match keeps only sessions whose text contains this substring.
	Match string
	// IncludeArchived includes Codex's archived_sessions tree.
	IncludeArchived bool

	ClaudeHome string
	CodexHome  string

	// Redact masks likely secrets; AllowSecrets exports them unmasked. Neither means the
	// export refuses a session that contains one.
	Redact       bool
	AllowSecrets bool

	// Source is recorded in the manifest (export, handoff, sync).
	Source string
	// Context carries cancellation for tools that shell out (OpenCode). Nil means Background.
	Context context.Context
}

// Report summarizes what an export wrote.
type Report struct {
	Tool         string   `json:"tool"`
	Home         string   `json:"home"`
	Project      string   `json:"project,omitempty"`
	Sessions     int      `json:"sessions"`
	Files        int      `json:"files"`
	Bytes        int64    `json:"bytes"`
	Redacted     int      `json:"redacted,omitempty"`
	SecretsFound int      `json:"secrets_found,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
	Skipped      []string `json:"skipped,omitempty"`
	ThreadIDs    []string `json:"thread_ids,omitempty"`
}

type candidate struct {
	session    Session
	source     string   // absolute path of the transcript
	data       []byte   // already-read bytes (OpenCode export), instead of a file
	extras     []string // absolute paths of supporting files (Claude session directory)
	compressed bool
}

// ClaudeHome resolves the Claude Code configuration directory: flag, then
// $CLAUDE_CONFIG_DIR, then ~/.claude.
func ClaudeHome(override string) (string, error) {
	return harnessHome(override, "CLAUDE_CONFIG_DIR", ".claude")
}

// CodexHome resolves the Codex home: flag, then $CODEX_HOME, then ~/.codex.
func CodexHome(override string) (string, error) { return harnessHome(override, "CODEX_HOME", ".codex") }

func harnessHome(override, env, dir string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	if v := os.Getenv(env); v != "" {
		return filepath.Abs(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dir), nil
}

// DetectTool picks the tool to act on when none was named: Claude Code when it has a
// projects directory and Codex does not, otherwise Codex. An explicit flag always wins.
func DetectTool(claudeHome, codexHome string) string {
	claudeOK := isDir(filepath.Join(claudeHome, "projects"))
	codexOK := isDir(filepath.Join(codexHome, "sessions"))
	switch {
	case claudeOK && !codexOK:
		return ToolClaude
	case claudeOK && codexOK:
		return ToolClaude
	default:
		return ToolCodex
	}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Export writes a bundle to w. Everything it selects is read once: the same bytes are
// hashed into the archive, scanned for secrets, and (with --match) searched. A bundle is
// only written if at least one session matched.
func Export(w io.Writer, opts ExportOptions) (Report, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	tool := normTool(opts.Tool)
	claudeHome, err := ClaudeHome(opts.ClaudeHome)
	if err != nil {
		return Report{}, err
	}
	codexHome, err := CodexHome(opts.CodexHome)
	if err != nil {
		return Report{}, err
	}
	if tool == "" {
		tool = DetectTool(claudeHome, codexHome)
	}
	home := claudeHome
	switch tool {
	case ToolCodex:
		home = codexHome
	case ToolOpenCode:
		home = ""
	}
	rep := Report{Tool: tool, Home: home, Project: opts.Project}

	cands, warnings, err := selectCandidates(ctx, opts, tool, home)
	if err != nil {
		return rep, err
	}
	rep.Warnings = warnings
	if len(cands) == 0 {
		return rep, ErrNothingSelected
	}

	projectPath := opts.Project
	if projectPath == "" {
		// A --session or --all export has no project flag, but a single-project bundle is
		// still grouped and recognized more usefully when its project is recorded.
		var only string
		distinct := map[string]bool{}
		for _, c := range cands {
			if c.session.OriginalCWD != "" {
				distinct[c.session.OriginalCWD] = true
			}
		}
		if len(distinct) == 1 {
			for p := range distinct {
				only = p
			}
		}
		projectPath = only
	}
	tw := NewWriter(w, tool, home, projectPath)
	tw.Source = opts.Source
	if tw.Source == "" {
		tw.Source = "export"
	}

	for _, c := range cands {
		data, compressed, warn, err := loadSessionBytes(c.source)
		if c.data != nil {
			data, compressed, warn, err = c.data, false, "", nil
		}
		if err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %v (skipped)", c.session.BundlePath, err))
			continue
		}
		if warn != "" {
			rep.Warnings = append(rep.Warnings, warn)
		}
		if opts.Match != "" && !bytes.Contains(data, []byte(opts.Match)) {
			continue
		}
		data, redacted, findings := applySecretPolicy(data, c.session.BundlePath, &rep, opts)
		if data == nil {
			return rep, &SecretsError{File: c.session.BundlePath, Findings: findings}
		}
		if redacted && compressed {
			// A redacted session must be stored as plain JSONL: rewriting compressed bytes is
			// not possible without recompressing, and a placeholder inside a zstd frame would
			// corrupt it.
			c.session.BundlePath = strings.TrimSuffix(c.session.BundlePath, ".zst")
			c.session.Compressed = false
		}
		if !redacted && compressed {
			c.session.Compressed = true
		}
		s, err := tw.AddSession(c.session, bytes.NewReader(data))
		if err != nil {
			return rep, err
		}
		rep.Sessions++
		rep.Files++
		rep.Bytes += s.SizeBytes
		rep.ThreadIDs = append(rep.ThreadIDs, s.ThreadID)

		for _, extra := range c.extras {
			rel, err := filepath.Rel(home, extra)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if tw.Has(rel) {
				continue
			}
			xdata, err := os.ReadFile(extra)
			if err != nil {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %v (skipped)", rel, err))
				continue
			}
			xdata, _, xfindings := applySecretPolicy(xdata, rel, &rep, opts)
			if xdata == nil {
				return rep, &SecretsError{File: rel, Findings: xfindings}
			}
			if err := tw.AddExtra(rel, bytes.NewReader(xdata)); err != nil {
				return rep, err
			}
			rep.Files++
			rep.Bytes += int64(len(xdata))
		}
	}
	if rep.Sessions == 0 {
		return rep, ErrNothingSelected
	}
	tw.Redacted = rep.Redacted
	tw.SecretsFound = rep.SecretsFound
	if err := tw.Close(); err != nil {
		return rep, err
	}
	return rep, nil
}

// applySecretPolicy enforces the gate on one file's bytes. It returns the bytes to write
// (possibly redacted), whether redaction happened, and the findings when it refused.
func applySecretPolicy(data []byte, rel string, rep *Report, opts ExportOptions) ([]byte, bool, []Finding) {
	findings := ScanSecrets(data)
	if len(findings) == 0 {
		return data, false, nil
	}
	rep.SecretsFound += len(findings)
	if opts.Redact {
		out, n := RedactSecrets(data)
		rep.Redacted += n
		return out, true, nil
	}
	if opts.AllowSecrets {
		return data, false, nil
	}
	return nil, false, findings
}

func loadSessionBytes(p string) (data []byte, compressed bool, warn string, err error) {
	if strings.HasSuffix(p, ".zst") {
		if zstdAvailable() {
			d, derr := decompressZstd(p, HardMaxSessionBytes)
			return d, true, "", derr
		}
		raw, rerr := os.ReadFile(p)
		return raw, true, fmt.Sprintf("%s: compressed and the optional zstd tool is missing; copied as-is (its text cannot be scanned or filtered)", p), rerr
	}
	data, err = os.ReadFile(p)
	return data, false, "", err
}

func selectCandidates(ctx context.Context, opts ExportOptions, tool, home string) ([]candidate, []string, error) {
	switch tool {
	case ToolClaude:
		return selectClaude(opts, home)
	case ToolOpenCode:
		return selectOpenCode(ctx, opts)
	default:
		return selectCodex(opts, home)
	}
}

// selectOpenCode asks the installed OpenCode for the sessions it associates with the
// project directory (its list command is project-scoped) and exports each one.
func selectOpenCode(ctx context.Context, opts ExportOptions) ([]candidate, []string, error) {
	if !opencodeAvailable() {
		return nil, []string{"the opencode CLI is not installed (nothing to export)"}, nil
	}
	dir := opts.Project
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, nil, err
		}
		dir = wd
	}
	sessions, err := listOpenCode(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	var out []candidate
	var warns []string
	for _, row := range sessions {
		if !matchesAnyID(opts.Sessions, row.ID) {
			continue
		}
		if !opts.Since.IsZero() && time.UnixMilli(row.Updated).Before(opts.Since) {
			continue
		}
		data, err := exportOpenCode(ctx, dir, row.ID)
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s: %v (skipped)", row.ID, err))
			continue
		}
		created, updated, cwd, title := openCodeExportTime(data)
		if opts.Project != "" && !opts.All && cwd != "" && !samePath(cwd, opts.Project) {
			continue
		}
		if title == "" {
			title = row.Title
		}
		at := row.Updated
		if at == 0 {
			at = updated
		}
		if at == 0 {
			at = created
		}
		out = append(out, candidate{
			session: Session{
				ThreadID:     row.ID,
				OriginalPath: "opencode://" + row.ID,
				BundlePath:   "opencode/" + row.ID + ".json",
				OriginalCWD:  cwd,
				Preview:      title,
				UpdatedAt:    time.UnixMilli(at).UTC().Format(time.RFC3339),
				SizeBytes:    int64(len(data)),
			},
			data: data,
		})
	}
	sortCandidates(out)
	return out, warns, nil
}

func selectClaude(opts ExportOptions, home string) ([]candidate, []string, error) {
	rows, err := checkpoint.InventoryClaude(home, "")
	if err != nil {
		// A home with no projects directory has no sessions; that is an empty selection, not
		// a failure (a first sync on a fresh machine is exactly this).
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var out []candidate
	var warns []string
	for _, r := range rows {
		if !matchesAnyID(opts.Sessions, r.SessionID) {
			continue
		}
		if r.Problem != "" {
			warns = append(warns, fmt.Sprintf("%s: %s (skipped)", r.Path, r.Problem))
			continue
		}
		if opts.Project != "" && (!opts.All && !samePath(r.Cwd, opts.Project)) {
			continue
		}
		if !opts.Since.IsZero() && r.LastActivity.Before(opts.Since) {
			continue
		}
		rel, err := filepath.Rel(home, r.Path)
		if err != nil || !SafeRel(filepath.ToSlash(rel)) {
			warns = append(warns, fmt.Sprintf("%s: not under %s (skipped)", r.Path, home))
			continue
		}
		c := candidate{
			session: Session{
				ThreadID:     r.SessionID,
				OriginalPath: r.Path,
				BundlePath:   filepath.ToSlash(rel),
				OriginalCWD:  r.Cwd,
				Preview:      r.Title,
				UpdatedAt:    r.LastActivity.UTC().Format(time.RFC3339),
				SizeBytes:    r.Bytes,
				SHA256:       r.SHA256,
				Compressed:   false,
			},
			source: r.Path,
		}
		c.extras = claudeExtras(home, r.Path)
		out = append(out, c)
	}
	sortCandidates(out)
	return out, warns, nil
}

// claudeExtras lists the per-session directory Claude Code writes beside a transcript
// (subagent transcripts and spilled tool results). Only regular files are carried.
func claudeExtras(home, transcript string) []string {
	dir := strings.TrimSuffix(transcript, ".jsonl")
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return nil
	}
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(home, p)
		if err != nil || !SafeRel(filepath.ToSlash(rel)) {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Size() <= 8<<20 {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func selectCodex(opts ExportOptions, home string) ([]candidate, []string, error) {
	roots := []string{filepath.Join(home, "sessions")}
	if opts.IncludeArchived {
		roots = append(roots, filepath.Join(home, "archived_sessions"))
	}
	var out []candidate
	var warns []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			name := d.Name()
			if !strings.HasPrefix(name, "rollout-") || (!strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".jsonl.zst")) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if !opts.Since.IsZero() && info.ModTime().Before(opts.Since) {
				return nil
			}
			rel, err := filepath.Rel(home, p)
			if err != nil || !SafeRel(filepath.ToSlash(rel)) {
				return nil
			}
			compressed := strings.HasSuffix(name, ".zst")
			meta, metaErr := codexMeta(p, compressed)
			if metaErr != nil {
				warns = append(warns, fmt.Sprintf("%s: %v (session cannot be identified; use --all or --session)", p, metaErr))
				if opts.Project != "" && !opts.All {
					return nil
				}
			}
			id := meta.ID
			if id == "" {
				id = codexIDFromName(name)
			}
			if !matchesAnyID(opts.Sessions, id) {
				return nil
			}
			if opts.Project != "" && !opts.All && !samePath(meta.Cwd, opts.Project) {
				return nil
			}
			out = append(out, candidate{
				session: Session{
					ThreadID:      id,
					OriginalPath:  p,
					BundlePath:    filepath.ToSlash(rel),
					OriginalCWD:   meta.Cwd,
					CreatedAt:     meta.Timestamp,
					UpdatedAt:     info.ModTime().UTC().Format(time.RFC3339),
					ModelProvider: meta.ModelProvider,
					Archived:      strings.Contains(filepath.ToSlash(rel), "archived_sessions/"),
					Compressed:    compressed,
					SizeBytes:     info.Size(),
				},
				source:     p,
				compressed: compressed,
			})
			return nil
		})
	}
	sortCandidates(out)
	return out, warns, nil
}

type codexMetaInfo struct {
	ID            string
	Cwd           string
	Timestamp     string
	ModelProvider string
}

func codexMeta(p string, compressed bool) (codexMetaInfo, error) {
	var r io.Reader
	var closer io.Closer
	if compressed {
		if !zstdAvailable() {
			return codexMetaInfo{}, ErrZstdMissing
		}
		rc, err := zstdReader(p)
		if err != nil {
			return codexMetaInfo{}, err
		}
		r, closer = rc, rc
	} else {
		f, err := os.Open(p)
		if err != nil {
			return codexMetaInfo{}, err
		}
		r, closer = f, f
	}
	defer closer.Close()
	meta, ok := usage.CodexMeta(io.LimitReader(r, 8<<20))
	if !ok {
		return codexMetaInfo{}, fmt.Errorf("no session_meta record")
	}
	return codexMetaInfo{ID: meta.ID, Cwd: meta.Cwd, ModelProvider: meta.ModelProvider}, nil
}

func codexIDFromName(name string) string {
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".zst"), ".jsonl")
	if i := strings.LastIndex(name, "-"); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return ""
}

func sortCandidates(c []candidate) {
	sort.SliceStable(c, func(i, j int) bool {
		return c[i].session.UpdatedAt > c[j].session.UpdatedAt
	})
}

func matchesAnyID(want []string, id string) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		if w != "" && strings.HasPrefix(id, w) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	return runtimeIsCaseInsensitive() && strings.EqualFold(a, b)
}

// ErrEncrypted is returned when a bundle is age-encrypted and no identity was supplied.
var ErrEncrypted = errors.New("bundle is encrypted; supply an age identity (--identity) or passphrase")
