package checkpoint

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ClaudeInventoryRow identifies one native Claude Code conversation, including Desktop
// remote-control mirrors under projects/ssh-<id>/. Status is deliberately not inferred
// from the transcript: a recent write does not prove that a process is running.
type ClaudeInventoryRow struct {
	SessionID    string    `json:"session_id"`
	ConfigDir    string    `json:"config_dir"`
	Path         string    `json:"path"`
	Cwd          string    `json:"cwd,omitempty"`
	Title        string    `json:"title,omitempty"`
	LastActivity time.Time `json:"last_activity,omitzero"`
	Bytes        int64     `json:"bytes"`
	SHA256       string    `json:"sha256"`
	Extras       int       `json:"extras,omitempty"`
	ExtrasSHA256 string    `json:"extras_sha256,omitempty"`
	// Other session IDs can occur when Claude forks a chat and copies its history.
	EmbeddedSessionIDs []string `json:"embedded_session_ids,omitempty"`
	RemoteMirror       bool     `json:"remote_mirror,omitempty"`
	Status             string   `json:"status"`            // unknown until matched to a live session record
	Problem            string   `json:"problem,omitempty"` // this row cannot be migrated without repair
}

// InventoryClaude reads every top-level transcript in a Claude configuration directory.
// It returns an error for unreadable or changing files so a batch cannot silently claim
// completeness after omitting a conversation.
func InventoryClaude(configDir string, selectedID string) ([]ClaudeInventoryRow, error) {
	configDir, err := filepath.Abs(configDir)
	if err != nil {
		return nil, err
	}
	projects := filepath.Join(configDir, "projects")
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return nil, fmt.Errorf("read Claude projects in %s: %w", configDir, err)
	}
	var out []ClaudeInventoryRow
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(projects, dir.Name()))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), ".jsonl") || strings.HasPrefix(file.Name(), "agent-") || file.IsDir() ||
				(selectedID != "" && file.Name() != selectedID+".jsonl") {
				continue
			}
			p := filepath.Join(projects, dir.Name(), file.Name())
			row := ClaudeInventoryRow{SessionID: strings.TrimSuffix(file.Name(), ".jsonl"),
				ConfigDir: configDir, Path: p, Status: "unknown"}
			info, err := file.Info()
			if err != nil || !info.Mode().IsRegular() {
				row.Problem = "transcript is not a readable regular file"
			} else {
				row, err = inventoryClaudeFile(configDir, p, row.SessionID)
				if err != nil {
					row = ClaudeInventoryRow{SessionID: strings.TrimSuffix(file.Name(), ".jsonl"),
						ConfigDir: configDir, Path: p, Bytes: info.Size(),
						Status: "unknown", Problem: err.Error()}
				}
			}
			row.RemoteMirror = strings.HasPrefix(dir.Name(), "ssh-")
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastActivity.Equal(out[j].LastActivity) {
			return out[i].LastActivity.After(out[j].LastActivity)
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

func inventoryClaudeFile(configDir, path, id string) (ClaudeInventoryRow, error) {
	before, err := os.Stat(path)
	if err != nil {
		return ClaudeInventoryRow{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return ClaudeInventoryRow{}, err
	}
	defer f.Close()
	hash := sha256.New()
	sc := bufio.NewScanner(io.TeeReader(f, hash))
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	var title string
	var last time.Time
	cwds := make(map[string]int)
	ids := make(map[string]bool)
	var parsed int
	for sc.Scan() {
		var line struct {
			Type        string    `json:"type"`
			SessionID   string    `json:"sessionId"`
			Cwd         string    `json:"cwd"`
			CustomTitle string    `json:"customTitle"`
			AITitle     string    `json:"aiTitle"`
			Timestamp   time.Time `json:"timestamp"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue // Native formats vary; the original bytes are kept in the checkpoint.
		}
		parsed++
		if line.SessionID != "" {
			ids[line.SessionID] = true
		}
		if line.Cwd != "" && (line.Type == "user" || line.Type == "assistant") {
			cwds[line.Cwd]++
		}
		if line.CustomTitle != "" {
			title = line.CustomTitle
		} else if line.AITitle != "" {
			title = line.AITitle
		}
		if line.Timestamp.After(last) {
			last = line.Timestamp
		}
	}
	if err := sc.Err(); err != nil {
		return ClaudeInventoryRow{}, err
	}
	if parsed == 0 {
		return ClaudeInventoryRow{}, fmt.Errorf("no parseable JSONL records")
	}
	if !ids[id] {
		return ClaudeInventoryRow{}, fmt.Errorf("filename id %s has no matching sessionId in transcript", id)
	}
	after, err := os.Stat(path)
	if err != nil {
		return ClaudeInventoryRow{}, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return ClaudeInventoryRow{}, fmt.Errorf("transcript changed during inventory; retry")
	}
	var cwd string
	for candidate, count := range cwds {
		if count > cwds[cwd] || (count == cwds[cwd] && candidate < cwd) {
			cwd = candidate
		}
	}
	src := ClaudeSource{ConfigDir: configDir, SessionID: id, Path: path}
	if err := collectClaudeExtras(&src); err != nil {
		return ClaudeInventoryRow{}, fmt.Errorf("find Claude session sidecars: %w", err)
	}
	extrasHash, err := ClaudeExtrasDigest(src.Extras)
	if err != nil {
		return ClaudeInventoryRow{}, fmt.Errorf("read Claude session sidecars: %w", err)
	}
	var embedded []string
	for other := range ids {
		if other != id {
			embedded = append(embedded, other)
		}
	}
	sort.Strings(embedded)
	return ClaudeInventoryRow{SessionID: id, ConfigDir: configDir, Path: path,
		Cwd: cwd, Title: title, LastActivity: last, Bytes: after.Size(),
		SHA256: hex.EncodeToString(hash.Sum(nil)), Extras: len(src.Extras),
		ExtrasSHA256: extrasHash, EmbeddedSessionIDs: embedded, Status: "unknown"}, nil
}

// ClaudeExtrasDigest binds the separately stored tool and agent files as well as the main
// transcript. A sidecar can change without appending to the main JSONL.
func ClaudeExtrasDigest(extras []ClaudeExtra) (string, error) {
	if len(extras) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, extra := range extras { // collectClaudeExtras sorts by Rel
		before, err := os.Stat(extra.Path)
		if err != nil {
			return "", err
		}
		f, err := os.Open(extra.Path)
		if err != nil {
			return "", err
		}
		contentHash := sha256.New()
		_, copyErr := io.Copy(contentHash, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		after, err := os.Stat(extra.Path)
		if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return "", fmt.Errorf("Claude sidecar %s changed during inventory", extra.Rel)
		}
		fmt.Fprintf(h, "%s\x00%s\x00", extra.Rel, hex.EncodeToString(contentHash.Sum(nil)))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ClaudeManifestExtrasDigest computes the same digest from the sealed checkpoint manifest.
func ClaudeManifestExtrasDigest(m Manifest, sessionID string) (string, int) {
	prefix := NativeDir + "/claude/" + sessionID + "/"
	var extras []FileEntry
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, prefix) {
			extras = append(extras, f)
		}
	}
	if len(extras) == 0 {
		return "", 0
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].Path < extras[j].Path })
	h := sha256.New()
	for _, f := range extras {
		fmt.Fprintf(h, "%s\x00%s\x00", strings.TrimPrefix(f.Path, prefix), f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil)), len(extras)
}
