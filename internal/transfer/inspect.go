package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// InspectReport is the read-only view of a bundle. It never writes anything.
type InspectReport struct {
	Bundle        string    `json:"bundle"`
	FormatVersion string    `json:"format_version"`
	Tool          string    `json:"tool"`
	CreatedAt     string    `json:"created_at"`
	Device        string    `json:"created_by_device,omitempty"`
	SourceHome    string    `json:"source_home,omitempty"`
	Project       string    `json:"project,omitempty"`
	Files         int       `json:"files"`
	Bytes         int64     `json:"bytes"`
	Sessions      []Session `json:"sessions"`
	// MissingProjects lists recorded working directories that do not exist on this machine;
	// a Claude import would still land, but the sidebar group would be for a path that is not
	// here, so the caller should offer --map-cwd.
	MissingProjects []string `json:"missing_projects,omitempty"`
	Redacted        int      `json:"redacted,omitempty"`
	SecretsFound    int      `json:"secrets_found,omitempty"`
}

// Inspect describes a bundle, optionally scanning its transcripts for likely secrets.
func Inspect(b *Bundle, scan bool) (InspectReport, error) {
	entries := 0
	for _, e := range b.Entries() {
		if e != ManifestName && e != ChecksumsName {
			entries++
		}
	}
	r := InspectReport{
		Bundle:        b.Path,
		FormatVersion: b.Manifest.FormatVersion,
		Tool:          b.Manifest.Tool,
		CreatedAt:     b.Manifest.CreatedAt,
		Device:        b.Manifest.CreatedByDevice,
		SourceHome:    b.Manifest.SourceCodexHome,
		Project:       b.Manifest.SourceProjectPath,
		Sessions:      b.Manifest.Sessions,
		Files:         entries,
	}
	if r.Sessions == nil {
		r.Sessions = []Session{}
	}
	if b.Manifest.Conductor != nil {
		r.Redacted = b.Manifest.Conductor.Redacted
		r.SecretsFound = b.Manifest.Conductor.SecretsFound
	}
	seen := map[string]bool{}
	for _, s := range b.Manifest.Sessions {
		r.Bytes += s.SizeBytes
		cwd := filepath.Clean(s.OriginalCWD)
		if cwd == "" || cwd == "." || seen[cwd] {
			continue
		}
		seen[cwd] = true
		if _, err := os.Stat(cwd); err != nil {
			r.MissingProjects = append(r.MissingProjects, cwd)
		}
	}
	if scan {
		found, err := ScanBundle(b)
		if err != nil {
			return r, err
		}
		r.SecretsFound += found
	}
	return r, nil
}

// ScanBundle counts likely secrets in every transcript a bundle carries. Compressed entries
// are checked when zstd is installed and otherwise skipped (they are counted as no finding,
// which the caller should surface as incomplete).
func ScanBundle(b *Bundle) (int, error) {
	total := 0
	for _, s := range b.Manifest.Sessions {
		data, err := b.Read(s.BundlePath)
		if err != nil {
			return total, err
		}
		if strings.HasSuffix(s.BundlePath, ".zst") {
			if !zstdAvailable() {
				continue
			}
			plain, err := decompressZstdBytes(data)
			if err != nil {
				continue
			}
			data = plain
		}
		total += len(ScanSecrets(data))
	}
	return total, nil
}

// Summary is one line about a bundle, for a list.
func (r InspectReport) Summary() string {
	return fmt.Sprintf("%s: %d session(s) from %s, %d file(s), %s",
		r.Tool, len(r.Sessions), orDash(r.Project), r.Files, humanBytes(r.Bytes))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
