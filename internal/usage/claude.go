package usage

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude Code keeps one JSONL transcript per session under
// ~/.claude/projects/<cwd with every non-alphanumeric rune replaced by '-'>/<session>.jsonl
// (or under $CLAUDE_CONFIG_DIR). Each assistant record carries message.usage and
// message.model. A streamed response is written as one record per content block, all sharing
// a requestId and the same usage block, so usage is counted once per requestId.

// claudeRecord is the subset of a transcript line this package looks at. There is no field
// for message content, so it cannot be decoded even by accident.
type claudeRecord struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"sessionId"`
	RequestID string    `json:"requestId"`
	Effort    string    `json:"effort"`
	Message   struct {
		Model string `json:"model"`
		Usage struct {
			Input      int64 `json:"input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			Output     int64 `json:"output_tokens"`
			Details    struct {
				Thinking int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"message"`
}

// ClaudeTurns parses a Claude Code transcript into turns.
func ClaudeTurns(r io.Reader) ([]Turn, error) {
	var out []Turn
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20) // tool results make long lines
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec claudeRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // a torn last line while the harness is writing; the next read gets it
		}
		if rec.Type != "assistant" || rec.Message.Model == "" {
			continue
		}
		if rec.RequestID != "" {
			if seen[rec.RequestID] {
				continue
			}
			seen[rec.RequestID] = true
		}
		u := rec.Message.Usage
		if u.Input+u.CacheRead+u.CacheWrite+u.Output == 0 {
			continue
		}
		out = append(out, Turn{
			Harness: "claude", ExternalSessionID: rec.SessionID,
			Model: rec.Message.Model, Provider: "anthropic", Effort: rec.Effort, At: rec.Timestamp,
			Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
			Output: u.Output, Reasoning: u.Details.Thinking,
		})
	}
	return out, sc.Err()
}

// ClaudeProjectDir is where Claude Code keeps the transcripts for a working directory.
func ClaudeProjectDir(configDir, cwd string) string {
	encoded := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '-'
	}, cwd)
	return filepath.Join(configDir, "projects", encoded)
}

// ClaudeConfigDir honors CLAUDE_CONFIG_DIR, defaulting to ~/.claude.
func ClaudeConfigDir(getenv func(string) string) string {
	if v := getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// ClaudeSessionFiles lists the transcripts for cwd that were written to since a moment —
// the sessions a wrap launched at that moment could have produced.
func ClaudeSessionFiles(configDir, cwd string, since time.Time) ([]string, error) {
	dir := ClaudeProjectDir(configDir, cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(since) {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

// ReadClaude collects the turns of every Claude Code session for cwd active since a moment.
func ReadClaude(configDir, cwd string, since time.Time) ([]Turn, error) {
	files, err := ClaudeSessionFiles(configDir, cwd, since)
	if err != nil {
		return nil, err
	}
	var out []Turn
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		turns, err := ClaudeTurns(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		for _, t := range turns {
			// A transcript resumed with --continue holds turns from before this launch; the
			// ledger keys on the harness session, so re-reporting them is harmless, but a
			// collector scoped to "since launch" should not attribute them to it.
			if !t.At.Before(since) {
				out = append(out, t)
			}
		}
	}
	return out, nil
}
