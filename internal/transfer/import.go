package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Action is what an import did (or, with DryRun, would do) to one session.
type Action string

const (
	ActionImport         Action = "import"
	ActionUpdate         Action = "update"
	ActionReplace        Action = "replace"
	ActionImportCopy     Action = "import-as-copy"
	ActionSkipIdentical  Action = "skip-identical"
	ActionSkipAhead      Action = "skip-ahead"
	ActionSkipDeselected Action = "skip-deselected"
	ActionConflict       Action = "conflict"
)

// Mapping rewrites one recorded working directory on import.
type Mapping struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// ImportOptions controls what is written and how conflicts are resolved.
type ImportOptions struct {
	// Tool overrides the bundle's recorded tool (it should not usually be needed).
	Tool       string
	ClaudeHome string
	CodexHome  string

	// Project only warns when the bundle was recorded somewhere else.
	Project string

	// Sessions keeps only these ids (unique prefixes are enough).
	Sessions []string

	// Merge appends the new messages when the local copy is a byte prefix of the bundle's
	// (lossless and idempotent). Without it, a grown session is a conflict.
	Merge bool
	// ReplaceWithBackup overwrites a diverged local file, keeping a timestamped backup.
	ReplaceWithBackup bool
	// ImportAsCopy writes the bundle's diverged version beside the local one instead.
	ImportAsCopy bool

	// MapCwd rewrites recorded working directories. MapCwdHere maps the bundle's single
	// project to Here.
	MapCwd     []Mapping
	MapCwdHere bool
	Here       string

	// DryRun validates and reports without writing anything.
	DryRun bool
	// Context carries cancellation for tools that shell out (OpenCode). Nil means Background.
	Context context.Context
}

// Item is the per-session outcome.
type Item struct {
	ThreadID   string `json:"thread_id"`
	BundlePath string `json:"bundle_path"`
	DestPath   string `json:"dest_path,omitempty"`
	Action     Action `json:"action"`
	LinesAdded int    `json:"lines_added,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	Note       string `json:"note,omitempty"`
}

// ImportResult is the whole plan or outcome.
type ImportResult struct {
	Tool      string   `json:"tool"`
	Bundle    string   `json:"bundle"`
	DryRun    bool     `json:"dry_run"`
	Items     []Item   `json:"items"`
	Warnings  []string `json:"warnings,omitempty"`
	Conflicts int      `json:"conflicts"`
	Imported  int      `json:"imported"`
	Updated   int      `json:"updated"`
	Skipped   int      `json:"skipped"`
	Bytes     int64    `json:"bytes"`
}

// Import writes a bundle's sessions into the target harness home. It verifies the bundle
// before it starts (Open already did) and never overwrites a differing local file without
// an explicit policy. The harness's own index is never touched: the agent re-discovers the
// files on its next start.
func Import(b *Bundle, opts ImportOptions) (ImportResult, error) {
	res := ImportResult{Bundle: b.Path, DryRun: opts.DryRun}
	tool := normTool(opts.Tool)
	if tool == "" {
		tool = normTool(b.Manifest.Tool)
	}
	res.Tool = tool
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if tool == ToolOpenCode {
		return importOpenCodeBundle(ctx, b, opts)
	}
	claudeHome, err := ClaudeHome(opts.ClaudeHome)
	if err != nil {
		return res, err
	}
	codexHome, err := CodexHome(opts.CodexHome)
	if err != nil {
		return res, err
	}
	home := claudeHome
	if tool == ToolCodex {
		home = codexHome
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return res, err
	}
	if tool == "" {
		return res, fmt.Errorf("bundle names no tool")
	}

	mappings, err := resolveMappings(opts)
	if err != nil {
		return res, err
	}
	if err := finishMappings(mappings, b.Manifest.Sessions); err != nil {
		return res, err
	}
	if opts.MapCwdHere && opts.Project == "" {
		opts.Project = opts.Here
	}
	if opts.Project != "" && !samePath(b.Manifest.SourceProjectPath, opts.Project) {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("bundle was recorded in %s, not %s; use --map-cwd to rewrite it if the project moved",
				orDash(b.Manifest.SourceProjectPath), opts.Project))
	}

	for _, s := range b.Manifest.Sessions {
		if !matchesAnyID(opts.Sessions, s.ThreadID) {
			res.Items = append(res.Items, Item{ThreadID: s.ThreadID, BundlePath: s.BundlePath, Action: ActionSkipDeselected})
			res.Skipped++
			continue
		}
		item, err := planOne(b, s, tool, home, mappings, opts)
		if err != nil {
			return res, err
		}
		if item.Action == ActionConflict {
			res.Conflicts++
		}
		switch item.Action {
		case ActionImport, ActionImportCopy:
			res.Imported++
		case ActionUpdate, ActionReplace:
			res.Updated++
		case ActionConflict, ActionSkipAhead, ActionSkipDeselected, ActionSkipIdentical:
			res.Skipped++
		}
		res.Bytes += item.Bytes
		res.Items = append(res.Items, item)
	}
	if res.Items == nil {
		res.Items = []Item{}
	}
	return res, nil
}

func planOne(b *Bundle, s Session, tool, home string, mappings map[string]string, opts ImportOptions) (Item, error) {
	item := Item{ThreadID: s.ThreadID, BundlePath: s.BundlePath}
	data, err := b.Read(s.BundlePath)
	if err != nil {
		return item, err
	}
	if s.SHA256 != "" && !strings.EqualFold(sha256Hex(data), s.SHA256) {
		return item, fmt.Errorf("%s: contents do not match the manifest (bundle is corrupt)", s.BundlePath)
	}
	item.Bytes = int64(len(data))

	compressed := strings.HasSuffix(s.BundlePath, ".zst")
	newCwd, mapped := mappings[filepath.Clean(s.OriginalCWD)]
	if mapped {
		if compressed {
			item.Action = ActionConflict
			item.Note = "compressed session cannot be re-homed without zstd recompression; skipped (conflict)"
			return item, nil
		}
		data = rewriteCwd(tool, data, s.OriginalCWD, newCwd)
	}

	destRel := s.BundlePath
	if mapped && tool == ToolClaude {
		destRel = filepath.ToSlash(filepath.Join("projects", claudeSlug(newCwd), path.Base(destRel)))
	}
	if !SafeRel(destRel) {
		return item, fmt.Errorf("%s: unsafe destination %q", s.BundlePath, destRel)
	}
	item.DestPath = filepath.Join(home, filepath.FromSlash(destRel))

	local, exists, err := readInside(home, destRel)
	if err != nil {
		return item, err
	}
	switch {
	case !exists:
		item.Action = ActionImport
		return item, writeItem(home, destRel, data, opts.DryRun)
	case bytes.Equal(local, data):
		item.Action = ActionSkipIdentical
		return item, nil
	}

	// Append-only logs: a byte prefix means one side only grew. Compressed sessions are
	// compared in plaintext when possible, but can only be replaced, never appended to.
	bundlePlain, localPlain, comparable := comparableSides(data, local, compressed)
	switch {
	case comparable && bytes.HasPrefix(bundlePlain, localPlain):
		if opts.Merge {
			item.Action = ActionUpdate
			item.LinesAdded = countNewLines(localPlain, bundlePlain)
			item.Note = "appended the new messages"
			if opts.DryRun {
				return item, nil
			}
			if compressed {
				item.Note = "replaced with the longer version (compressed sessions cannot be appended to)"
				return item, writeItem(home, destRel, data, false)
			}
			return item, appendInside(home, destRel, bundlePlain[len(localPlain):])
		}
		item.Action = ActionConflict
		item.Note = "local copy is behind the bundle; re-run with --merge to append the new messages"
		return item, nil
	case comparable && bytes.HasPrefix(localPlain, bundlePlain):
		item.Action = ActionSkipAhead
		item.Note = "local copy is already ahead of the bundle"
		return item, nil
	}

	// Genuinely diverged (or not comparable without zstd).
	switch {
	case opts.ReplaceWithBackup:
		item.Action = ActionReplace
		item.Note = "local file backed up and replaced"
		if opts.DryRun {
			return item, nil
		}
		backup := destRel + ".bak-" + time.Now().UTC().Format("20060102T150405Z")
		if err := writeInside(home, backup, local, 0o600, 0o700); err != nil {
			return item, err
		}
		item.Note = "local file backed up to " + filepath.Join(home, filepath.FromSlash(backup))
		return item, writeItem(home, destRel, data, false)
	case opts.ImportAsCopy:
		return importAsCopy(item, home, destRel, tool, s, data, compressed, opts.DryRun)
	default:
		item.Action = ActionConflict
		item.Note = "local file differs; use --merge (if it only grew), --replace-with-backup, or --import-as-copy"
		return item, nil
	}
}

// comparableSides returns plaintext for both sides when they can be compared, so a
// compressed session that grew elsewhere is still recognised as append-only.
func comparableSides(bundleData, local []byte, compressed bool) (bundlePlain, localPlain []byte, ok bool) {
	if !compressed {
		return bundleData, local, true
	}
	if !zstdAvailable() {
		return nil, nil, false
	}
	bp, err := decompressZstdBytes(bundleData)
	if err != nil {
		return nil, nil, false
	}
	lp, err := decompressZstdBytes(local)
	if err != nil {
		return nil, nil, false
	}
	return bp, lp, true
}

func importAsCopy(item Item, home, destRel, tool string, s Session, data []byte, compressed bool, dryRun bool) (Item, error) {
	item.Action = ActionImportCopy
	if compressed {
		item.Note = "compressed session cannot be given a new id without recompression; skipped"
		return item, nil
	}
	newID := s.ThreadID + "-copy-" + randomHex(3)
	base := path.Base(destRel)
	switch tool {
	case ToolClaude:
		base = newID + ".jsonl"
		data = rewriteSessionID(tool, data, s.ThreadID, newID)
	case ToolCodex:
		base = strings.TrimSuffix(base, path.Ext(base)) + "-copy-" + randomHex(3) + ".jsonl"
		data = rewriteSessionID(tool, data, s.ThreadID, newID)
	}
	copyRel := path.Join(path.Dir(destRel), base)
	item.DestPath = filepath.Join(home, filepath.FromSlash(copyRel))
	item.Note = "imported beside the local copy as " + copyRel
	return item, writeItem(home, copyRel, data, dryRun)
}

// resolveMappings validates --map-cwd pairs. A --map-cwd-here request leaves a placeholder
// under the empty key, resolved by finishMappings once the bundle's project is known.
func resolveMappings(opts ImportOptions) (map[string]string, error) {
	m := make(map[string]string)
	for _, mp := range opts.MapCwd {
		old := strings.TrimSpace(mp.Old)
		newPath := strings.TrimSpace(mp.New)
		if old == "" || newPath == "" {
			return nil, fmt.Errorf("--map-cwd needs OLD=NEW, got %q=%q", mp.Old, mp.New)
		}
		absNew, err := filepath.Abs(newPath)
		if err != nil {
			return nil, err
		}
		m[filepath.Clean(old)] = absNew
	}
	if opts.MapCwdHere {
		if len(m) > 0 {
			return nil, errors.New("--map-cwd-here cannot be combined with --map-cwd")
		}
		here := opts.Here
		if here == "" {
			wd, err := os.Getwd()
			if err != nil {
				return nil, err
			}
			here = wd
		}
		abs, err := filepath.Abs(here)
		if err != nil {
			return nil, err
		}
		m[""] = abs
	}
	return m, nil
}

// finishMappings resolves the map-cwd-here placeholder once the bundle is known: the
// bundle must describe exactly one project, or the caller must say which.
func finishMappings(m map[string]string, sessions []Session) error {
	here, ok := m[""]
	if !ok {
		return nil
	}
	delete(m, "")
	var distinct []string
	seen := map[string]bool{}
	for _, s := range sessions {
		c := filepath.Clean(s.OriginalCWD)
		if c == "." || c == "" || seen[c] {
			continue
		}
		seen[c] = true
		distinct = append(distinct, c)
	}
	switch len(distinct) {
	case 0:
		return errors.New("bundle records no working directory to map")
	case 1:
		m[distinct[0]] = here
		return nil
	default:
		return fmt.Errorf("bundle spans %d projects; use --map-cwd OLD=NEW for each instead of --map-cwd-here", len(distinct))
	}
}

func writeItem(home, rel string, data []byte, dryRun bool) error {
	if dryRun {
		return nil
	}
	return writeInside(home, rel, data, 0o600, 0o700)
}

func countNewLines(before, after []byte) int {
	delta := after[len(before):]
	n := bytes.Count(delta, []byte("\n"))
	if len(delta) > 0 && delta[len(delta)-1] != '\n' {
		n++
	}
	return n
}

func rewriteCwd(tool string, data []byte, from, to string) []byte {
	if from == "" || to == "" || from == to {
		return data
	}
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		changed := false
		switch tool {
		case ToolClaude:
			if v, ok := rec["cwd"].(string); ok && samePath(v, from) {
				rec["cwd"] = to
				changed = true
			}
		case ToolCodex:
			if p, ok := rec["payload"].(map[string]any); ok {
				if v, ok := p["cwd"].(string); ok && samePath(v, from) {
					p["cwd"] = to
					changed = true
				}
			}
		}
		if !changed {
			continue
		}
		if out, err := json.Marshal(rec); err == nil {
			lines[i] = out
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

func rewriteSessionID(tool string, data []byte, from, to string) []byte {
	if from == "" || to == "" {
		return data
	}
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		changed := false
		switch tool {
		case ToolClaude:
			if v, ok := rec["sessionId"].(string); ok && v == from {
				rec["sessionId"] = to
				changed = true
			}
		case ToolCodex:
			if p, ok := rec["payload"].(map[string]any); ok {
				if v, ok := p["id"].(string); ok && v == from {
					p["id"] = to
					changed = true
				}
			}
		}
		if changed {
			if out, err := json.Marshal(rec); err == nil {
				lines[i] = out
			}
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// claudeSlug mirrors Claude Code's project directory naming: the working directory with
// every non-alphanumeric character replaced by '-'.
func claudeSlug(cwd string) string {
	cwd = filepath.Clean(cwd)
	var b strings.Builder
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// importOpenCodeBundle loads OpenCode exports through the CLI. OpenCode owns its store, so
// there is no file to classify here: the import is idempotent when the session id already
// exists, and that is reported as an identical skip.
func importOpenCodeBundle(ctx context.Context, b *Bundle, opts ImportOptions) (ImportResult, error) {
	res := ImportResult{Tool: ToolOpenCode, Bundle: b.Path, DryRun: opts.DryRun}
	if !opencodeAvailable() {
		return res, errors.New("the opencode CLI is not installed")
	}
	baseDir := opts.Project
	if baseDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return res, err
		}
		baseDir = wd
	}
	mappings, err := resolveMappings(opts)
	if err != nil {
		return res, err
	}
	if err := finishMappings(mappings, b.Manifest.Sessions); err != nil {
		return res, err
	}
	for _, s := range b.Manifest.Sessions {
		if !matchesAnyID(opts.Sessions, s.ThreadID) {
			res.Items = append(res.Items, Item{ThreadID: s.ThreadID, BundlePath: s.BundlePath, Action: ActionSkipDeselected})
			res.Skipped++
			continue
		}
		data, err := b.Read(s.BundlePath)
		if err != nil {
			return res, err
		}
		cwd := s.OriginalCWD
		if to, ok := mappings[filepath.Clean(cwd)]; ok {
			data = rewriteOpenCodeCwd(data, cwd, to)
			cwd = to
		}
		if cwd == "" {
			cwd = baseDir
		}
		item := Item{ThreadID: s.ThreadID, BundlePath: s.BundlePath, Bytes: int64(len(data))}
		if opts.DryRun {
			item.Action = ActionImport
			item.DestPath = "opencode:" + s.ThreadID + " (" + cwd + ")"
			res.Items = append(res.Items, item)
			res.Imported++
			continue
		}
		got, err := importOpenCode(ctx, cwd, data)
		if errors.Is(err, errAlreadyExists) {
			item.Action = ActionSkipIdentical
			res.Skipped++
			res.Items = append(res.Items, item)
			continue
		}
		if err != nil {
			return res, err
		}
		item.Action = ActionImport
		item.DestPath = "opencode:" + got + " (" + cwd + ")"
		res.Imported++
		res.Items = append(res.Items, item)
	}
	if res.Items == nil {
		res.Items = []Item{}
	}
	return res, nil
}

// rewriteOpenCodeCwd re-homes an export document without touching its messages.
func rewriteOpenCodeCwd(data []byte, from, to string) []byte {
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return data
	}
	info, _ := doc["info"].(map[string]any)
	if info == nil {
		return data
	}
	if v, ok := info["directory"].(string); ok && samePath(v, from) {
		info["directory"] = to
	}
	if loc, ok := info["location"].(map[string]any); ok {
		if v, ok := loc["directory"].(string); ok && samePath(v, from) {
			loc["directory"] = to
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return data
	}
	return out
}
