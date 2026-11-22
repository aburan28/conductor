package transfer

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/checkpoint"
)

// Translation is the other half of portability: not moving a session between machines, but
// between harnesses. A bundle records the tool it came from; --to renders each conversation
// into the target's own on-disk format and writes a session that harness can open.
//
// Fidelity is honest and bounded: user and assistant text is preserved, each tool call
// becomes a one-line note in the assistant's reply, and no model, credentials, or live
// process state crosses over. The target opens with a short handoff preamble saying where
// the conversation came from. Target ids are derived from the source, so re-running is an
// idempotent skip rather than a duplicate.
//
//	Claude  -> synthesized ~/.claude/projects/<slug>/<uuid>.jsonl
//	Codex   -> synthesized ~/.codex/sessions/Y/M/D/rollout-<ts>-<uuid>.jsonl
//	OpenCode-> synthesized export JSON, loaded with `opencode session import`
//
// Claude and OpenCode are verified against live installs; the Codex rollout shape follows
// the one cct has used against a real Codex (and that checkpoint.ParseCodex reads back).

// TranslateOptions controls a cross-harness import.
type TranslateOptions struct {
	// Target is the harness to write into: claude, codex, or opencode.
	Target string
	// Cwd overrides the recorded working directory. OpenCode scopes sessions by it.
	Cwd string

	ClaudeHome string
	CodexHome  string

	// Sessions keeps only these source ids (unique prefixes are enough).
	Sessions []string

	DryRun bool
	// Force replaces a translated session whose bytes differ (a translator upgrade).
	Force bool
	// OpenCodeModel pins the provider/model recorded on translated OpenCode sessions.
	OpenCodeModel string
}

// TranslateItem is the per-session outcome.
type TranslateItem struct {
	BundlePath string `json:"bundle_path"`
	SourceID   string `json:"source_id,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
	DestPath   string `json:"dest_path,omitempty"`
	Action     string `json:"action"` // translated, skip-existing, conflict, skip
	Reason     string `json:"reason,omitempty"`
	Turns      int    `json:"turns,omitempty"`
}

// TranslateResult summarizes a cross-harness import.
type TranslateResult struct {
	SourceTool string          `json:"source_tool"`
	TargetTool string          `json:"target_tool"`
	DryRun     bool            `json:"dry_run"`
	Items      []TranslateItem `json:"items"`
	Warnings   []string        `json:"warnings,omitempty"`
	Translated int             `json:"translated"`
	Skipped    int             `json:"skipped"`
}

// TranslateImport converts a bundle's sessions into target, writing real sessions into that
// harness's home (or importing them through its CLI, for OpenCode).
func TranslateImport(ctx context.Context, b *Bundle, opts TranslateOptions) (TranslateResult, error) {
	source := normTool(b.Manifest.Tool)
	target := normTool(opts.Target)
	res := TranslateResult{SourceTool: source, TargetTool: target, DryRun: opts.DryRun}
	switch {
	case target == "":
		return res, fmt.Errorf("--to needs claude, codex, or opencode")
	case source == "":
		return res, fmt.Errorf("bundle names no tool")
	case source == target:
		return res, fmt.Errorf("bundle already holds %s sessions; use a plain import (no --to) instead", source)
	}

	var home string
	switch target {
	case ToolClaude:
		h, err := ClaudeHome(opts.ClaudeHome)
		if err != nil {
			return res, err
		}
		home = h
	case ToolCodex:
		h, err := CodexHome(opts.CodexHome)
		if err != nil {
			return res, err
		}
		home = h
	}
	if target != ToolOpenCode {
		if err := os.MkdirAll(home, 0o700); err != nil {
			return res, err
		}
	}

	for _, s := range b.Manifest.Sessions {
		if !matchesAnyID(opts.Sessions, s.ThreadID) {
			res.Items = append(res.Items, TranslateItem{BundlePath: s.BundlePath, Action: "skip", Reason: "not selected"})
			res.Skipped++
			continue
		}
		item, err := translateOne(ctx, b, s, source, target, home, opts)
		if err != nil {
			return res, err
		}
		switch item.Action {
		case "translated":
			res.Translated++
		default:
			res.Skipped++
		}
		res.Items = append(res.Items, item)
	}
	if res.Items == nil {
		res.Items = []TranslateItem{}
	}
	return res, nil
}

func translateOne(ctx context.Context, b *Bundle, s Session, source, target, home string, opts TranslateOptions) (TranslateItem, error) {
	item := TranslateItem{BundlePath: s.BundlePath, SourceID: s.ThreadID}
	data, err := b.Read(s.BundlePath)
	if err != nil {
		return item, err
	}
	conv := conversationFor(source, data)
	if conv == nil || len(conv.Turns) == 0 {
		item.Action = "skip"
		item.Reason = "no recoverable conversation in this session"
		return item, nil
	}
	cwd := opts.Cwd
	if cwd == "" {
		cwd = conv.Cwd
	}
	if cwd == "" {
		cwd = s.OriginalCWD
	}
	if cwd == "" {
		item.Action = "skip"
		item.Reason = "the session records no working directory; pass --cwd"
		return item, nil
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return item, err
	}
	cwd = abs
	if target != ToolOpenCode {
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			item.Action = "skip"
			item.Reason = fmt.Sprintf("working directory %s does not exist here; pass --cwd", cwd)
			return item, nil
		}
	}

	sourceKey := s.ThreadID
	if sourceKey == "" {
		sourceKey = conv.SessionID
	}
	if sourceKey == "" {
		sourceKey = s.BundlePath
	}
	targetID := deterministicUUID(source + ":" + sourceKey)
	item.Turns = len(conv.Turns)
	item.TargetID = targetID

	base := conv.Started
	if base.IsZero() {
		base = time.Now().UTC()
	}
	preamble := handoffPreamble(source, sourceKey, target)

	switch target {
	case ToolClaude:
		content := claudeTranscript(targetID, conv, cwd, preamble)
		rel := path.Join("projects", claudeSlug(cwd), targetID+".jsonl")
		item.DestPath = filepath.Join(home, filepath.FromSlash(rel))
		return finishTranslation(item, home, rel, content, opts)
	case ToolCodex:
		content := codexTranscript(targetID, conv, cwd, preamble, base)
		rel := path.Join("sessions", base.Format("2006"), base.Format("01"), base.Format("02"),
			"rollout-"+base.Format("2006-01-02T15-04-05")+"-"+targetID+".jsonl")
		item.DestPath = filepath.Join(home, filepath.FromSlash(rel))
		return finishTranslation(item, home, rel, content, opts)
	case ToolOpenCode:
		ocID, content, err := openCodeTranscript(ctx, conv, cwd, preamble, base, targetID, opts.OpenCodeModel)
		if err != nil {
			item.Action = "skip"
			item.Reason = err.Error()
			return item, nil
		}
		item.TargetID = ocID
		item.DestPath = "opencode:" + ocID + " (" + cwd + ")"
		if opts.DryRun {
			item.Action = "translated"
			return item, nil
		}
		got, err := importOpenCode(ctx, cwd, content)
		if errors.Is(err, errAlreadyExists) {
			item.Action = "skip-existing"
			item.Reason = "already present in OpenCode"
			return item, nil
		}
		if err != nil {
			return item, err
		}
		if got != "" {
			item.TargetID = got
			item.DestPath = "opencode:" + got + " (" + cwd + ")"
		}
		item.Action = "translated"
		return item, nil
	default:
		return item, fmt.Errorf("unknown target %q", target)
	}
}

func finishTranslation(item TranslateItem, home, rel string, content []byte, opts TranslateOptions) (TranslateItem, error) {
	if opts.DryRun {
		item.Action = "translated"
		return item, nil
	}
	local, exists, err := readInside(home, rel)
	if err != nil {
		return item, err
	}
	if exists {
		if bytes.Equal(local, content) {
			item.Action = "skip-existing"
			item.Reason = "already translated"
			return item, nil
		}
		if !opts.Force {
			item.Action = "conflict"
			item.Reason = "a translated session already exists with different bytes; pass --force to replace it"
			return item, nil
		}
	}
	if err := writeInside(home, rel, content, 0o600, 0o700); err != nil {
		return item, err
	}
	item.Action = "translated"
	return item, nil
}

// conversationFor parses a source transcript into the harness-neutral conversation the
// checkpoint package already defines. A session that cannot be parsed yields nil.
func conversationFor(tool string, data []byte) *checkpoint.Conversation {
	switch tool {
	case ToolClaude:
		return checkpoint.ParseClaude(data)
	case ToolCodex:
		return checkpoint.ParseCodex(data)
	case ToolOpenCode:
		return checkpoint.ParseOpenCode(data)
	default:
		return nil
	}
}

// handoffPreamble is the first thing the target harness reads. It says what was translated
// and what did not survive, so the model does not assume it can see the original tools.
func handoffPreamble(source, sourceID, target string) string {
	id := sourceID
	if id == "" {
		id = "unknown id"
	}
	return fmt.Sprintf("[conductor transfer] Translated from a %s session (%s) into %s. "+
		"The conversation below is a text replay: user and assistant messages are preserved, "+
		"tool calls are reduced to one-line notes, and model or runtime state did not transfer. "+
		"Continue the work from here.", source, id, target)
}

// turnsWithPreamble puts the handoff note in front of the conversation. If the first turn is
// the assistant's, a user turn is inserted so the preamble has somewhere to live.
func turnsWithPreamble(conv *checkpoint.Conversation, preamble string) []checkpoint.Turn {
	out := []checkpoint.Turn{{Role: "user", At: conv.Started, Text: preamble}}
	for _, t := range conv.Turns {
		if strings.TrimSpace(renderTurnText(t)) == "" {
			continue
		}
		out = append(out, t)
	}
	if len(out) > 1 && out[1].Role == "user" {
		// Merge the note into the first user message, so the target does not show two
		// consecutive user turns at the top.
		out[1].Text = preamble + "\n\n---\n\n" + out[1].Text
		out = out[1:]
	}
	return out
}

// renderTurnText flattens a turn to the text a target can replay: the assistant's prose plus
// one bracketed line per tool call.
func renderTurnText(t checkpoint.Turn) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(t.Text))
	for _, call := range t.Tools {
		note := strings.TrimSpace(strings.Join([]string{call.Name, call.Summary}, " — "))
		if note == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("[tool: ")
		b.WriteString(note)
		if call.Path != "" {
			b.WriteString(" (")
			b.WriteString(call.Path)
			b.WriteString(")")
		}
		if call.Error {
			b.WriteString(" [error]")
		}
		b.WriteString("]")
	}
	return strings.TrimSpace(b.String())
}

// claudeTranscript synthesizes the JSONL Claude Code reads from projects/<slug>/.
func claudeTranscript(sessionID string, conv *checkpoint.Conversation, cwd, preamble string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	base := conv.Started
	if base.IsZero() {
		base = time.Now().UTC()
	}
	parent := ""
	for i, t := range turnsWithPreamble(conv, preamble) {
		uuid := deterministicUUID(sessionID + ":line:" + fmt.Sprint(i))
		line := map[string]any{
			"type":        t.Role,
			"uuid":        uuid,
			"sessionId":   sessionID,
			"cwd":         cwd,
			"version":     "conductor-transfer",
			"timestamp":   base.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			"userType":    "external",
			"isSidechain": false,
			"entrypoint":  "conductor",
			"message":     claudeMessage(t),
		}
		if parent == "" {
			line["parentUuid"] = nil
		} else {
			line["parentUuid"] = parent
		}
		_ = enc.Encode(line)
		parent = uuid
	}
	return buf.Bytes()
}

func claudeMessage(t checkpoint.Turn) map[string]any {
	text := renderTurnText(t)
	if t.Role == "user" {
		return map[string]any{"role": "user", "content": text}
	}
	return map[string]any{
		"role":    "assistant",
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

// codexTranscript synthesizes a rollout: session_meta, then each turn as the durable
// response_item Codex replays on resume and the event_msg its transcript view uses.
func codexTranscript(sessionID string, conv *checkpoint.Conversation, cwd, preamble string, base time.Time) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	ts := func(i int) string { return base.Add(time.Duration(i) * time.Second).Format(time.RFC3339) }
	_ = enc.Encode(rolloutLine(ts(0), "session_meta", map[string]any{
		"id":          sessionID,
		"timestamp":   ts(0),
		"cwd":         cwd,
		"originator":  "conductor",
		"cli_version": "conductor-transfer",
		"source":      "cli",
	}))
	for i, t := range turnsWithPreamble(conv, preamble) {
		text := renderTurnText(t)
		at := ts(i + 1)
		switch t.Role {
		case "user":
			_ = enc.Encode(rolloutLine(at, "event_msg", map[string]any{"type": "user_message", "message": text}))
			_ = enc.Encode(rolloutLine(at, "response_item", codexMessage("user", "input_text", text)))
		default:
			_ = enc.Encode(rolloutLine(at, "event_msg", map[string]any{"type": "agent_message", "message": text}))
			_ = enc.Encode(rolloutLine(at, "response_item", codexMessage("assistant", "output_text", text)))
		}
	}
	return buf.Bytes()
}

func rolloutLine(ts, typ string, payload map[string]any) map[string]any {
	return map[string]any{"timestamp": ts, "type": typ, "payload": payload}
}

func codexMessage(role, blockType, text string) map[string]any {
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": []map[string]any{{"type": blockType, "text": text}},
	}
}

// openCodeTranscript builds a v2 export document and the id it will have. The shape was
// checked against a live OpenCode 2.x: import validates the model id, so a model the
// installation knows is recorded on every assistant message.
func openCodeTranscript(ctx context.Context, conv *checkpoint.Conversation, cwd, preamble string, base time.Time, seed, modelOverride string) (string, []byte, error) {
	providerID, modelID := "", ""
	if modelOverride != "" {
		p, m, err := splitModel(modelOverride)
		if err != nil {
			return "", nil, err
		}
		providerID, modelID = p, m
	} else {
		p, m, err := openCodeModel(ctx, cwd)
		if err != nil {
			return "", nil, err
		}
		providerID, modelID = p, m
	}
	sessionID := deterministicOpenCodeID("ses", seed)
	turns := turnsWithPreamble(conv, preamble)
	created := millis(base)
	updated := created + int64(len(turns)+1)*1000

	model := map[string]any{"id": modelID, "providerID": providerID, "variant": ""}
	// OpenCode validates the token object's shape, including cache.read/write, even for a
	// session that never ran in it.
	zeroTokens := map[string]any{"input": 0, "output": 0, "reasoning": 0,
		"cache": map[string]any{"read": 0, "write": 0}}
	// projectID is required by the importer. OpenCode re-derives its own project from
	// location.directory, so a stable value derived from the directory is enough.
	projectSum := sha1.Sum([]byte(cwd))

	var messages []map[string]any
	for i, t := range turns {
		at := created + int64(i)*1000
		text := renderTurnText(t)
		if t.Role == "user" {
			messages = append(messages, map[string]any{
				"id":     deterministicOpenCodeID("msg", fmt.Sprintf("%s:%d", seed, i)),
				"time":   map[string]any{"created": at},
				"text":   text,
				"files":  []any{},
				"agents": []any{},
				"type":   "user",
			})
			continue
		}
		messages = append(messages, map[string]any{
			"id":        deterministicOpenCodeID("msg", fmt.Sprintf("%s:%d", seed, i)),
			"time":      map[string]any{"created": at, "streamed": at + 500, "completed": at + 900},
			"type":      "assistant",
			"agent":     "build",
			"model":     model,
			"content":   []map[string]any{{"type": "text", "text": text}},
			"finish":    "stop",
			"rawFinish": "stop",
			"cost":      0,
			"tokens":    zeroTokens,
		})
	}
	messages = append(messages, map[string]any{
		"id":      deterministicOpenCodeID("msg", fmt.Sprintf("%s:idle", seed)),
		"time":    map[string]any{"created": updated},
		"type":    "idle",
		"outcome": "succeeded",
	})

	doc := map[string]any{
		"info": map[string]any{
			"id":        sessionID,
			"projectID": hex.EncodeToString(projectSum[:]),
			"agent":     "build",
			"model":     model,
			"cost":      0,
			"tokens":    zeroTokens,
			"outcome":   "succeeded",
			"time":      map[string]any{"created": created, "updated": updated},
			"title":     openCodeTitle(conv),
			"location": map[string]any{
				"directory": cwd,
			},
		},
		"messages": messages,
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return "", nil, err
	}
	return sessionID, out, nil
}

func openCodeTitle(conv *checkpoint.Conversation) string {
	if strings.TrimSpace(conv.Title) != "" {
		return conv.Title
	}
	for _, t := range conv.Turns {
		if t.Role == "user" {
			s := strings.TrimSpace(t.Text)
			if len(s) > 60 {
				s = s[:60]
			}
			if s != "" {
				return s
			}
		}
	}
	return "Translated session"
}

// deterministicUUID derives a stable, uuid-shaped id from a key. Claude and Codex both use
// ids that look like UUIDs, and stability is what makes re-import a skip.
func deterministicUUID(key string) string {
	sum := sha256.Sum256([]byte("conductor-transfer/uuid/" + key))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// deterministicOpenCodeID derives an OpenCode-shaped id (ses_/msg_ plus 26 base62 chars).
func deterministicOpenCodeID(prefix, key string) string {
	sum := sha256.Sum256([]byte("conductor-transfer/" + prefix + "/" + key))
	n := new(big.Int).SetBytes(sum[:])
	mod := big.NewInt(int64(len(base62)))
	var out [26]byte
	for i := 25; i >= 0; i-- {
		q, r := new(big.Int).QuoRem(n, mod, new(big.Int))
		out[i] = base62[r.Int64()]
		n = q
	}
	return prefix + "_" + string(out[:])
}
