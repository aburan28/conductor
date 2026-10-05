package checkpoint

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// A bundle is a gzip-compressed tar archive whose first member is manifest.json. The tar
// form was chosen over a directory because the point of a checkpoint is to be moved: one
// file copies with scp, uploads to a bucket, or attaches to a message. Members are plain
// files with forward-slash paths; the manifest's Files list carries the sha256 of each so a
// bundle that arrived through an untrusted channel can be verified before use.

// Builder accumulates members in memory, then writes the archive once with the manifest
// first. Checkpoints are transcript-sized (kilobytes to tens of megabytes), so holding the
// members in memory is simpler than streaming and lets the hashes be computed before the
// manifest is sealed.
type Builder struct {
	members []member
}

type member struct {
	path string
	data []byte
	mode int64
}

// Add stages one member. Paths are cleaned and must be relative.
func (b *Builder) Add(p string, data []byte) error {
	p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if p == "." || p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") || p == ".." {
		return fmt.Errorf("bundle member %q: path must be relative and inside the bundle", p)
	}
	if p == ManifestPath {
		return errors.New("the manifest is written by Seal, not added as a member")
	}
	for _, m := range b.members {
		if m.path == p {
			return fmt.Errorf("bundle member %q added twice", p)
		}
	}
	b.members = append(b.members, member{path: p, data: data, mode: 0o600})
	return nil
}

// Has reports whether a member is staged.
func (b *Builder) Has(p string) bool {
	for _, m := range b.members {
		if m.path == p {
			return true
		}
	}
	return false
}

// Seal fills the manifest's Files list from the staged members and writes the archive.
func (b *Builder) Seal(m *Manifest, w io.Writer) error {
	sort.Slice(b.members, func(i, j int) bool { return b.members[i].path < b.members[j].path })
	m.Files = m.Files[:0]
	for _, mem := range b.members {
		sum := sha256.Sum256(mem.data)
		m.Files = append(m.Files, FileEntry{Path: mem.path, Size: int64(len(mem.data)), SHA256: hex.EncodeToString(sum[:])})
	}
	if err := m.Validate(); err != nil {
		return err
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	write := func(p string, data []byte, mode int64) error {
		hdr := &tar.Header{Name: p, Mode: mode, Size: int64(len(data)), ModTime: m.CreatedAt, Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := write(ManifestPath, manifest, 0o600); err != nil {
		return err
	}
	for _, mem := range b.members {
		if err := write(mem.path, mem.data, mem.mode); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// Bundle is an opened archive.
type Bundle struct {
	Manifest Manifest
	files    map[string][]byte
}

// ReadManifest decodes only the manifest of an archive, which is its first member, without
// reading the rest. It is what listing uses on bundles that have no sidecar manifest.
func ReadManifest(r io.Reader) (Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Manifest{}, fmt.Errorf("not a checkpoint bundle: %w", err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		return Manifest{}, fmt.Errorf("not a checkpoint bundle: %w", err)
	}
	if hdr.Name != ManifestPath {
		return Manifest{}, fmt.Errorf("not a checkpoint bundle: first member is %q, not %s", hdr.Name, ManifestPath)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 16<<20)).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("checkpoint manifest: %w", err)
	}
	return m, m.Validate()
}

// Open reads a whole archive and verifies every member against the manifest. A member the
// manifest does not list, a listed member that is missing, or a hash that does not match
// is an error: a bundle is trusted entirely or not at all.
func Open(r io.Reader) (*Bundle, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("not a checkpoint bundle: %w", err)
	}
	tr := tar.NewReader(gz)
	b := &Bundle{files: map[string][]byte{}}
	var haveManifest bool
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("checkpoint bundle: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean(hdr.Name)
		if strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
			return nil, fmt.Errorf("checkpoint bundle: member %q escapes the bundle", hdr.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("checkpoint bundle: reading %s: %w", name, err)
		}
		if name == ManifestPath {
			if err := json.Unmarshal(data, &b.Manifest); err != nil {
				return nil, fmt.Errorf("checkpoint manifest: %w", err)
			}
			haveManifest = true
			continue
		}
		b.files[name] = data
	}
	if !haveManifest {
		return nil, errors.New("not a checkpoint bundle: no manifest.json")
	}
	if err := b.Manifest.Validate(); err != nil {
		return nil, err
	}
	listed := map[string]FileEntry{}
	for _, f := range b.Manifest.Files {
		listed[f.Path] = f
		data, ok := b.files[f.Path]
		if !ok {
			return nil, fmt.Errorf("checkpoint %s: member %s is listed but missing", b.Manifest.ID, f.Path)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return nil, fmt.Errorf("checkpoint %s: member %s does not match its recorded hash", b.Manifest.ID, f.Path)
		}
	}
	for p := range b.files {
		if _, ok := listed[p]; !ok {
			return nil, fmt.Errorf("checkpoint %s: member %s is not listed in the manifest", b.Manifest.ID, p)
		}
	}
	return b, nil
}

// OpenFile opens a bundle on disk, decrypting it first when it is sealed (see crypto.go).
// passphrase is consulted only for a sealed bundle.
func OpenFile(p string, passphrase func() (string, error)) (*Bundle, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if IsSealed(data) {
		if passphrase == nil {
			return nil, fmt.Errorf("%s is encrypted; a passphrase is required", p)
		}
		pass, err := passphrase()
		if err != nil {
			return nil, err
		}
		if data, err = Unseal(data, pass); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return Open(bytes.NewReader(data))
}

// File returns a member's bytes.
func (b *Bundle) File(p string) ([]byte, bool) {
	data, ok := b.files[p]
	return data, ok
}

// Files lists member paths under a prefix, sorted.
func (b *Bundle) Files(prefix string) []string {
	var out []string
	for p := range b.files {
		if prefix == "" || strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Age is how old the checkpoint is at now.
func (m Manifest) Age(now time.Time) time.Duration { return now.Sub(m.CreatedAt) }
