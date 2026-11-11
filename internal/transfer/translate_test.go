package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/checkpoint"
)

// claudeBundle exports one fake Claude conversation and returns the opened bundle.
func claudeBundle(t *testing.T, body string) *Bundle {
	t.Helper()
	home := fakeClaudeHome(t, project, struct{ id, text string }{sessionA, body})
	var buf bytes.Buffer
	if _, err := Export(&buf, ExportOptions{Tool: ToolClaude, ClaudeHome: home, Project: project}); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTranslateClaudeToCodex(t *testing.T) {
	b := claudeBundle(t, "the answer")
	dst := t.TempDir()
	work := t.TempDir()
	res, err := TranslateImport(context.Background(), b, TranslateOptions{Target: ToolCodex, CodexHome: dst, Cwd: work})
	if err != nil {
		t.Fatal(err)
	}
	if res.Translated != 1 || res.SourceTool != ToolClaude || res.TargetTool != ToolCodex {
		t.Fatalf("result = %+v", res)
	}
	var file string
	_ = filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			file = p
		}
		return nil
	})
	if file == "" {
		t.Fatal("no Codex rollout was written")
	}
	if !strings.Contains(file, filepath.Join("sessions")) || !strings.Contains(filepath.Base(file), res.Items[0].TargetID) {
		t.Errorf("rollout path = %s", file)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	conv := checkpoint.ParseCodex(data)
	if conv.SessionID != res.Items[0].TargetID {
		t.Errorf("session id = %q, want %q", conv.SessionID, res.Items[0].TargetID)
	}
	if conv.Cwd != work {
		t.Errorf("cwd = %q, want %q", conv.Cwd, work)
	}
	if len(conv.Turns) != 2 {
		t.Fatalf("turns = %d, want the preamble merged into the user turn plus the assistant turn", len(conv.Turns))
	}
	if !strings.Contains(conv.Turns[0].Text, "Translated from a claude session") {
		t.Errorf("missing handoff preamble: %q", conv.Turns[0].Text)
	}
	if !strings.Contains(conv.Turns[len(conv.Turns)-1].Text, "the answer") {
		t.Errorf("last turn = %q", conv.Turns[len(conv.Turns)-1].Text)
	}
}

func codexBundle(t *testing.T) *Bundle {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "01")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"
	body := `{"timestamp":"2026-10-01T10:00:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"` + project + `","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-10-01T10:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"codex question"}}` + "\n" +
		`{"timestamp":"2026-10-01T10:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"codex answer"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-01T10-00-00-"+id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := Export(&buf, ExportOptions{Tool: ToolCodex, CodexHome: home, Project: project}); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTranslateCodexToClaude(t *testing.T) {
	b := codexBundle(t)
	dst := t.TempDir()
	work := t.TempDir()
	res, err := TranslateImport(context.Background(), b, TranslateOptions{Target: ToolClaude, ClaudeHome: dst, Cwd: work})
	if err != nil {
		t.Fatal(err)
	}
	if res.Translated != 1 {
		t.Fatalf("result = %+v", res)
	}
	file := res.Items[0].DestPath
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("translated transcript missing: %v", err)
	}
	conv := checkpoint.ParseClaude(data)
	if conv.SessionID != res.Items[0].TargetID {
		t.Errorf("session id = %q, want %q", conv.SessionID, res.Items[0].TargetID)
	}
	if conv.Cwd != work {
		t.Errorf("cwd = %q, want %q", conv.Cwd, work)
	}
	var texts []string
	for _, turn := range conv.Turns {
		texts = append(texts, turn.Text)
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "codex question") || !strings.Contains(joined, "codex answer") {
		t.Errorf("conversation not carried: %q", joined)
	}
}

func TestTranslateIsIdempotent(t *testing.T) {
	b := claudeBundle(t, "once")
	dst := t.TempDir()
	work := t.TempDir()
	opts := TranslateOptions{Target: ToolCodex, CodexHome: dst, Cwd: work}
	first, err := TranslateImport(context.Background(), b, opts)
	if err != nil || first.Translated != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	data1, err := os.ReadFile(first.Items[0].DestPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := TranslateImport(context.Background(), b, opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Translated != 0 || second.Items[0].Action != "skip-existing" {
		t.Fatalf("second = %+v", second)
	}
	data2, err := os.ReadFile(first.Items[0].DestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data1, data2) {
		t.Fatal("a re-run changed the translated transcript")
	}
}

func TestOpenCodeTranscriptShape(t *testing.T) {
	conv := &checkpoint.Conversation{
		Cwd:   "/tmp/x",
		Title: "shape test",
		Turns: []checkpoint.Turn{
			{Role: "user", Text: "hello"},
			{Role: "assistant", Text: "hi", Tools: []checkpoint.ToolCall{{Name: "shell", Summary: "ls -la"}}},
		},
	}
	id, data, err := openCodeTranscript(context.Background(), conv, "/tmp/x", "preamble", time.Unix(1700000000, 0).UTC(), "seed", "anthropic/claude-x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "ses_") || len(id) != 30 {
		t.Fatalf("id = %q, want ses_ + 26 chars", id)
	}
	var doc struct {
		Info struct {
			ID       string `json:"id"`
			Location struct {
				Directory string `json:"directory"`
			} `json:"location"`
			Model struct {
				ID         string `json:"id"`
				ProviderID string `json:"providerID"`
			} `json:"model"`
		} `json:"info"`
		Messages []struct {
			Type    string `json:"type"`
			Finish  string `json:"finish"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Text string `json:"text"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Info.ID != id || doc.Info.Location.Directory != "/tmp/x" {
		t.Errorf("info = %+v", doc.Info)
	}
	if doc.Info.Model.ProviderID != "anthropic" || doc.Info.Model.ID != "claude-x" {
		t.Errorf("model = %+v", doc.Info.Model)
	}
	if len(doc.Messages) < 3 {
		t.Fatalf("messages = %d", len(doc.Messages))
	}
	if doc.Messages[0].Type != "user" || doc.Messages[1].Type != "assistant" || doc.Messages[1].Finish != "stop" {
		t.Errorf("message types = %s/%s finish=%s", doc.Messages[0].Type, doc.Messages[1].Type, doc.Messages[1].Finish)
	}
	if !strings.Contains(doc.Messages[1].Content[0].Text, "[tool: shell") {
		t.Errorf("tool note missing: %q", doc.Messages[1].Content[0].Text)
	}
	if doc.Messages[len(doc.Messages)-1].Type != "idle" {
		t.Errorf("last message = %s, want idle", doc.Messages[len(doc.Messages)-1].Type)
	}
}

func TestTranslateToOpenCodeUsesCLI(t *testing.T) {
	old := openCodeExec
	defer func() { openCodeExec = old }()
	var calls []string
	openCodeExec = func(ctx context.Context, cwd string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch {
		case len(args) > 0 && args[0] == "models":
			return []byte("anthropic/claude-x\n"), nil
		case len(args) > 1 && args[0] == "session" && args[1] == "import":
			return []byte("Imported session: ses_ABCDEFGHIJKLMNOPQRSTUVWXYZ\n"), nil
		}
		return nil, os.ErrNotExist
	}
	b := claudeBundle(t, "to opencode")
	work := t.TempDir()
	res, err := TranslateImport(context.Background(), b, TranslateOptions{Target: ToolOpenCode, Cwd: work})
	if err != nil {
		t.Fatal(err)
	}
	if res.Translated != 1 || res.Items[0].TargetID != "ses_ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		t.Fatalf("result = %+v", res)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "session import") || !strings.Contains(joined, "--directory") {
		t.Errorf("opencode calls = %q", joined)
	}
}

func TestExportOpenCodeBundle(t *testing.T) {
	old := openCodeExec
	defer func() { openCodeExec = old }()
	id := "ses_0123456789abcdefghijklmnop"
	proj := t.TempDir()
	sample := `{"info":{"id":"` + id + `","title":"oc title","directory":"` + proj + `","time":{"created":1760000000000,"updated":1760000100000}},"messages":[{"type":"user","id":"msg_1","time":{"created":1760000000000},"text":"oc question"},{"type":"assistant","id":"msg_2","time":{"created":1760000100000},"content":[{"type":"text","text":"oc answer"}],"finish":"stop"}]}`
	openCodeExec = func(ctx context.Context, cwd string, args ...string) ([]byte, error) {
		switch {
		case len(args) > 1 && args[0] == "session" && args[1] == "list":
			return []byte(`[{"id":"` + id + `","title":"oc title","directory":"` + proj + `","updated":1760000100000}]`), nil
		case len(args) > 1 && args[0] == "session" && args[1] == "export":
			return []byte(sample), nil
		}
		return nil, os.ErrNotExist
	}
	var buf bytes.Buffer
	rep, err := Export(&buf, ExportOptions{Tool: ToolOpenCode, Project: proj})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sessions != 1 {
		t.Fatalf("sessions = %d", rep.Sessions)
	}
	b, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if b.Manifest.Tool != ToolOpenCode {
		t.Errorf("tool = %q", b.Manifest.Tool)
	}
	if len(b.Manifest.Sessions) != 1 || b.Manifest.Sessions[0].BundlePath != "opencode/"+id+".json" {
		t.Fatalf("manifest = %+v", b.Manifest.Sessions)
	}
	conv := checkpoint.ParseOpenCode([]byte(sample))
	if len(conv.Turns) != 2 || conv.Turns[0].Text != "oc question" || conv.Turns[1].Text != "oc answer" {
		t.Errorf("v2 export did not parse: %+v", conv.Turns)
	}
}

func TestTranslateRefusesSameTool(t *testing.T) {
	b := claudeBundle(t, "x")
	_, err := TranslateImport(context.Background(), b, TranslateOptions{Target: ToolClaude, ClaudeHome: t.TempDir(), Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error translating claude to claude")
	}
}
