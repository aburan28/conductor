package db

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/taskcard"
)

// DESIGN.md §31.6: a task can be handed from one harness to another without sharing the
// original transcript.
//
// This is the acceptance criterion that most needs an executable proof, because "we did not
// share the transcript" is exactly the kind of claim that quietly stops being true.
func TestHandoffCarriesStateNotTranscript(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Rework scheduler backoff")

	// Attempt 1: Claude claims, works, and hands off to Codex.
	claim, err := f.store.Claim(f.ctx, func() ClaimParams {
		p := f.claimParams(task, f.alice,
			domain.ScopeRequest{Resource: "path:internal/scheduler/scheduler.go", Mode: domain.ModeWriteExclusive})
		p.Harness = "claude"
		p.ModelAlias = "worker.general"
		return p
	}())
	if err != nil {
		t.Fatalf("claude claim: %v", err)
	}

	if _, err := f.store.UpdateAttempt(f.ctx, claim.Attempt.ID, AttemptProgress{
		State: domain.AttemptPreparingWorkspace, Branch: "agent/" + task.Ref + "/attempt-1",
	}); err != nil {
		t.Fatalf("update attempt: %v", err)
	}

	handoff, err := f.store.CreateHandoff(f.ctx, domain.Handoff{
		TaskID:        task.ID,
		FromAttemptID: claim.Attempt.ID,
		ToHarness:     "codex",
		CreatedBy:     f.alice.ID,
		Bundle: domain.HandoffBundle{
			RecommendedNextAction: "Write the backoff tests",
			CompletedWork:         []string{"Implemented exponential backoff"},
			Assumptions:           []string{"Retryable errors are already classified"},
			OpenQuestions:         []string{"Should jitter be configurable?"},
			Decisions: []domain.DecisionRecord{
				{Summary: "Backoff caps at the lease TTL", By: "alice", At: time.Now().UTC()},
			},
			Blockers: []domain.Blocker{{Kind: "none", Summary: ""}},
		},
	}, claim.Fence, true)
	if err != nil {
		t.Fatalf("CreateHandoff: %v", err)
	}

	// The handoff released the lease and the territory, so a different harness can pick it up.
	if _, err := f.store.ActiveLeaseForTask(f.ctx, task.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("handoff should have released the lease, got %v", err)
	}
	held, err := f.store.ReservationsForTask(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ReservationsForTask: %v", err)
	}
	if len(held) != 0 {
		t.Errorf("handoff left %d reservation(s) held", len(held))
	}

	fresh, err := f.store.GetTask(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if fresh.Status != domain.TaskReady {
		t.Errorf("status after handoff = %s, want ready", fresh.Status)
	}

	// The old fence is dead: the handing-off session cannot keep publishing.
	if err := f.store.AssertFence(f.ctx, claim.Fence); err == nil {
		t.Error("the released fence should no longer be valid")
	}

	// Attempt 2: a different harness, same task identity.
	second, err := f.store.Claim(f.ctx, func() ClaimParams {
		p := f.claimParams(task, f.bob)
		p.Harness = "codex"
		p.ModelAlias = "worker.fast"
		return p
	}())
	if err != nil {
		t.Fatalf("codex claim: %v", err)
	}
	if second.Task.ID != task.ID {
		t.Error("handoff should preserve task identity across harnesses")
	}
	if second.Attempt.AttemptNumber != claim.Attempt.AttemptNumber+1 {
		t.Errorf("attempt number = %d, want %d",
			second.Attempt.AttemptNumber, claim.Attempt.AttemptNumber+1)
	}
	if second.Fence.FencingEpoch <= claim.Fence.FencingEpoch {
		t.Errorf("new epoch %d must exceed the old %d",
			second.Fence.FencingEpoch, claim.Fence.FencingEpoch)
	}

	// What the second harness actually receives.
	loaded, err := f.store.LatestHandoff(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("LatestHandoff: %v", err)
	}
	if loaded.ID != handoff.ID {
		t.Errorf("loaded handoff %s, want %s", loaded.ID, handoff.ID)
	}

	card := taskcard.Card{ID: task.Ref, Title: task.Title}
	instruction, err := taskcard.Instruction(card, "# Workflow\n\nrules here", &loaded.Bundle)
	if err != nil {
		t.Fatalf("Instruction: %v", err)
	}

	// Continuation state is present...
	for _, want := range []string{
		"Write the backoff tests",
		"Implemented exponential backoff",
		"Should jitter be configurable?",
		"Backoff caps at the lease TTL",
	} {
		if !strings.Contains(instruction, want) {
			t.Errorf("handoff instruction lost %q", want)
		}
	}

	// ...and the bundle, as stored, has no field that could carry a conversation.
	encoded, err := json.Marshal(loaded.Bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	for key := range asMap {
		for _, banned := range []string{"prompt", "transcript", "message", "conversation", "reasoning"} {
			if strings.Contains(strings.ToLower(key), banned) {
				t.Errorf("handoff bundle carries field %q, which could hold a transcript", key)
			}
		}
	}
}

// A handoff to a harness that is not installed must still release the task cleanly. The
// alternative — a task stuck holding territory because its target is missing — is the worst
// possible failure mode for a coordination system.
func TestHandoffReleasesEvenWithUnknownTarget(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Portable work")

	claim, err := f.store.Claim(f.ctx, f.claimParams(task, f.alice,
		domain.ScopeRequest{Resource: "dir:internal/api", Mode: domain.ModeWriteExclusive}))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, err := f.store.CreateHandoff(f.ctx, domain.Handoff{
		TaskID: task.ID, FromAttemptID: claim.Attempt.ID,
		ToHarness: "some-tool-that-does-not-exist", CreatedBy: f.alice.ID,
		Bundle: domain.HandoffBundle{RecommendedNextAction: "continue"},
	}, claim.Fence, true); err != nil {
		t.Fatalf("CreateHandoff: %v", err)
	}

	held, err := f.store.ReservationsForTask(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ReservationsForTask: %v", err)
	}
	if len(held) != 0 {
		t.Errorf("territory still held after handoff to an unknown target: %d", len(held))
	}
}
