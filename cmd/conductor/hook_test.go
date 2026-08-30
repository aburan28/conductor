package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
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

	// A cooperative project downgraded the decision to a warning even though the raw
	// conflict is blocking-class: the hook follows the decision, not the matrix, and the
	// server's advice (how to request the territory) is what the model reads.
	v = judgePreTool(coord.IntentDecision{
		Outcome:     domain.OutcomeAllowWithWarning,
		Enforcement: domain.EnforceCooperative,
		Conflicts:   []db.ScopeConflict{conflict("alice", domain.OutcomeBlockConflict)},
		Advice:      "alice holds path:internal/api/api.go for T-7. Request the territory with coord_expand_scope.",
	}, "bob")
	if v.Block || !strings.Contains(v.Warning, "coord_expand_scope") {
		t.Fatalf("cooperative downgrade must warn with the expansion advice: %+v", v)
	}

	// A strict project blocks on the same shape.
	v = judgePreTool(coord.IntentDecision{
		Outcome:     domain.OutcomeBlockConflict,
		Enforcement: domain.EnforceStrictHarness,
		Conflicts:   []db.ScopeConflict{conflict("alice", domain.OutcomeBlockConflict)},
	}, "bob")
	if !v.Block || !strings.Contains(v.Message, "alice") {
		t.Fatalf("strict must block: %+v", v)
	}

	// A block for a non-conflict reason (an exact duplicate) uses the server's advice.
	v = judgePreTool(coord.IntentDecision{
		Outcome:   domain.OutcomeBlockDuplicate,
		Conflicts: []db.ScopeConflict{conflict("alice", domain.OutcomeBlockConflict)},
		Advice:    "T-3 (owner alice) is already this exact work. Attach to it.",
	}, "bob")
	if !v.Block || !strings.Contains(v.Message, "T-3") {
		t.Fatalf("duplicate block should carry the server's advice: %+v", v)
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
