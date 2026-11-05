// Package transfer moves coding-agent sessions between machines as one file.
//
// It is deliberately the thin, multi-session counterpart to internal/checkpoint. A
// checkpoint is one conversation plus its working tree, distilled for a resume; a transfer
// bundle is many conversations from one project, carried verbatim, for a machine that
// already has the code (or will get it with git). Neither ever touches the control plane:
// bundles are written by the CLI, on the user's machine, and leave it only as a file the
// user moves or sends over their own local network.
//
// The wire format is the one cct (github.com/ahmojo/codex-claude-transfer) writes, so a
// bundle made here imports there and the reverse. It is a ZIP:
//
//	manifest.json          format version, tool, source, per-session metadata
//	checksums.json         sha256 of every other member
//	projects/<slug>/<id>.jsonl      Claude Code transcripts (paths kept verbatim)
//	sessions/YYYY/MM/DD/rollout-*.jsonl[.zst]   Codex rollouts (paths kept verbatim)
//
// Keep the copy in docs/PORTABILITY.md in step with this file.
package transfer

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"time"
)

// FormatVersion is the bundle layout this build reads and writes. It matches cct's, so
// bundles are interchangeable; a change here is a breaking format change.
const FormatVersion = "codex-sync-bundle-v1"

// Fixed member names at the root of the archive.
const (
	ManifestName  = "manifest.json"
	ChecksumsName = "checksums.json"
)

// Resource limits, enforced before untrusted bundle content is trusted or written. They
// bound a decompression bomb: the declared size is checked first, and the actual read is
// capped too, because a ZIP header can understate it.
const (
	MaxSessionBytes       = 100 << 20 // a single session entry
	HardMaxSessionBytes   = 256 << 20 // an explicitly raised per-import limit
	MaxMetadataBytes      = 16 << 20  // manifest.json / checksums.json
	MaxBundleEntries      = 100_000
	MaxBundleUncompressed = 2 << 30
)

// Tools a bundle can carry. The names match cct's manifest values.
const (
	ToolClaude = "claude"
	ToolCodex  = "codex"
)

// Manifest describes a bundle. Field names and types follow cct's manifest so the two
// tools read each other's bundles; Conductor-specific data rides in Conductor, which an
// older cct ignores.
type Manifest struct {
	FormatVersion     string    `json:"format_version"`
	Tool              string    `json:"tool,omitempty"`
	CreatedAt         string    `json:"created_at"`
	CreatedByDevice   string    `json:"created_by_device,omitempty"`
	SourceOS          string    `json:"source_os"`
	SourceCodexHome   string    `json:"source_codex_home"`
	SourceProjectPath string    `json:"source_project_path,omitempty"`
	Sessions          []Session `json:"sessions"`
	Conductor         *Meta     `json:"conductor,omitempty"`
}

// Meta is Conductor's extension to the shared manifest. It never contains conversation
// text: only counts, ids, and the policy that was applied.
type Meta struct {
	Schema       int    `json:"schema"`
	BundleID     string `json:"bundle_id"`
	Project      string `json:"project,omitempty"`
	Redacted     int    `json:"redacted,omitempty"`
	SecretsFound int    `json:"secrets_found,omitempty"`
	Source       string `json:"source,omitempty"` // export, handoff, sync
}

// Session is one conversation in the bundle. OriginalPath is where it lived (absolute, on
// the exporting machine); BundlePath is its forward-slash path inside the archive, always
// relative to the harness home, exactly as the harness stores it.
type Session struct {
	ThreadID         string `json:"thread_id,omitempty"`
	OriginalPath     string `json:"original_path"`
	BundlePath       string `json:"bundle_path"`
	OriginalCWD      string `json:"original_cwd,omitempty"`
	Preview          string `json:"preview,omitempty"`
	FirstUserMessage string `json:"first_user_message,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
	UpdatedAt        string `json:"updated_at,omitempty"`
	Source           string `json:"source,omitempty"`
	ModelProvider    string `json:"model_provider,omitempty"`
	GitBranch        string `json:"git_branch,omitempty"`
	GitSHA           string `json:"git_sha,omitempty"`
	Archived         bool   `json:"archived"`
	Compressed       bool   `json:"compressed"`
	SizeBytes        int64  `json:"size_bytes"`
	SHA256           string `json:"sha256"`
}

// Checksums maps bundle-relative paths to their SHA-256 hex digest. It covers every member
// except checksums.json itself (which would otherwise have to hash itself).
type Checksums map[string]string

// Bundle is an opened, verified bundle. Entries are read on demand, so opening a large
// bundle does not load every transcript into memory.
type Bundle struct {
	Path      string
	Manifest  Manifest
	Checksums Checksums

	zr     *zip.Reader
	closer io.Closer
	mem    map[string][]byte // in-memory bundles (a sync exchange) need no file handle
}

// Open verifies and opens a bundle at p. Every member declared in checksums.json must be
// present with a matching digest, and no undeclared member is allowed; a corrupt or
// tampered bundle opens with an error and changes nothing.
func Open(p string) (*Bundle, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	b, err := openReaderAt(f, st.Size())
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	b.Path = p
	b.closer = f
	return b, nil
}

// OpenBytes opens a bundle already in memory, which is how a sync exchange hands one over.
func OpenBytes(data []byte) (*Bundle, error) {
	return openReaderAt(bytes.NewReader(data), int64(len(data)))
}

func openReaderAt(r io.ReaderAt, size int64) (*Bundle, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("not a bundle (bad ZIP): %w", err)
	}
	if len(zr.File) > MaxBundleEntries {
		return nil, fmt.Errorf("bundle has %d entries, over the %d limit", len(zr.File), MaxBundleEntries)
	}
	b := &Bundle{zr: zr}
	if err := b.loadMetadata(); err != nil {
		return nil, err
	}
	if err := b.verify(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Bundle) loadMetadata() error {
	mf, err := b.entry(ManifestName, MaxMetadataBytes)
	if err != nil {
		return fmt.Errorf("bundle has no readable %s: %w", ManifestName, err)
	}
	cf, err := b.entry(ChecksumsName, MaxMetadataBytes)
	if err != nil {
		return fmt.Errorf("bundle has no readable %s: %w", ChecksumsName, err)
	}
	if err := json.Unmarshal(mf, &b.Manifest); err != nil {
		return fmt.Errorf("%s: %w", ManifestName, err)
	}
	if err := json.Unmarshal(cf, &b.Checksums); err != nil {
		return fmt.Errorf("%s: %w", ChecksumsName, err)
	}
	return b.Manifest.Validate()
}

func (b *Bundle) entry(name string, max int64) ([]byte, error) {
	f, err := b.file(name)
	if err != nil {
		return nil, err
	}
	if f.UncompressedSize64 > uint64(max) {
		return nil, fmt.Errorf("%s declares %d bytes, over the %d-byte limit", name, f.UncompressedSize64, max)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readCapped(rc, max, name)
}

func (b *Bundle) file(name string) (*zip.File, error) {
	if !SafeRel(name) {
		return nil, fmt.Errorf("%q is not a safe bundle path", name)
	}
	for _, f := range b.zr.File {
		if f.Name == name {
			return f, nil
		}
	}
	return nil, fmt.Errorf("%s: not in bundle", name)
}

func (b *Bundle) verify() error {
	seen := make(map[string]bool, len(b.zr.File))
	var total int64
	for _, f := range b.zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		seen[f.Name] = true
		if f.UncompressedSize64 > MaxBundleUncompressed {
			return fmt.Errorf("%s declares %d bytes, over the bundle limit", f.Name, f.UncompressedSize64)
		}
		total += int64(f.UncompressedSize64)
		if total > MaxBundleUncompressed {
			return fmt.Errorf("bundle expands to over %d bytes", int64(MaxBundleUncompressed))
		}
		if f.Name == ChecksumsName {
			continue
		}
		want, ok := b.Checksums[f.Name]
		if !ok {
			return fmt.Errorf("%s is not covered by %s", f.Name, ChecksumsName)
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(rc, int64(f.UncompressedSize64)+1))
		rc.Close()
		if err != nil {
			return err
		}
		if n != int64(f.UncompressedSize64) {
			return fmt.Errorf("%s: size does not match its ZIP header", f.Name)
		}
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
			return fmt.Errorf("%s: checksum mismatch (bundle is corrupt or was modified)", f.Name)
		}
	}
	for name := range b.Checksums {
		if name != ChecksumsName && !seen[name] {
			return fmt.Errorf("%s is listed in %s but missing from the bundle", name, ChecksumsName)
		}
	}
	for _, s := range b.Manifest.Sessions {
		sum, ok := b.Checksums[s.BundlePath]
		if !ok {
			return fmt.Errorf("session %s is not covered by %s", s.BundlePath, ChecksumsName)
		}
		if s.SHA256 != "" && !strings.EqualFold(s.SHA256, sum) {
			return fmt.Errorf("session %s: manifest checksum disagrees with %s", s.BundlePath, ChecksumsName)
		}
	}
	return nil
}

// Entries lists every member, sorted, for inspect.
func (b *Bundle) Entries() []string {
	out := make([]string, 0, len(b.zr.File))
	for _, f := range b.zr.File {
		if !f.FileInfo().IsDir() {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Read returns one member, capped at the session limit. It is for metadata-sized reads and
// individual sessions; imports stream through ReadStream instead.
func (b *Bundle) Read(name string) ([]byte, error) {
	if b.mem != nil {
		data, ok := b.mem[name]
		if !ok {
			return nil, fmt.Errorf("%s: not in bundle", name)
		}
		return data, nil
	}
	f, err := b.file(name)
	if err != nil {
		return nil, err
	}
	if f.UncompressedSize64 > HardMaxSessionBytes {
		return nil, fmt.Errorf("%s declares %d bytes, over the %d-byte limit", name, f.UncompressedSize64, HardMaxSessionBytes)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readCapped(rc, HardMaxSessionBytes, name)
}

// ReadStream streams one member without buffering it whole.
func (b *Bundle) ReadStream(name string) (io.ReadCloser, error) {
	if b.mem != nil {
		data, ok := b.mem[name]
		if !ok {
			return nil, fmt.Errorf("%s: not in bundle", name)
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	f, err := b.file(name)
	if err != nil {
		return nil, err
	}
	return f.Open()
}

// Close releases the underlying file, when there is one.
func (b *Bundle) Close() error {
	if b.closer != nil {
		return b.closer.Close()
	}
	return nil
}

// Validate checks the invariants a manifest must hold before any path or id in it is acted
// on. A bundle may arrive from anywhere, so ids become file names and arguments only after
// passing here.
func (m Manifest) Validate() error {
	switch {
	case m.FormatVersion != FormatVersion:
		return fmt.Errorf("bundle format %q is not supported by this build (wants %q)", m.FormatVersion, FormatVersion)
	case normTool(m.Tool) == "":
		return fmt.Errorf("bundle names tool %q; expected %q or %q", m.Tool, ToolClaude, ToolCodex)
	case m.CreatedAt == "":
		return fmt.Errorf("bundle has no creation time")
	}
	seen := make(map[string]bool, len(m.Sessions))
	for i, s := range m.Sessions {
		switch {
		case s.BundlePath == "":
			return fmt.Errorf("session %d has no bundle path", i)
		case !SafeRel(s.BundlePath):
			return fmt.Errorf("session %d path %q leaves the bundle", i, s.BundlePath)
		case seen[s.BundlePath]:
			return fmt.Errorf("session path %q appears twice", s.BundlePath)
		}
		seen[s.BundlePath] = true
		if s.ThreadID != "" && !SafeIdentifier(s.ThreadID) {
			return fmt.Errorf("session %d names id %q, which is not a plain identifier", i, s.ThreadID)
		}
		if s.OriginalCWD != "" && !isAbsPath(s.OriginalCWD) {
			return fmt.Errorf("session %d records a working directory %q that is not absolute", i, s.OriginalCWD)
		}
	}
	return nil
}

// normTool canonicalises a tool name, returning "" for anything unknown.
func normTool(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "auto":
		return ""
	case ToolClaude, "claude-code", "claudecode":
		return ToolClaude
	case ToolCodex:
		return ToolCodex
	default:
		return ""
	}
}

// SafeIdentifier reports whether s is a plain identifier: letters, digits, '.', '_' and '-',
// starting with a letter or digit, at most 200 bytes, never containing "..". Session ids
// end up as file names and as arguments to the harness's resume command.
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

// SafeRel reports whether p is a relative, forward-slash path that stays inside the
// directory it is joined to: no absolute path, no "..", no backslash, no NUL, no empty
// component.
func SafeRel(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") || path.IsAbs(p) {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func isAbsPath(p string) bool {
	return strings.HasPrefix(p, "/") || (len(p) > 2 && p[1] == ':' && (p[2] == '\\' || p[2] == '/'))
}

func readCapped(r io.Reader, max int64, what string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit (possible decompression bomb)", what, max)
	}
	return data, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// writing
// ---------------------------------------------------------------------------

// Writer builds a bundle directly onto an io.Writer, streaming each session instead of
// holding it in memory. Sessions are added first; Close writes manifest.json and
// checksums.json, which need the finished session list and digests.
type Writer struct {
	zw     *zip.Writer
	sums   Checksums
	closed bool

	Tool          string
	Home          string
	ProjectPath   string
	Sessions      []Session
	Device        string
	Source        string
	Redacted      int
	SecretsFound  int
	CreatedAt     time.Time
	extraSessions int
}

// NewWriter starts a bundle for one tool. home is the harness state directory the paths in
// the archive are relative to.
func NewWriter(w io.Writer, tool, home, projectPath string) *Writer {
	return &Writer{
		zw:          zip.NewWriter(w),
		sums:        Checksums{},
		Tool:        normTool(tool),
		Home:        home,
		ProjectPath: projectPath,
		Device:      deviceName(),
		CreatedAt:   time.Now().UTC(),
	}
}

// AddSession streams r into the archive at s.BundlePath, filling in size and digest. The
// returned Session is the manifest entry to keep.
func (w *Writer) AddSession(s Session, r io.Reader) (Session, error) {
	if !SafeRel(s.BundlePath) {
		return s, fmt.Errorf("refusing to add session at unsafe path %q", s.BundlePath)
	}
	zf, err := w.zw.CreateHeader(&zip.FileHeader{Name: s.BundlePath, Method: zip.Deflate, Modified: w.CreatedAt})
	if err != nil {
		return s, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(zf, h), r)
	if err != nil {
		return s, fmt.Errorf("%s: %w", s.BundlePath, err)
	}
	s.SizeBytes = n
	s.SHA256 = hex.EncodeToString(h.Sum(nil))
	s.Compressed = strings.HasSuffix(s.BundlePath, ".zst")
	w.sums[s.BundlePath] = s.SHA256
	w.Sessions = append(w.Sessions, s)
	return s, nil
}

// AddExtra adds a supporting file (a Claude session's directory of subagent transcripts and
// spilled tool results) under the same relative layout.
func (w *Writer) AddExtra(p string, r io.Reader) error {
	if !SafeRel(p) {
		return fmt.Errorf("refusing to add %q: unsafe path", p)
	}
	zf, err := w.zw.CreateHeader(&zip.FileHeader{Name: p, Method: zip.Deflate, Modified: w.CreatedAt})
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(zf, h), r); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	w.sums[p] = hex.EncodeToString(h.Sum(nil))
	w.extraSessions++
	return nil
}

// Has reports whether a path has already been added, so two selectors cannot duplicate it.
func (w *Writer) Has(p string) bool {
	_, ok := w.sums[p]
	return ok
}

// Close finishes the archive. It is idempotent.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	m := Manifest{
		FormatVersion:     FormatVersion,
		Tool:              w.Tool,
		CreatedAt:         w.CreatedAt.Format(time.RFC3339),
		CreatedByDevice:   w.Device,
		SourceOS:          runtime.GOOS,
		SourceCodexHome:   w.Home,
		SourceProjectPath: w.ProjectPath,
		Sessions:          w.Sessions,
		Conductor: &Meta{
			Schema:       1,
			BundleID:     bundleID(w.CreatedAt),
			Project:      w.ProjectPath,
			Redacted:     w.Redacted,
			SecretsFound: w.SecretsFound,
			Source:       w.Source,
		},
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	w.sums[ManifestName] = sha256Hex(mb)
	cb, err := json.MarshalIndent(w.sums, "", "  ")
	if err != nil {
		return err
	}
	for _, entry := range []struct {
		name string
		data []byte
	}{{ManifestName, mb}, {ChecksumsName, cb}} {
		zf, err := w.zw.CreateHeader(&zip.FileHeader{Name: entry.name, Method: zip.Deflate, Modified: w.CreatedAt})
		if err != nil {
			return err
		}
		if _, err := zf.Write(entry.data); err != nil {
			return err
		}
	}
	return w.zw.Close()
}

func bundleID(now time.Time) string {
	return "xb-" + now.UTC().Format("20060102T150405Z")
}

func deviceName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// ErrNothingSelected means the export filters matched no session. Callers turn it into a
// clear message rather than writing an empty bundle.
var ErrNothingSelected = errors.New("no sessions match the selection")

// MissingFieldError names a bundle field this build cannot work without.
func MissingFieldError(field string) error {
	return fmt.Errorf("bundle is missing %s", field)
}
