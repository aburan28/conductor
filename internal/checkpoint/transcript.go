package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Conversation is the harness-neutral view of a transcript: what the person asked, what
// the agent said and did, in order. It is built from the native transcript at capture time
// and rendered as CONTINUATION.md so that a different harness — one that cannot read the
// native format and has no account for the original conversation — can take the work over.
//
// This is the one place in Conductor that reads conversation content. It runs on the
// user's own machine, over the user's own transcript, and its output goes into the same
// local bundle as the transcript itself. Nothing here is reachable from the control plane.
type Conversation struct {
	Harness   string
	SessionID string
	Title     string
	Cwd       string
	Version   string
	Started   time.Time
	Last      time.Time
	Records   int
	Turns     []Turn
	// Summary is a harness-written compaction summary, when the transcript carries one.
	Summary string
}

// Turn is one message from either side.
type Turn struct {
	Role  string // "user" or "assistant"
	At    time.Time
	Text  string
	Tools []ToolCall
}

// ToolCall is an assistant tool use reduced to its name and a one-line description.
type ToolCall struct {
	Name    string
	Summary string
	Path    string // the file it touched, when it touched one
	Error   bool   // the tool reported an error
}

// Counts summarise the conversation for the manifest.
func (c *Conversation) Counts() (users, assistants int) {
	for _, t := range c.Turns {
		switch t.Role {
		case "user":
			users++
		case "assistant":
			assistants++
		}
	}
	return
}

// FilesTouched lists the paths the agent edited or created, in first-seen order.
func (c *Conversation) FilesTouched() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range c.Turns {
		for _, tc := range t.Tools {
			if tc.Path == "" || !editingTool(tc.Name) || seen[tc.Path] {
				continue
			}
			seen[tc.Path] = true
			out = append(out, tc.Path)
		}
	}
	return out
}

func editingTool(name string) bool {
	switch strings.ToLower(name) {
	case "edit", "write", "multiedit", "notebookedit", "str_replace_editor", "apply_patch", "write_file", "edit_file", "patch", "create_file":
		return true
	}
	return false
}

// Rendering limits. The continuation is a prompt, and a prompt has a budget: the whole
// transcript would not fit and would not help. Older turns are elided from the middle,
// never from the beginning (the task) or the end (where the work stands).
const (
	maxTurnText     = 2500
	maxToolSummary  = 160
	maxContinuation = 160_000 // bytes of conversation log
	keepHeadTurns   = 6
)

// RenderContinuation writes CONTINUATION.md.
func RenderContinuation(m Manifest, c *Conversation, filesNow []string) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Continuation of a %s session\n\n", harnessTitle(c.Harness))
	sb.WriteString("This file was written by `conductor checkpoint` from a coding-agent session that stopped before its work was done. ")
	sb.WriteString("You are picking that work up. Read this whole file, then continue from **Where the work stands** below. ")
	sb.WriteString("Do not redo work the log shows as finished; verify it instead. If something in the log contradicts the working tree, the working tree wins.\n\n")

	sb.WriteString("## Provenance\n\n")
	fmt.Fprintf(&sb, "- Checkpoint: `%s` taken %s (%s)\n", m.ID, m.CreatedAt.UTC().Format(time.RFC3339), orDash(m.Reason))
	fmt.Fprintf(&sb, "- Original harness: %s", harnessTitle(c.Harness))
	if m.HarnessVersion != "" {
		fmt.Fprintf(&sb, " %s", m.HarnessVersion)
	}
	fmt.Fprintf(&sb, ", session `%s`\n", c.SessionID)
	if c.Title != "" {
		fmt.Fprintf(&sb, "- Title: %s\n", c.Title)
	}
	fmt.Fprintf(&sb, "- Working directory: `%s`\n", m.Cwd)
	if m.Repo.Root != "" {
		fmt.Fprintf(&sb, "- Repository: `%s`", m.Repo.Root)
		if m.Repo.Remote != "" {
			fmt.Fprintf(&sb, " (%s)", m.Repo.Remote)
		}
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "- Branch: `%s` at `%s`", orDash(m.Repo.Branch), short(m.Repo.Head))
		if m.Repo.Ahead > 0 {
			fmt.Fprintf(&sb, ", %d commit(s) not on any remote (carried in the checkpoint)", m.Repo.Ahead)
		}
		sb.WriteString("\n")
	}
	if m.Conductor.Task != "" || m.Conductor.Project != "" {
		fmt.Fprintf(&sb, "- Conductor: project `%s`", orDash(m.Conductor.Project))
		if m.Conductor.Task != "" {
			fmt.Fprintf(&sb, ", task `%s`", m.Conductor.Task)
		}
		sb.WriteString("\n")
	}
	users, assistants := c.Counts()
	fmt.Fprintf(&sb, "- Conversation: %d user message(s), %d assistant turn(s)", users, assistants)
	if !c.Last.IsZero() {
		fmt.Fprintf(&sb, ", last activity %s", c.Last.UTC().Format(time.RFC3339))
	}
	sb.WriteString("\n\n")

	if m.Note != "" {
		sb.WriteString("## Note left at checkpoint time\n\n")
		sb.WriteString(strings.TrimSpace(m.Note))
		sb.WriteString("\n\n")
	}

	// The task, verbatim: the first user message is the one thing never truncated.
	if first := firstUserTurn(c); first != nil {
		sb.WriteString("## Original request\n\n")
		sb.WriteString(quote(first.Text))
		sb.WriteString("\n\n")
	}

	if c.Summary != "" {
		sb.WriteString("## Summary the original harness kept\n\n")
		sb.WriteString(strings.TrimSpace(c.Summary))
		sb.WriteString("\n\n")
	}

	sb.WriteString("## Conversation log\n\n")
	sb.WriteString("User messages are quoted; assistant text follows; tool calls are listed one per line. Tool output is not included — re-run a tool if you need its result.\n\n")
	sb.WriteString(renderLog(c))
	sb.WriteString("\n")

	touched := c.FilesTouched()
	if len(touched) > 0 || len(filesNow) > 0 {
		sb.WriteString("## Files\n\n")
		if len(touched) > 0 {
			sb.WriteString("Edited or created during the session, in order:\n\n")
			for _, p := range touched {
				fmt.Fprintf(&sb, "- `%s`\n", p)
			}
			sb.WriteString("\n")
		}
		if len(filesNow) > 0 {
			sb.WriteString("Uncommitted in the working tree at checkpoint time (restored with the checkpoint):\n\n")
			for _, p := range filesNow {
				fmt.Fprintf(&sb, "- `%s`\n", p)
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString("## Where the work stands\n\n")
	if last := lastAssistantTurn(c); last != nil && strings.TrimSpace(last.Text) != "" {
		sb.WriteString("The last thing the previous agent said:\n\n")
		sb.WriteString(quote(truncate(last.Text, maxTurnText*2)))
		sb.WriteString("\n\n")
	}
	if last := lastUserTurn(c); last != nil && firstUserTurn(c) != last {
		sb.WriteString("The last thing the user said:\n\n")
		sb.WriteString(quote(truncate(last.Text, maxTurnText)))
		sb.WriteString("\n\n")
	}
	sb.WriteString("Start by running `git status` and `git log --oneline -5` to confirm the tree matches the state described above, then continue the task. ")
	sb.WriteString("When you reach a stopping point, say what you finished and what remains, so the next checkpoint carries it.\n")
	return []byte(sb.String())
}

func renderLog(c *Conversation) string {
	lines := make([]string, 0, len(c.Turns))
	for _, t := range c.Turns {
		lines = append(lines, renderTurn(t))
	}
	total := 0
	for _, l := range lines {
		total += len(l)
	}
	if total <= maxContinuation || len(lines) <= keepHeadTurns+1 {
		return strings.Join(lines, "\n")
	}
	// Keep the head (the task and its first steps) and as much of the tail as fits.
	head := lines[:keepHeadTurns]
	budget := maxContinuation
	for _, l := range head {
		budget -= len(l)
	}
	var tail []string
	for i := len(lines) - 1; i >= keepHeadTurns; i-- {
		if budget-len(lines[i]) < 0 {
			break
		}
		budget -= len(lines[i])
		tail = append([]string{lines[i]}, tail...)
	}
	omitted := len(lines) - len(head) - len(tail)
	out := append([]string{}, head...)
	out = append(out, fmt.Sprintf("_[… %d earlier turn(s) omitted to fit; the native transcript in the checkpoint has them all …]_\n", omitted))
	out = append(out, tail...)
	return strings.Join(out, "\n")
}

func renderTurn(t Turn) string {
	var sb strings.Builder
	stamp := ""
	if !t.At.IsZero() {
		stamp = " " + t.At.UTC().Format("15:04:05")
	}
	switch t.Role {
	case "user":
		fmt.Fprintf(&sb, "**User**%s:\n\n%s\n", stamp, quote(truncate(t.Text, maxTurnText)))
	default:
		fmt.Fprintf(&sb, "**Assistant**%s:\n\n", stamp)
		if text := strings.TrimSpace(t.Text); text != "" {
			sb.WriteString(truncate(text, maxTurnText))
			sb.WriteString("\n")
		}
		if len(t.Tools) > 0 {
			if strings.TrimSpace(t.Text) != "" {
				sb.WriteString("\n")
			}
			for _, tc := range t.Tools {
				fmt.Fprintf(&sb, "- `%s`", tc.Name)
				if tc.Summary != "" {
					fmt.Fprintf(&sb, ": %s", truncate(oneLine(tc.Summary), maxToolSummary))
				}
				if tc.Error {
					sb.WriteString(" _(error)_")
				}
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

func firstUserTurn(c *Conversation) *Turn {
	for i := range c.Turns {
		if c.Turns[i].Role == "user" && strings.TrimSpace(c.Turns[i].Text) != "" {
			return &c.Turns[i]
		}
	}
	return nil
}

func lastUserTurn(c *Conversation) *Turn {
	for i := len(c.Turns) - 1; i >= 0; i-- {
		if c.Turns[i].Role == "user" && strings.TrimSpace(c.Turns[i].Text) != "" {
			return &c.Turns[i]
		}
	}
	return nil
}

func lastAssistantTurn(c *Conversation) *Turn {
	for i := len(c.Turns) - 1; i >= 0; i-- {
		if c.Turns[i].Role == "assistant" && strings.TrimSpace(c.Turns[i].Text) != "" {
			return &c.Turns[i]
		}
	}
	return nil
}

func quote(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "> _(empty)_"
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut-- // do not split a UTF-8 sequence
	}
	return s[:cut] + fmt.Sprintf(" …[%d more chars]", len(s)-cut)
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func harnessTitle(h string) string {
	switch h {
	case "claude", "claude-code":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	}
	return h
}

// ToolSummary reduces a tool call's input to one descriptive line and the file it touched.
// It knows the common input shapes of all three harnesses; an unknown tool yields its
// name and nothing else, never a dump of its arguments.
func ToolSummary(name string, input map[string]any) (summary, path string) {
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := input[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	path = str("file_path", "filePath", "notebook_path", "path", "filename")
	switch strings.ToLower(name) {
	case "bash", "shell", "exec_command", "local_shell", "run_terminal_cmd", "terminal":
		if d := str("description"); d != "" {
			return d, ""
		}
		if cmd, ok := input["command"].([]any); ok { // Codex: ["bash","-lc","…"]
			parts := make([]string, 0, len(cmd))
			for _, c := range cmd {
				if s, ok := c.(string); ok {
					parts = append(parts, s)
				}
			}
			if len(parts) == 3 && (parts[0] == "bash" || parts[0] == "sh" || parts[0] == "zsh") {
				return parts[2], ""
			}
			return strings.Join(parts, " "), ""
		}
		return str("command", "cmd"), ""
	case "read", "edit", "write", "multiedit", "notebookedit", "str_replace_editor", "write_file", "edit_file", "read_file", "create_file":
		return path, path
	case "grep", "glob", "search", "codebase_search":
		return str("pattern", "query"), ""
	case "agent", "task":
		return str("description", "prompt"), ""
	case "webfetch", "websearch", "fetch":
		return str("url", "query"), ""
	case "apply_patch", "patch":
		return "apply a patch", ""
	}
	if path != "" {
		return path, path
	}
	return "", ""
}

// Fingerprint summarises content so a capture that changed nothing can be skipped.
func Fingerprint(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		var n [8]byte
		l := len(p)
		for i := 7; i >= 0; i-- {
			n[i] = byte(l)
			l >>= 8
		}
		h.Write(n[:])
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// sortedKeys is a small helper for deterministic output.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// decodeMap decodes a JSON object into a map, tolerating failure.
func decodeMap(raw json.RawMessage) map[string]any {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return map[string]any{}
	}
	return m
}
