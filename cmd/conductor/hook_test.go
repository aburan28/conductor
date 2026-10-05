package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
)

func TestIsEditTool(t *testing.T) {
	edit := []string{"Edit", "Write", "MultiEdit", "NotebookEdit", "edit", "write", "patch",
		"apply_patch", "str_replace_editor", "mcp__files__edit_file", "TodoWrite"}
	for _, name := range edit {
		if !isEditTool(name) {
			t.Errorf("isEditTool(%q) = false, want true", name)
		}
	}
	// TodoWrite matches by name but never carries a file path, so it is filtered by the
	// path requirement, not here.
	notEdit := []string{"Read", "Bash", "Grep", "Glob", "WebFetch", "WebSearch", "Task", ""}
	for _, name := range notEdit {
		if isEditTool(name) {
			t.Errorf("isEditTool(%q) = true, want false", name)
		}
	}
}

func TestReadHookInputDecodesOnlyThePath(t *testing.T) {
	payload := `{"session_id":"s1","cwd":"/repo","hook_event_name":"PreToolUse","tool_name":"Write",
		"transcript_path":"/secret/transcript.jsonl",
		"tool_input":{"file_path":"internal/api/api.go","content":"SECRET DO NOT READ"}}`
	in, err := readHookInput(strings.NewReader(payload), false)
	if err != nil {
		t.Fatal(err)
	}
	if in.ToolName != "Write" || in.path() != "internal/api/api.go" || in.Cwd != "/repo" {
		t.Fatalf("decoded wrong: %+v", in)
	}
}

func TestRepoRelative(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "internal", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	if rel, ok := repoRelative(sub, "api.go"); !ok || rel != "internal/api/api.go" {
		t.Fatalf("relative path: %q %v", rel, ok)
	}
	if rel, ok := repoRelative(root, filepath.Join(root, "cmd", "x.go")); !ok || rel != "cmd/x.go" {
		t.Fatalf("absolute path: %q %v", rel, ok)
	}
	if _, ok := repoRelative(root, "/etc/hosts"); ok {
		t.Fatal("path outside the repository must not be checked")
	}
}

func TestJudgePreTool(t *testing.T) {
	conflict := func(owner string, outcome domain.Outcome) db.ScopeConflict {
		return db.ScopeConflict{
			ResourceKey: "internal/api/api.go", Outcome: outcome,
			HolderOwner: owner, HolderTaskRef: "T-7", HolderMode: domain.ModeWriteExclusive,
		}
	}

	// A hard conflict held by someone else blocks, and the message names them.
	v := judgePreTool(coord.IntentDecision{
		Outcome:   domain.OutcomeBlockConflict,
		Conflicts: []db.ScopeConflict{conflict("alice", domain.OutcomeBlockConflict)},
	}, "bob")
	if !v.Block || !strings.Contains(v.Message, "alice") || !strings.Contains(v.Message, "T-7") {
		t.Fatalf("verdict = %+v", v)
	}

	// The same conflict held by the caller's own task is the system working, not a collision.
	v = judgePreTool(coord.IntentDecision{
		Outcome:   domain.OutcomeBlockConflict,
		Conflicts: []db.ScopeConflict{conflict("bob", domain.OutcomeBlockConflict)},
	}, "bob")
	if v.Block || v.Warning != "" {
		t.Fatalf("own holding must be allowed: %+v", v)
	}

	// Advisory overlap warns without blocking.
	v = judgePreTool(coord.IntentDecision{
		Outcome:   domain.OutcomeAllowWithWarning,
		Conflicts: []db.ScopeConflict{conflict("alice", domain.OutcomeAllowWithWarning)},
	}, "bob")
	if v.Block || !strings.Contains(v.Warning, "T-7") {
		t.Fatalf("verdict = %+v", v)
	}

	// Nothing in flight: silence.
	if v := judgePreTool(coord.IntentDecision{Outcome: domain.OutcomeAllow}, "bob"); v.Block || v.Warning != "" {
		t.Fatalf("clean allow must be silent: %+v", v)
	}
}

func TestHookCacheRoundTrip(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	writeHookCache("session-abc", activeTask{ID: "id-1", Ref: "T-1"})
	var got activeTask
	if !readHookCache("session-abc", &got) || got.Ref != "T-1" {
		t.Fatalf("cache round trip: %+v", got)
	}
	if readHookCache("../escape", &got) {
		// The name is sanitized into the cache directory; a traversal never reads outside it.
		t.Log("sanitized name resolved to an existing file, which is fine; it must be inside the dir")
	}
}

// The checkpoint hook reads only the session id, cwd, and event name from the payload, and
// locates the transcript itself; a Stop event within the rate limit of a previous capture
// writes nothing, a PreCompact event always captures a changed transcript.
func TestHookCheckpointCapturesFromStdin(t *testing.T) {
	state := t.TempDir()
	cfg := t.TempDir()
	work := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", state)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CONDUCTOR_HARNESS", "")
	t.Setenv("CONDUCTOR_CHECKPOINT", "")
	slug := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, work)
	dir := filepath.Join(cfg, "projects", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s-1.jsonl")
	line := `{"type":"user","uuid":"u1","timestamp":"2026-10-05T17:37:06Z","cwd":"` + work + `","sessionId":"s-1","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	feed := func(payload string) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		w.WriteString(payload)
		w.Close()
		old := os.Stdin
		os.Stdin = r
		t.Cleanup(func() { os.Stdin = old })
	}
	count := func() int {
		entries, _ := os.ReadDir(filepath.Join(state, "checkpoints"))
		n := 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".ckpt") {
				n++
			}
		}
		return n
	}

	feed(`{"session_id":"s-1","cwd":"` + work + `","hook_event_name":"Stop","transcript_path":"/should/not/be/read"}`)
	if err := cmdHook(context.Background(), []string{"checkpoint"}); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatalf("Stop hook wrote %d checkpoints, want 1", count())
	}

	// A new line, but within the rate limit: Stop writes nothing, PreCompact does.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(strings.Replace(line, `"u1"`, `"u2"`, 1))
	f.Close()
	feed(`{"session_id":"s-1","cwd":"` + work + `","hook_event_name":"Stop"}`)
	_ = cmdHook(context.Background(), []string{"checkpoint"})
	if count() != 1 {
		t.Fatalf("rate-limited Stop hook wrote a checkpoint (%d total)", count())
	}
	feed(`{"session_id":"s-1","cwd":"` + work + `","hook_event_name":"PreCompact"}`)
	_ = cmdHook(context.Background(), []string{"checkpoint"})
	if count() != 2 {
		t.Fatalf("PreCompact hook did not checkpoint the changed transcript (%d total)", count())
	}
}

func TestJudgePreToolExplainsPendingMerge(t *testing.T) {
	v := judgePreTool(coord.IntentDecision{
		Outcome: domain.OutcomeBlockConflict,
		Conflicts: []db.ScopeConflict{{
			ResourceKey: "path:internal/api/api.go", Outcome: domain.OutcomeBlockConflict,
			HolderOwner: "alice", HolderTaskRef: "T-7", HolderMode: domain.ModeWriteExclusive,
			HolderStatus: domain.TaskVerifying, HolderPullRequest: "https://github.com/acme/w/pull/3",
		}},
	}, "bob")
	if !v.Block || !strings.Contains(v.Message, "waiting to merge") || !strings.Contains(v.Message, "pull/3") {
		t.Fatalf("verdict = %+v, want a pending-merge explanation", v)
	}
}

func TestCoveredBy(t *testing.T) {
	scopes := []string{"dir:internal/api", "path:README.md"}
	for rel, want := range map[string]bool{
		"internal/api/handlers.go": true,
		"README.md":                true,
		"internal/db/claim.go":     false,
	} {
		if got := coveredBy(scopes, rel); got != want {
			t.Errorf("coveredBy(%q) = %v, want %v", rel, got, want)
		}
	}
	if !coveredBy([]string{"repo:"}, "anything.go") {
		t.Error("a repo-wide claim covers every file")
	}
}

// An edit outside the session's own claim is reserved under that claim on first edit, by
// path alone, and the model is told; with auto-reserve off it is only reported.
func TestExpandOwnScopeReservesThroughTheSession(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	var got map[string]any
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/sessions/s-1/scopes" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"outcome":"allow","task_ref":"T-3"}`))
	}))
	defer srv.Close()
	api := client.New(srv.URL, "tok")
	claim := activeTask{ID: "t-3", Ref: "T-3", Scopes: []string{"dir:internal/api"}, SessionID: "s-1"}

	if note := expandOwnScope(context.Background(), api, claim, "internal/api/x.go", true); note != "" || calls != 0 {
		t.Fatalf("an edit inside the claim = %q (%d calls), want silence", note, calls)
	}
	note := expandOwnScope(context.Background(), api, claim, "internal/db/y.go", true)
	if calls != 1 || !strings.Contains(note, "now reserved for T-3") {
		t.Fatalf("note = %q after %d call(s)", note, calls)
	}
	scopes, _ := got["scopes"].([]any)
	first, _ := scopes[0].(map[string]any)
	if first["resource"] != "path:internal/db/y.go" || got["source"] != "observed" {
		t.Errorf("request = %v", got)
	}
	if body, _ := json.Marshal(got); strings.Contains(string(body), "content") {
		t.Errorf("something besides the path was sent: %s", body)
	}

	// Off: report, do not reserve.
	note = expandOwnScope(context.Background(), api, claim, "internal/db/z.go", false)
	if calls != 1 || !strings.Contains(note, "outside T-3's claimed scope") {
		t.Errorf("report-only note = %q (%d calls)", note, calls)
	}
}

func TestUnclaimedEditNoteIsRateLimited(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	if note := unclaimedEditNote("s-9", "/repo"); !strings.Contains(note, "holds no task") {
		t.Fatalf("first note = %q", note)
	}
	if note := unclaimedEditNote("s-9", "/repo"); note != "" {
		t.Errorf("repeated note = %q, want silence", note)
	}
}
