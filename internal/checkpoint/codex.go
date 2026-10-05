package checkpoint

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/usage"
)

// Codex writes one rollout per thread at
// $CODEX_HOME/sessions/YYYY/MM/DD/rollout-<timestamp>-<thread id>.jsonl. Every line is
// {timestamp, type, payload}: the first is session_meta (id, cwd, cli_version); event_msg
// payloads of type user_message / agent_message carry the conversation as the person saw
// it; response_item payloads are the model-facing items, of which function_call names the
// tools used. `codex resume <id>` finds a rollout by scanning the sessions tree for the id
// in the file name, so a rollout copied to another CODEX_HOME keeps its name and is
// resumable there.

// CodexSource locates a Codex rollout.
type CodexSource struct {
	CodexHome string
	SessionID string
	Path      string
	RelPath   string // relative to CodexHome
}

// LocateCodex finds a rollout by thread id, or the newest for cwd written since `since`.
func LocateCodex(codexHome, cwd, sessionID string, since time.Time) (CodexSource, error) {
	src := CodexSource{CodexHome: codexHome}
	root := filepath.Join(codexHome, "sessions")
	var newest string
	var newestAt time.Time
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if sessionID != "" {
			if strings.Contains(name, sessionID) {
				src.Path = p
				return filepath.SkipAll
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(since) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		meta, ok := usage.CodexMeta(f)
		f.Close()
		if !ok || (cwd != "" && meta.Cwd != cwd) {
			return nil
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = p, info.ModTime()
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return src, err
	}
	if src.Path == "" {
		src.Path = newest
	}
	if src.Path == "" {
		if sessionID != "" {
			return src, fmt.Errorf("no Codex rollout for thread %s under %s", sessionID, codexHome)
		}
		return src, fmt.Errorf("no Codex rollout for %s has been written since %s", cwd, since.Format(time.RFC3339))
	}
	f, err := os.Open(src.Path)
	if err != nil {
		return src, err
	}
	meta, ok := usage.CodexMeta(f)
	f.Close()
	if !ok {
		return src, fmt.Errorf("%s has no session_meta record", src.Path)
	}
	src.SessionID = meta.ID
	if rel, err := filepath.Rel(codexHome, src.Path); err == nil {
		src.RelPath = filepath.ToSlash(rel)
	}
	return src, nil
}

// ParseCodex builds the neutral conversation from a rollout.
func ParseCodex(data []byte) *Conversation {
	c := &Conversation{Harness: "codex"}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	type line struct {
		Timestamp time.Time       `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	type meta struct {
		ID         string `json:"id"`
		Cwd        string `json:"cwd"`
		CLIVersion string `json:"cli_version"`
	}
	type event struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	type item struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		CallID    string          `json:"call_id"`
		Output    json.RawMessage `json:"output"`
		Action    struct {
			Command []string `json:"command"`
		} `json:"action"`
	}
	var sawEventMessages bool
	var pendingItems []Turn // response_item messages, used only if no event_msg carried text
	var lastAssistant *Turn
	toolByID := map[string]*ToolCall{}
	assistant := func(at time.Time) *Turn {
		if lastAssistant == nil {
			c.Turns = append(c.Turns, Turn{Role: "assistant", At: at})
			lastAssistant = &c.Turns[len(c.Turns)-1]
		}
		return lastAssistant
	}
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
		if !l.Timestamp.IsZero() {
			if c.Started.IsZero() || l.Timestamp.Before(c.Started) {
				c.Started = l.Timestamp
			}
			if l.Timestamp.After(c.Last) {
				c.Last = l.Timestamp
			}
		}
		switch l.Type {
		case "session_meta":
			var m meta
			if json.Unmarshal(l.Payload, &m) == nil {
				if c.SessionID == "" {
					c.SessionID = m.ID
				}
				if c.Cwd == "" {
					c.Cwd = m.Cwd
				}
				if m.CLIVersion != "" {
					c.Version = m.CLIVersion
				}
			}
		case "event_msg":
			var e event
			if json.Unmarshal(l.Payload, &e) != nil {
				continue
			}
			switch e.Type {
			case "user_message":
				sawEventMessages = true
				lastAssistant = nil
				if t := strings.TrimSpace(e.Message); t != "" && !isInjectedContext(t) {
					c.Turns = append(c.Turns, Turn{Role: "user", At: l.Timestamp, Text: t})
				}
			case "agent_message":
				sawEventMessages = true
				a := assistant(l.Timestamp)
				a.Text = joinText(a.Text, e.Message)
			}
		case "response_item":
			var it item
			if json.Unmarshal(l.Payload, &it) != nil {
				continue
			}
			switch it.Type {
			case "message":
				var blocks []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				_ = json.Unmarshal(it.Content, &blocks)
				var sb strings.Builder
				for _, b := range blocks {
					if b.Type == "input_text" || b.Type == "output_text" {
						if sb.Len() > 0 {
							sb.WriteString("\n")
						}
						sb.WriteString(b.Text)
					}
				}
				t := strings.TrimSpace(sb.String())
				if t == "" || isInjectedContext(t) {
					continue
				}
				switch it.Role {
				case "user":
					pendingItems = append(pendingItems, Turn{Role: "user", At: l.Timestamp, Text: t})
				case "assistant":
					pendingItems = append(pendingItems, Turn{Role: "assistant", At: l.Timestamp, Text: t})
				}
			case "function_call", "custom_tool_call":
				a := assistant(l.Timestamp)
				input := map[string]any{}
				_ = json.Unmarshal([]byte(it.Arguments), &input)
				summary, path := ToolSummary(it.Name, input)
				a.Tools = append(a.Tools, ToolCall{Name: it.Name, Summary: summary, Path: path})
				if it.CallID != "" {
					toolByID[it.CallID] = &a.Tools[len(a.Tools)-1]
				}
			case "local_shell_call":
				a := assistant(l.Timestamp)
				summary, _ := ToolSummary("shell", map[string]any{"command": toAny(it.Action.Command)})
				a.Tools = append(a.Tools, ToolCall{Name: "shell", Summary: summary})
				if it.CallID != "" {
					toolByID[it.CallID] = &a.Tools[len(a.Tools)-1]
				}
			case "function_call_output", "custom_tool_call_output":
				if tc := toolByID[it.CallID]; tc != nil {
					var out string
					if json.Unmarshal(it.Output, &out) == nil && looksLikeFailure(out) {
						tc.Error = true
					}
				}
			}
		}
	}
	if !sawEventMessages && len(pendingItems) > 0 {
		// An older rollout, or an exec run: fold the model-facing messages in with the tool
		// calls already collected, in timestamp order.
		merged := make([]Turn, 0, len(c.Turns)+len(pendingItems))
		i, j := 0, 0
		for i < len(c.Turns) || j < len(pendingItems) {
			switch {
			case j >= len(pendingItems), i < len(c.Turns) && !c.Turns[i].At.After(pendingItems[j].At):
				merged = append(merged, c.Turns[i])
				i++
			default:
				merged = append(merged, pendingItems[j])
				j++
			}
		}
		c.Turns = merged
	}
	return c
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func looksLikeFailure(out string) bool {
	head := strings.ToLower(out)
	if len(head) > 200 {
		head = head[:200]
	}
	return strings.Contains(head, "exit code: 1") || strings.Contains(head, "exited with code") || strings.HasPrefix(head, "error")
}

// CodexInstall is where a rollout was put back.
type CodexInstall struct {
	CodexHome string
	Path      string
	SessionID string
}

// InstallCodex writes a bundle's rollout under a CODEX_HOME at the path it came from,
// rewriting the session's cwd when the directory moved so Codex does not stop to ask which
// to use.
func InstallCodex(b *Bundle, codexHome, cwd string, force bool) (CodexInstall, error) {
	m := b.Manifest
	data, ok := b.File(m.Transcript.Path)
	if !ok {
		return CodexInstall{}, errors.New("the checkpoint carries no transcript")
	}
	rel := m.Transcript.NativeRelPath
	if !SafeRelPath(rel) {
		rel = "sessions/" + m.CreatedAt.UTC().Format("2006/01/02") + "/rollout-" + m.CreatedAt.UTC().Format("2006-01-02T15-04-05") + "-" + m.SessionID + ".jsonl"
	}
	dst := filepath.Join(codexHome, filepath.FromSlash(rel))
	if _, err := os.Stat(dst); err == nil && !force {
		return CodexInstall{}, fmt.Errorf("%s already exists; pass --force to replace it", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return CodexInstall{}, err
	}
	if cwd != m.Cwd {
		data = rewriteCodexCwd(data, cwd)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return CodexInstall{}, err
	}
	return CodexInstall{CodexHome: codexHome, Path: dst, SessionID: m.SessionID}, nil
}

// rewriteCodexCwd sets the cwd in session_meta and turn_context payloads.
func rewriteCodexCwd(data []byte, to string) []byte {
	var out bytes.Buffer
	out.Grow(len(data))
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		raw := sc.Bytes()
		var rec map[string]json.RawMessage
		if json.Unmarshal(raw, &rec) == nil {
			var typ string
			_ = json.Unmarshal(rec["type"], &typ)
			if typ == "session_meta" || typ == "turn_context" {
				var payload map[string]json.RawMessage
				if json.Unmarshal(rec["payload"], &payload) == nil {
					if _, ok := payload["cwd"]; ok {
						enc, _ := json.Marshal(to)
						payload["cwd"] = enc
						if p, err := json.Marshal(payload); err == nil {
							rec["payload"] = p
							if fixed, err := json.Marshal(rec); err == nil {
								raw = fixed
							}
						}
					}
				}
			}
		}
		out.Write(raw)
		out.WriteByte('\n')
	}
	return out.Bytes()
}
