// Package externalwork inventories local coding-agent session metadata. It never opens
// conversation transcripts; finding a saved conversation does not prove it is running.
package externalwork

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Candidate struct {
	Source     string    `json:"source"`
	ID         string    `json:"id"`
	Title      string    `json:"title,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	RecordPath string    `json:"record_path"`
	UpdatedAt  time.Time `json:"updated_at"`
	Activity   string    `json:"activity"`
}

func (c Candidate) Ref() string { return c.Source + ":" + c.ID }

type Inventory struct {
	Version    int                 `json:"version"`
	ScannedAt  time.Time           `json:"scanned_at"`
	Configs    map[string][]string `json:"configs"`
	Candidates []Candidate         `json:"candidates"`
	Warnings   []string            `json:"warnings,omitempty"`
}

// Scan checks only metadata: Codex's thread index and Claude/Cursor filenames and
// modification times. Activity remains unknown until the originating tool confirms it.
func Scan(ctx context.Context, home, workspace string, since time.Time) Inventory {
	inv := Inventory{Version: 1, ScannedAt: time.Now(), Configs: map[string][]string{}, Candidates: []Candidate{}}
	roots := []string{filepath.Join(home, ".codex")}
	// Codex Desktop can place its state at a mounted volume root on macOS.
	if mounted, err := filepath.Glob("/Volumes/*/.codex"); err == nil {
		roots = append(roots, mounted...)
	}
	if custom := os.Getenv("CODEX_HOME"); custom != "" {
		roots = append([]string{custom}, roots...)
	}
	seenRoots := map[string]bool{}
	for _, root := range roots {
		if seenRoots[root] {
			continue
		}
		seenRoots[root] = true
		if _, err := os.Stat(root); err != nil {
			continue
		}
		inv.Configs["codex"] = appendExisting(inv.Configs["codex"], filepath.Join(root, "config.toml"))
		path := filepath.Join(root, "state_5.sqlite")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		rows, err := codexRows(ctx, path)
		if err != nil {
			inv.Warnings = append(inv.Warnings, fmt.Sprintf("Codex index %s: %v", path, err))
			continue
		}
		for _, row := range rows {
			if row.Archived != 0 || row.ID == "" || time.Unix(row.UpdatedAt, 0).Before(since) {
				continue
			}
			inv.Candidates = append(inv.Candidates, Candidate{Source: "codex", ID: row.ID, Title: row.Title, Cwd: row.Cwd, RecordPath: path, UpdatedAt: time.Unix(row.UpdatedAt, 0), Activity: "saved; running state unknown"})
		}
	}
	claude := filepath.Join(home, ".claude")
	inv.Configs["claude"] = appendExisting(nil, filepath.Join(claude, "settings.json"), filepath.Join(claude, "settings.local.json"), filepath.Join(home, ".claude.json"))
	inv.Candidates = append(inv.Candidates, scanFiles(filepath.Join(claude, "projects"), "claude", since, func(path string) bool {
		return filepath.Ext(path) == ".jsonl" && validUUID(strings.TrimSuffix(filepath.Base(path), ".jsonl"))
	})...)
	cursor := filepath.Join(home, ".cursor")
	inv.Configs["cursor"] = appendExisting(nil, filepath.Join(cursor, "mcp.json"), filepath.Join(cursor, "cli-config.json"), filepath.Join(cursor, "hooks.json"), filepath.Join(home, "Library/Application Support/Cursor/User/settings.json"))
	if workspace != "" {
		inv.Configs["codex"] = appendExisting(inv.Configs["codex"], filepath.Join(workspace, ".codex/hooks.json"), filepath.Join(workspace, "AGENTS.md"))
		inv.Configs["claude"] = appendExisting(inv.Configs["claude"], filepath.Join(workspace, ".mcp.json"), filepath.Join(workspace, ".claude/settings.json"), filepath.Join(workspace, ".claude/settings.local.json"))
		inv.Configs["cursor"] = appendExisting(inv.Configs["cursor"], filepath.Join(workspace, ".cursor/mcp.json"), filepath.Join(workspace, ".cursor/rules/conductor.mdc"))
	}
	inv.Candidates = append(inv.Candidates, scanFiles(filepath.Join(cursor, "projects"), "cursor", since, func(path string) bool {
		return filepath.Ext(path) == ".jsonl" && strings.Contains(path, string(filepath.Separator)+"agent-transcripts"+string(filepath.Separator)) && validUUID(strings.TrimSuffix(filepath.Base(path), ".jsonl"))
	})...)
	// The same Codex thread can appear in both a current and an older state root.
	sort.Slice(inv.Candidates, func(i, j int) bool { return inv.Candidates[i].UpdatedAt.After(inv.Candidates[j].UpdatedAt) })
	seen := map[string]bool{}
	unique := inv.Candidates[:0]
	for _, c := range inv.Candidates {
		if !seen[c.Ref()] {
			unique = append(unique, c)
			seen[c.Ref()] = true
		}
	}
	inv.Candidates = unique
	return inv
}

func appendExisting(dst []string, paths ...string) []string {
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			dst = append(dst, p)
		}
	}
	return dst
}

type codexRow struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Cwd       string `json:"cwd"`
	UpdatedAt int64  `json:"updated_at"`
	Archived  int    `json:"archived"`
}

func codexRows(ctx context.Context, path string) ([]codexRow, error) {
	cmd := exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", path, "SELECT id,title,cwd,updated_at,archived FROM threads")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sqlite3: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var rows []codexRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func scanFiles(root, source string, since time.Time, accept func(string) bool) []Candidate {
	var out []Candidate
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !accept(path) {
			return nil
		}
		st, e := d.Info()
		if e != nil || st.ModTime().Before(since) {
			return nil
		}
		out = append(out, Candidate{Source: source, ID: strings.TrimSuffix(d.Name(), ".jsonl"), RecordPath: path, UpdatedAt: st.ModTime(), Activity: "saved; running state unknown"})
		return nil
	})
	return out
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, b := range []byte(s) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if b != '-' {
				return false
			}
			continue
		}
		if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')) {
			return false
		}
	}
	return true
}

// ValidCandidate limits imported identifiers to the three supported local sources.
// Display titles and local paths are not sent to Conductor.
func ValidCandidate(c Candidate) bool {
	return (c.Source == "codex" || c.Source == "claude" || c.Source == "cursor") && validUUID(c.ID)
}
