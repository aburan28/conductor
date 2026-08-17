package db

import (
	"errors"
	"testing"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/resource"
)

// Scope drift is the normal case, not an exception: an agent discovers mid-task that it needs
// an adjacent file. DESIGN.md §8.6 requires the expansion to be granted when the ground is
// free and to pause the attempt when it is not. The runner's supervise loop calls exactly
// these store operations every heartbeat, and none of it was covered.

func TestScopeExpansionIsGrantedWhenFree(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Drifting work")

	claim, err := f.store.Claim(f.ctx, f.claimParams(task, f.alice,
		domain.ScopeRequest{Resource: "path:internal/router/router.go", Mode: domain.ModeWriteExclusive}))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// The agent touches an adjacent file nobody holds.
	granted, conflicts, err := f.store.AcquireScopes(f.ctx, AcquireScopesParams{
		ProjectID: f.project.ID, TaskID: task.ID,
		AttemptID: claim.Attempt.ID, LeaseID: claim.Lease.ID, PrincipalID: f.alice.ID,
		Requests: []domain.ScopeRequest{
			{Resource: "path:internal/router/policy.go", Mode: domain.ModeWriteExclusive}},
		Source: domain.SourceObserved,
		Policy: resource.DefaultPolicy(), AllowWarnings: true,
	})
	if err != nil {
		t.Fatalf("expansion onto free ground should succeed: %v", err)
	}
	if len(granted) != 1 {
		t.Fatalf("granted %d reservations, want 1", len(granted))
	}
	if len(conflicts) != 0 {
		t.Errorf("unexpected conflicts: %+v", conflicts)
	}
	// `observed` marks territory the runner detected rather than the agent declared, which
	// is what distinguishes drift from a plan on the conflict radar.
	if granted[0].Source != domain.SourceObserved {
		t.Errorf("source = %s, want observed", granted[0].Source)
	}

	held, err := f.store.ReservationsForTask(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ReservationsForTask: %v", err)
	}
	if len(held) != 2 {
		t.Errorf("task holds %d reservations after expansion, want 2", len(held))
	}
}

// Drifting into someone else's territory must be refused, and the refusal must name the
// holder so the agent can be paused with an actionable reason.
func TestScopeExpansionIsBlockedByAnotherHolder(t *testing.T) {
	f := newFixture(t)
	mine := f.newTask(t, "My work")
	theirs := f.newTask(t, "Their work")

	if _, err := f.store.Claim(f.ctx, f.claimParams(theirs, f.bob,
		domain.ScopeRequest{Resource: "dir:internal/api", Mode: domain.ModeWriteExclusive})); err != nil {
		t.Fatalf("bob claim: %v", err)
	}

	claim, err := f.store.Claim(f.ctx, f.claimParams(mine, f.alice,
		domain.ScopeRequest{Resource: "path:internal/router/router.go", Mode: domain.ModeWriteExclusive}))
	if err != nil {
		t.Fatalf("alice claim: %v", err)
	}

	_, conflicts, err := f.store.AcquireScopes(f.ctx, AcquireScopesParams{
		ProjectID: f.project.ID, TaskID: mine.ID,
		AttemptID: claim.Attempt.ID, LeaseID: claim.Lease.ID, PrincipalID: f.alice.ID,
		Requests: []domain.ScopeRequest{
			{Resource: "path:internal/api/handler.go", Mode: domain.ModeWriteExclusive}},
		Source: domain.SourceObserved,
		Policy: resource.DefaultPolicy(), AllowWarnings: true,
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("drifting into held territory = %v, want ErrConflict", err)
	}
	if len(conflicts) == 0 {
		t.Fatal("blocked without naming a holder; the agent cannot be told what to do")
	}
	if conflicts[0].HolderOwner != "bob" || conflicts[0].HolderTaskRef != theirs.Ref {
		t.Errorf("conflict does not identify the holder: %+v", conflicts[0])
	}

	// The attempt can then be paused, which is what the runner does on a blocked expansion.
	if _, err := f.store.UpdateAttempt(f.ctx, claim.Attempt.ID,
		AttemptProgress{State: domain.AttemptPausedConflict}); err != nil {
		t.Fatalf("pausing a drifting attempt: %v", err)
	}
	attempt, err := f.store.GetAttempt(f.ctx, claim.Attempt.ID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	if attempt.State != domain.AttemptPausedConflict {
		t.Errorf("attempt state = %s, want paused_conflict", attempt.State)
	}
	// A paused attempt keeps its lease: it is stopped, not handed back.
	if _, err := f.store.ActiveLeaseForTask(f.ctx, mine.ID); err != nil {
		t.Errorf("a paused attempt should keep its lease: %v", err)
	}
}

// The observed footprint, not the declaration, is what drives merge risk. An agent that
// quietly edits outside its scope must still show up on the radar.
func TestObservedPathsFeedTheMergeRiskGraph(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "First branch")
	b := f.newTask(t, "Second branch")

	claimA, err := f.store.Claim(f.ctx, f.claimParams(a, f.alice))
	if err != nil {
		t.Fatalf("claim a: %v", err)
	}
	claimB, err := f.store.Claim(f.ctx, f.claimParams(b, f.bob))
	if err != nil {
		t.Fatalf("claim b: %v", err)
	}

	// Neither declared anything, but both are editing the same file.
	shared := []string{"internal/shared/thing.go"}
	for _, id := range []domain.ID{claimA.Attempt.ID, claimB.Attempt.ID} {
		for _, state := range []domain.AttemptState{
			domain.AttemptPreparingWorkspace, domain.AttemptStartingHarness, domain.AttemptRunning,
		} {
			progress := AttemptProgress{State: state}
			if state == domain.AttemptRunning {
				progress.ChangedPaths = shared
			}
			if _, err := f.store.UpdateAttempt(f.ctx, id, progress); err != nil {
				t.Fatalf("advance attempt: %v", err)
			}
		}
	}

	footprints, err := f.store.OpenFootprints(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("OpenFootprints: %v", err)
	}

	seen := 0
	for _, fp := range footprints {
		if fp.TaskID != a.ID && fp.TaskID != b.ID {
			continue
		}
		seen++
		if len(fp.ChangedPaths) != 1 || fp.ChangedPaths[0] != shared[0] {
			t.Errorf("%s footprint = %v, want the observed path", fp.TaskRef, fp.ChangedPaths)
		}
	}
	if seen != 2 {
		t.Errorf("found %d of the 2 tasks in the footprint set", seen)
	}
}
