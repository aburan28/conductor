package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aburan28/conductor/internal/checkpoint"
)

func TestCheckpointMigrationCaptureIsExactAndIdempotent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", filepath.Join(root, "conductor"))
	configDir := filepath.Join(root, "claude")
	project := filepath.Join(configDir, "projects", "ssh-source")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "source-1.jsonl")
	write := func(extra string) {
		t.Helper()
		body := `{"type":"user","sessionId":"source-1","cwd":"/remote/repo","timestamp":"2026-10-16T18:00:00Z","message":{"role":"user","content":"fix the docs"}}` + "\n" +
			`{"type":"assistant","sessionId":"source-1","cwd":"/remote/repo","timestamp":"2026-10-16T18:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}` + "\n" + extra
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("")
	agent := filepath.Join(project, "agent-child.jsonl")
	if err := os.WriteFile(agent, []byte(`{"type":"assistant","sessionId":"source-1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := checkpoint.InventoryClaude(configDir, "source-1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("inventory: %+v, %v", rows, err)
	}
	if err := checkpointMigrate(context.Background(), []string{"--source-state-dir", configDir,
		"--session", "source-1", "--transcript-only", "--execute", "--codex-home", filepath.Join(root, "missing")}); err == nil {
		t.Fatal("migration accepted a missing Codex home")
	}
	first, err := migrateClaudeRow(context.Background(), rows[0], root, true, true, nil)
	if err != nil || first.State != "checkpointed" || first.Source.SessionID != "source-1" ||
		first.TargetThreadID != "" || first.CheckpointSHA == "" {
		t.Fatalf("first capture: %+v, %v", first, err)
	}
	bundle, err := checkpoint.OpenFile(first.CheckpointPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	native, ok := bundle.File("native/claude/source-1.jsonl")
	if !ok {
		t.Fatal("exact native transcript is absent from checkpoint")
	}
	source, _ := os.ReadFile(path)
	if string(native) != string(source) {
		t.Fatal("checkpoint did not preserve the exact source bytes")
	}
	if _, ok := bundle.File("native/claude/source-1/agents/agent-child.jsonl"); !ok {
		t.Fatal("top-level Claude subagent transcript was omitted")
	}
	again, err := migrateClaudeRow(context.Background(), rows[0], root, true, true, nil)
	if err != nil || again.CheckpointID != first.CheckpointID {
		t.Fatalf("same snapshot was captured twice: %+v, %v", again, err)
	}
	// A batch capture can encounter a snapshot already moved to Codex with a
	// different target directory. It must reuse that result without starting a
	// second thread or misreporting the source as a failed checkpoint.
	receiptFile, err := migrationReceiptPath(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	again.State, again.Error = "failed", "Codex temporarily unavailable"
	if err := saveMigrationReceipt(receiptFile, again); err != nil {
		t.Fatal(err)
	}
	recovered, err := migrateClaudeRow(context.Background(), rows[0], root, true, true, nil)
	if err != nil || recovered.State != "checkpointed" || recovered.Error != "" ||
		len(recovered.PreviousErrors) != 1 || recovered.PreviousErrors[0] != "Codex temporarily unavailable" {
		t.Fatalf("capture-only lost the checkpoint or failure history: %+v, %v", recovered, err)
	}
	again = recovered
	again.State, again.TargetDir, again.TargetThreadID = "ready", "/other/checkout", "thread-1"
	if err := saveMigrationReceipt(receiptFile, again); err != nil {
		t.Fatal(err)
	}
	reused, err := migrateClaudeRow(context.Background(), rows[0], root, true, true, nil)
	if err != nil || reused.TargetThreadID != "thread-1" || reused.State != "ready" {
		t.Fatalf("batch capture did not reuse ready snapshot: %+v, %v", reused, err)
	}
	write(`{"type":"user","sessionId":"source-1","cwd":"/remote/repo","timestamp":"2026-10-16T18:02:00Z","message":{"role":"user","content":"one more thing"}}` + "\n")
	newRows, err := checkpoint.InventoryClaude(configDir, "source-1")
	if err != nil || len(newRows) != 1 {
		t.Fatalf("updated inventory: %+v, %v", newRows, err)
	}
	updated, err := migrateClaudeRow(context.Background(), newRows[0], root, true, true, nil)
	if err != nil || updated.CheckpointID == first.CheckpointID {
		t.Fatalf("new source snapshot reused the old checkpoint: %+v, %v", updated, err)
	}
}

func TestCheckpointMigrationRestoresCapturedWorkspace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", filepath.Join(root, "conductor"))
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Test"}} {
		if _, err := checkpoint.Git(ctx, source, args...); err != nil {
			t.Fatal(err)
		}
	}
	tracked := filepath.Join(source, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked.txt"}, {"commit", "-m", "base"}} {
		if _, err := checkpoint.Git(ctx, source, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := checkpoint.Git(ctx, root, "clone", source, target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracked, []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "claude")
	project := filepath.Join(configDir, "projects", "source")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(project, "session.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","sessionId":"session","cwd":"`+source+`","message":{"role":"user","content":"continue"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := checkpoint.Capture(ctx, checkpoint.Request{Harness: "claude", SessionID: "session", Cwd: source, ClaudePath: transcript, ClaudeConfigDir: configDir, Force: true})
	if err != nil || result.Manifest.Workspace.Skipped != "" {
		t.Fatalf("capture omitted workspace: %+v, %v", result.Manifest.Workspace, err)
	}
	if gap := migrationWorkspaceGap(result.Manifest); gap != "" {
		t.Fatalf("captured workspace is incomplete: %s", gap)
	}
	missing := result.Manifest
	missing.Workspace.PatchBytes = 0
	if gap := migrationWorkspaceGap(missing); gap != "tracked patch missing" {
		t.Fatalf("missing tracked patch was accepted: %q", gap)
	}
	missing = result.Manifest
	missing.Workspace.UntrackedFiles = nil
	if gap := migrationWorkspaceGap(missing); gap != "untracked files missing" {
		t.Fatalf("missing untracked file was accepted: %q", gap)
	}
	bundle, err := checkpoint.OpenFile(result.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	receiptFile := filepath.Join(root, "receipt.json")
	receipt := migrationReceipt{TargetDir: target, Workspace: result.Manifest.Workspace}
	if err := restoreMigrationWorkspace(ctx, &receipt, source, target, bundle, receiptFile); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"tracked.txt": "after\n", "new.txt": "new\n"} {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(got) != want {
			t.Fatalf("restored %s = %q, %v", name, got, err)
		}
	}
	if receipt.WorkspaceRestoredTo != target {
		t.Fatalf("missing restoration receipt: %+v", receipt)
	}
	if err := restoreMigrationWorkspace(ctx, &receipt, source, target, bundle, receiptFile); err != nil {
		t.Fatalf("same target was not idempotent: %v", err)
	}
}
