package checkpoint

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/usage"
)

// Claude Code keeps one JSONL transcript per session at
// $CLAUDE_CONFIG_DIR/projects/<cwd slug>/<session uuid>.jsonl, with a sibling directory of
// the same name holding subagent transcripts and spilled tool results. Every line is a JSON
// object; the ones that matter here are type "user" and "assistant", whose message.content
// is either a string or an array of blocks (text, tool_use, tool_result, thinking), and
// type "summary", which a compaction writes. The format is Claude Code's own and changes
// between versions, so parsing is tolerant: an unknown line is counted and skipped.
//
// Resume is a transcript lookup: `claude --resume <id>` searches every project directory
// under the config dir, and `claude --resume <absolute path>` opens a file directly. The
// config dir also holds the credentials, so a transcript placed under a different
// CLAUDE_CONFIG_DIR is resumed by that directory's login.

// ClaudeSource locates a Claude Code session's files.
type ClaudeSource struct {
	ConfigDir string
	SessionID string
	Path      string   // the transcript
	Extras    []string // files under the sibling <session>/ directory, absolute
}

// ClaudeSessionForPID reads the session id Claude Code records for a running process
// (config/sessions/<pid>.json, recent versions), so a wrapper that knows the child's pid
// can name the exact session instead of guessing from mtimes.
func ClaudeSessionForPID(configDir string, pid int) (string, bool) {
	data, err := os.ReadFile(filepath.Join(configDir, "sessions", fmt.Sprintf("%d.json", pid)))
	if err != nil {
		return "", false
	}
	var rec struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(data, &rec) != nil || rec.SessionID == "" {
		return "", false
	}
	return rec.SessionID, true
}

// LocateClaude finds a session's transcript. With a session id it looks in the cwd's
// project directory first and then in every project directory; without one it takes the
// transcript for cwd most recently written to since `since`.
func LocateClaude(configDir, cwd, sessionID string, since time.Time) (ClaudeSource, error) {
	src := ClaudeSource{ConfigDir: configDir}
	projectDir := usage.ClaudeProjectDir(configDir, cwd)
	if sessionID != "" {
		candidates := []string{filepath.Join(projectDir, sessionID+".jsonl")}
		if entries, err := os.ReadDir(filepath.Join(configDir, "projects")); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					candidates = append(candidates, filepath.Join(configDir, "projects", e.Name(), sessionID+".jsonl"))
				}
			}
		}
		for _, p := range candidates {
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				src.SessionID, src.Path = sessionID, p
				break
			}
		}
		if src.Path == "" {
			return src, fmt.Errorf("no Claude Code transcript for session %s under %s", sessionID, configDir)
		}
	} else {
		files, err := usage.ClaudeSessionFiles(configDir, cwd, since)
		if err != nil {
			return src, err
		}
		var newest string
		var newestAt time.Time
		for _, p := range files {
			st, err := os.Stat(p)
			if err != nil {
				continue
			}
			if newest == "" || st.ModTime().After(newestAt) {
				newest, newestAt = p, st.ModTime()
			}
		}
		if newest == "" {
			return src, fmt.Errorf("no Claude Code transcript for %s has been written since %s", cwd, since.Format(time.RFC3339))
		}
		src.Path = newest
		src.SessionID = strings.TrimSuffix(filepath.Base(newest), ".jsonl")
	}
	// Subagent transcripts and spilled tool results live beside the transcript.
	side := strings.TrimSuffix(src.Path, ".jsonl")
	_ = filepath.WalkDir(side, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() && info.Size() <= maxUntrackedFileBytes {
			src.Extras = append(src.Extras, p)
		}
		return nil
	})
	sort.Strings(src.Extras)
	return src, nil
}

// ParseClaude builds the neutral conversation from a transcript.
func ParseClaude(data []byte) *Conversation {
	c := &Conversation{Harness: "claude"}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	type line struct {
		Type        string          `json:"type"`
		UUID        string          `json:"uuid"`
		Timestamp   time.Time       `json:"timestamp"`
		SessionID   string          `json:"sessionId"`
		Cwd         string          `json:"cwd"`
		Version     string          `json:"version"`
		IsSidechain bool            `json:"isSidechain"`
		IsMeta      bool            `json:"isMeta"`
		RequestID   string          `json:"requestId"`
		AITitle     string          `json:"aiTitle"`
		Summary     string          `json:"summary"`
		Message     json.RawMessage `json:"message"`
	}
	type msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	type block struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Input   json.RawMessage `json:"input"`
		IsError bool            `json:"is_error"`
		ID      string          `json:"id"`
		UseID   string          `json:"tool_use_id"`
	}
	var lastAssistant *Turn
	var lastRequest string
	toolByID := map[string]*ToolCall{}
	for sc.Scan() {
		raw := sc.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var l line
		if json.Unmarshal(raw, &l) != nil {
			continue
		}
		c.Records++
		if c.SessionID == "" && l.SessionID != "" {
			c.SessionID = l.SessionID
		}
		if c.Cwd == "" && l.Cwd != "" {
			c.Cwd = l.Cwd
		}
		if l.Version != "" {
			c.Version = l.Version
		}
		if l.AITitle != "" {
			c.Title = l.AITitle
		}
		if !l.Timestamp.IsZero() {
			if c.Started.IsZero() || l.Timestamp.Before(c.Started) {
				c.Started = l.Timestamp
			}
			if l.Timestamp.After(c.Last) {
				c.Last = l.Timestamp
			}
		}
		if l.Type == "summary" && l.Summary != "" {
			c.Summary = l.Summary
			continue
		}
		if l.IsSidechain || l.IsMeta || (l.Type != "user" && l.Type != "assistant") {
			continue
		}
		var m msg
		if json.Unmarshal(l.Message, &m) != nil {
			continue
		}
		var text strings.Builder
		var blocks []block
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			text.WriteString(s)
		} else {
			_ = json.Unmarshal(m.Content, &blocks)
		}
		switch l.Type {
		case "user":
			lastAssistant = nil
			var results int
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(b.Text)
				case "tool_result":
					results++
					if tc := toolByID[b.UseID]; tc != nil && b.IsError {
						tc.Error = true
					}
				}
			}
			t := strings.TrimSpace(text.String())
			if t == "" || (results > 0 && len(blocks) == results) {
				continue // a pure tool-result line is the agent's own turn continuing
			}
			if isInjectedContext(t) {
				continue
			}
			c.Turns = append(c.Turns, Turn{Role: "user", At: l.Timestamp, Text: t})
		case "assistant":
			// A streamed response is one line per content block, sharing a requestId;
			// fold them into one turn.
			if lastAssistant == nil || (l.RequestID != "" && l.RequestID != lastRequest) {
				c.Turns = append(c.Turns, Turn{Role: "assistant", At: l.Timestamp})
				lastAssistant = &c.Turns[len(c.Turns)-1]
				lastRequest = l.RequestID
			}
			if text.Len() > 0 {
				lastAssistant.Text = joinText(lastAssistant.Text, text.String())
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					lastAssistant.Text = joinText(lastAssistant.Text, b.Text)
				case "tool_use":
					summary, path := ToolSummary(b.Name, decodeMap(b.Input))
					lastAssistant.Tools = append(lastAssistant.Tools, ToolCall{Name: b.Name, Summary: summary, Path: path})
					if b.ID != "" {
						toolByID[b.ID] = &lastAssistant.Tools[len(lastAssistant.Tools)-1]
					}
				}
			}
		}
	}
	return c
}

func joinText(have, add string) string {
	add = strings.TrimSpace(add)
	if add == "" {
		return have
	}
	if have == "" {
		return add
	}
	return have + "\n\n" + add
}

// isInjectedContext recognises user-role lines the harness wrote itself (system reminders,
// command outputs, environment context) rather than the person.
func isInjectedContext(t string) bool {
	for _, p := range []string{"<system-reminder>", "<command-name>", "<local-command-stdout>", "<environment_context>", "<user_instructions>", "<task-notification>", "<bash-input>", "<bash-stdout>", "<bash-stderr>"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// ClaudeInstall is where a transcript was put back, and how to open it.
type ClaudeInstall struct {
	ConfigDir string
	Path      string
	SessionID string
}

// InstallClaude writes a bundle's Claude transcript into a config directory for `cwd`,
// rewriting each record's cwd when the directory moved. It refuses to overwrite a transcript
// that already exists unless force is set: the same session id in two project directories
// makes resume-by-id fail, so the caller resumes by path, which InstallClaude returns.
func InstallClaude(b *Bundle, configDir, cwd string, force bool) (ClaudeInstall, error) {
	m := b.Manifest
	data, ok := b.File(m.Transcript.Path)
	if !ok {
		return ClaudeInstall{}, errors.New("the checkpoint carries no transcript")
	}
	projectDir := usage.ClaudeProjectDir(configDir, cwd)
	dst := filepath.Join(projectDir, m.SessionID+".jsonl")
	if _, err := os.Stat(dst); err == nil && !force {
		return ClaudeInstall{}, fmt.Errorf("%s already exists; pass --force to replace it", dst)
	}
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		return ClaudeInstall{}, err
	}
	if cwd != m.Cwd {
		data = rewriteClaudeCwd(data, m.Cwd, cwd)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return ClaudeInstall{}, err
	}
	prefix := NativeDir + "/claude/" + m.SessionID + "/"
	for _, p := range b.Files(prefix) {
		rel := strings.TrimPrefix(p, prefix)
		if rel == "" {
			continue
		}
		// The session id was validated with the manifest; rel must stay beneath it too. The
		// harness's state directory is the user's own, and a bundle writes no links into it,
		// so a containment check is enough here (contrast writeInside for a checkout).
		if !SafeRelPath(rel) {
			return ClaudeInstall{}, fmt.Errorf("checkpoint member %s leaves its directory", p)
		}
		extra, _ := b.File(p)
		out := filepath.Join(projectDir, m.SessionID, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return ClaudeInstall{}, err
		}
		if err := os.WriteFile(out, extra, 0o600); err != nil {
			return ClaudeInstall{}, err
		}
	}
	return ClaudeInstall{ConfigDir: configDir, Path: dst, SessionID: m.SessionID}, nil
}

// rewriteClaudeCwd replaces the top-level cwd of each record. Only that field: paths inside
// messages are history, and history is left as it was.
func rewriteClaudeCwd(data []byte, from, to string) []byte {
	var out bytes.Buffer
	out.Grow(len(data))
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		raw := sc.Bytes()
		var rec map[string]json.RawMessage
		if json.Unmarshal(raw, &rec) == nil {
			if cwdRaw, ok := rec["cwd"]; ok {
				var cwd string
				if json.Unmarshal(cwdRaw, &cwd) == nil && (cwd == from || from == "") {
					enc, _ := json.Marshal(to)
					rec["cwd"] = enc
					if fixed, err := json.Marshal(rec); err == nil {
						raw = fixed
					}
				}
			}
		}
		out.Write(raw)
		out.WriteByte('\n')
	}
	return out.Bytes()
}
