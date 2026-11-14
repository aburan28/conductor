// Package externalwork inventories local coding-agent session metadata. It never opens
// conversation transcripts; finding a saved conversation does not prove it is running.
package externalwork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
	// These facts describe one process-table snapshot, not whether the task is unfinished.
	ProcessAliveAtScan bool `json:"process_alive_at_scan,omitempty"`
	ProcessID          int  `json:"process_id,omitempty"`
}

func (c Candidate) Ref() string { return c.Source + ":" + c.ID }

type Inventory struct {
	Version    int                 `json:"version"`
	StartedAt  time.Time           `json:"started_at,omitzero"`
	ScannedAt  time.Time           `json:"scanned_at"`
	Configs    map[string][]string `json:"configs"`
	Candidates []Candidate         `json:"candidates"`
	Warnings   []string            `json:"warnings,omitempty"`
}

const sourceTimeout = 60 * time.Second

// Scan reads saved-session metadata and looks for an exact UUID in a live Codex/Claude
// resume invocation. A live process does not establish that its task is unfinished.
func Scan(ctx context.Context, home, workspace string, since time.Time) (Inventory, error) {
	inv := Inventory{Version: 1, StartedAt: time.Now(), Configs: map[string][]string{}, Candidates: []Candidate{}}
	roots, err := codexRoots(ctx, home, os.Getenv("CODEX_HOME"), "/Volumes")
	if err != nil {
		return inv, fmt.Errorf("Codex root discovery: %w", err)
	}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return inv, err
		}
		inv.Configs["codex"] = appendExisting(inv.Configs["codex"], filepath.Join(root, "config.toml"))
		path := filepath.Join(root, "state_5.sqlite")
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return inv, fmt.Errorf("Codex index %s: %w", path, err)
		}
		sourceCtx, cancel := context.WithTimeout(ctx, sourceTimeout)
		rows, err := codexRows(sourceCtx, path, since)
		cancel()
		if err != nil {
			return inv, fmt.Errorf("Codex index %s: %w", path, err)
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
	claudeCtx, cancel := context.WithTimeout(ctx, sourceTimeout)
	claudeCandidates, err := scanFiles(claudeCtx, filepath.Join(claude, "projects"), "claude", since, func(path string) bool {
		return filepath.Ext(path) == ".jsonl" && validUUID(strings.TrimSuffix(filepath.Base(path), ".jsonl"))
	})
	cancel()
	if err != nil {
		return inv, fmt.Errorf("Claude projects %s: %w", filepath.Join(claude, "projects"), err)
	}
	inv.Candidates = append(inv.Candidates, claudeCandidates...)
	cursor := filepath.Join(home, ".cursor")
	inv.Configs["cursor"] = appendExisting(nil, filepath.Join(cursor, "mcp.json"), filepath.Join(cursor, "cli-config.json"), filepath.Join(cursor, "hooks.json"), filepath.Join(home, "Library/Application Support/Cursor/User/settings.json"))
	if workspace != "" {
		inv.Configs["codex"] = appendExisting(inv.Configs["codex"], filepath.Join(workspace, ".codex/hooks.json"), filepath.Join(workspace, "AGENTS.md"))
		inv.Configs["claude"] = appendExisting(inv.Configs["claude"], filepath.Join(workspace, ".mcp.json"), filepath.Join(workspace, ".claude/settings.json"), filepath.Join(workspace, ".claude/settings.local.json"))
		inv.Configs["cursor"] = appendExisting(inv.Configs["cursor"], filepath.Join(workspace, ".cursor/mcp.json"), filepath.Join(workspace, ".cursor/rules/conductor.mdc"))
	}
	cursorCtx, cancel := context.WithTimeout(ctx, sourceTimeout)
	cursorCandidates, err := scanFiles(cursorCtx, filepath.Join(cursor, "projects"), "cursor", since, func(path string) bool {
		return filepath.Ext(path) == ".jsonl" && strings.Contains(path, string(filepath.Separator)+"agent-transcripts"+string(filepath.Separator)) && validUUID(strings.TrimSuffix(filepath.Base(path), ".jsonl"))
	})
	cancel()
	if err != nil {
		return inv, fmt.Errorf("Cursor projects %s: %w", filepath.Join(cursor, "projects"), err)
	}
	inv.Candidates = append(inv.Candidates, cursorCandidates...)
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
	processCtx, cancel := context.WithTimeout(ctx, sourceTimeout)
	active, err := explicitResumeProcesses(processCtx, os.Getuid())
	cancel()
	if err != nil {
		return inv, fmt.Errorf("live process check: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return inv, err
	}
	markProcessAliveAtScan(inv.Candidates, active)
	inv.ScannedAt = time.Now()
	return inv, nil
}

// codexRoots lists /Volumes itself once, then checks each literal .codex path.
// filepath.Glob("/Volumes/*/.codex") opens every mounted volume root to match
// the wildcard; an unresponsive mount can stall the whole background scan.
func codexRoots(ctx context.Context, home, custom, volumes string) ([]string, error) {
	roots := []string{}
	if custom != "" {
		roots = append(roots, custom)
	}
	roots = append(roots, filepath.Join(home, ".codex"))
	if entries, err := os.ReadDir(volumes); err == nil {
		for _, entry := range entries {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			root := filepath.Join(volumes, entry.Name(), ".codex")
			if st, err := os.Stat(root); err == nil && st.IsDir() {
				roots = append(roots, root)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%s: %w", root, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", volumes, err)
	}
	seen := map[string]bool{}
	unique := roots[:0]
	for _, root := range roots {
		if !seen[root] {
			seen[root] = true
			unique = append(unique, root)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return unique, nil
}

func markProcessAliveAtScan(candidates []Candidate, active map[string]int) {
	for i := range candidates {
		if pid := active[strings.ToLower(candidates[i].Ref())]; pid > 0 {
			candidates[i].ProcessAliveAtScan = true
			candidates[i].ProcessID = pid
			candidates[i].Activity = "session process observed at scan; task state unknown"
		}
	}
}

// explicitResumeProcesses keeps only the PID and exact session reference. Process argv may
// contain private prompts, so it must never be saved in the inventory or a task card.
func explicitResumeProcesses(ctx context.Context, uid int) (map[string]int, error) {
	out, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,uid=,comm=,args=").Output()
	if err != nil {
		return nil, err
	}
	return parseResumeProcesses(string(out), uid), nil
}

func parseResumeProcesses(output string, uid int) map[string]int {
	refs := map[string]int{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		owner, uidErr := strconv.Atoi(fields[1])
		if pidErr != nil || uidErr != nil || pid <= 0 || owner != uid {
			continue
		}
		comm := filepath.Base(fields[2])
		argv := fields[3:]
		if filepath.Base(argv[0]) != comm {
			continue
		}
		var source, id string
		switch comm {
		case "codex":
			if len(argv) >= 3 && argv[1] == "resume" {
				source, id = "codex", argv[2]
			}
		case "claude":
			if len(argv) >= 3 && (argv[1] == "--resume" || argv[1] == "-r") {
				source, id = "claude", argv[2]
			} else if len(argv) >= 2 && strings.HasPrefix(argv[1], "--resume=") {
				source, id = "claude", strings.TrimPrefix(argv[1], "--resume=")
			}
		}
		if validUUID(id) {
			refs[source+":"+strings.ToLower(id)] = pid
		}
	}
	return refs
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

func codexRows(ctx context.Context, path string, since time.Time) ([]codexRow, error) {
	// Codex indexes updated_at. Keep the time predicate first and filter archived rows
	// afterward: combining archived=0 here can make SQLite choose its archived index and
	// sort the whole result instead of seeking to the recent end of updated_at.
	query := fmt.Sprintf("SELECT id,title,cwd,updated_at,archived FROM threads WHERE updated_at >= %d ORDER BY updated_at DESC", since.Unix())
	cmd := exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", path, query)
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("sqlite3: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	// sqlite3 -json writes no bytes for a successful query with zero rows.
	if len(bytes.TrimSpace(stdout.Bytes())) == 0 {
		return []codexRow{}, nil
	}
	var rows []codexRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func scanFiles(ctx context.Context, root, source string, since time.Time, accept func(string) bool) ([]Candidate, error) {
	var out []Candidate
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if path == root && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !accept(path) {
			return nil
		}
		st, e := d.Info()
		if e != nil {
			return e
		}
		if st.ModTime().Before(since) {
			return nil
		}
		out = append(out, Candidate{Source: source, ID: strings.TrimSuffix(d.Name(), ".jsonl"), RecordPath: path, UpdatedAt: st.ModTime(), Activity: "saved; running state unknown"})
		return nil
	})
	return out, err
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
