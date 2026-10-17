package db

import (
	"errors"
	"testing"

	"github.com/aburan28/conductor/internal/domain"
)

// A no-change attempt is legitimate: the runner classifies it as failed/no_changes and still
// submits its evidence manifest, whose ChangedPaths is a nil slice (JSON null over HTTP). pgx
// encodes that as SQL NULL, and attempts.changed_paths is NOT NULL — so the merge guard must
// treat NULL like "nothing new" rather than falling through to writing it.
func TestSubmitEvidenceNilChangedPaths(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "No-change attempt")
	claim, err := f.store.Claim(f.ctx, f.claimParams(task, f.alice))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	for _, paths := range [][]string{nil, {}} {
		m := domain.EvidenceManifest{
			TaskID: claim.Fence.TaskID, AttemptID: claim.Fence.AttemptID,
			LeaseID: claim.Fence.LeaseID, FencingEpoch: claim.Fence.FencingEpoch,
			ChangedPaths: paths,
		}
		if err := f.store.SubmitEvidence(f.ctx, m, f.alice.ID); err != nil {
			t.Fatalf("SubmitEvidence with ChangedPaths=%#v: %v", paths, err)
		}
	}

	var n int
	if err := f.store.pool.QueryRow(f.ctx,
		`SELECT cardinality(changed_paths) FROM attempts WHERE id = $1::uuid`,
		claim.Fence.AttemptID).Scan(&n); err != nil {
		t.Fatalf("read back changed_paths: %v", err)
	}
	if n != 0 {
		t.Errorf("changed_paths cardinality = %d, want 0 (empty array, not NULL)", n)
	}
}

// A stale fence must still be refused before any evidence lands, including the empty case.
func TestSubmitEvidenceNilChangedPathsStaleFence(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Fenced no-change attempt")
	first, err := f.store.Claim(f.ctx, f.claimParams(task, f.alice))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, err := f.store.pool.Exec(f.ctx,
		`UPDATE leases SET expires_at = now() - interval '1 minute' WHERE id = $1::uuid`,
		first.Lease.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, err := f.store.ReconcileLeases(f.ctx, f.project.ID); err != nil {
		t.Fatalf("ReconcileLeases: %v", err)
	}

	err = f.store.SubmitEvidence(f.ctx, domain.EvidenceManifest{
		TaskID: first.Fence.TaskID, AttemptID: first.Fence.AttemptID,
		LeaseID: first.Fence.LeaseID, FencingEpoch: first.Fence.FencingEpoch,
	}, "")
	if !errors.Is(err, domain.ErrStaleFencing) {
		t.Errorf("SubmitEvidence with stale epoch = %v, want ErrStaleFencing", err)
	}
}

// A required check passes only on a runner's observation, and only while the runner's latest
// result for it is a pass (DESIGN.md §25.5).
func TestRequiredChecksPassedNeedsRunnerEvidence(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Self-reported checks")
	claim, err := f.store.Claim(f.ctx, f.claimParams(task, f.alice))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	runner, err := f.store.RegisterRunner(f.ctx, domain.Runner{OrganizationID: f.org.ID,
		ProjectID: f.project.ID, PrincipalID: f.alice.ID, Name: "rc-" + string(claim.Fence.AttemptID)[:8]})
	if err != nil {
		t.Fatalf("RegisterRunner: %v", err)
	}
	required := []string{"go test ./..."}
	submit := func(code int, runnerID domain.ID) {
		t.Helper()
		if err := f.store.SubmitEvidence(f.ctx, domain.EvidenceManifest{
			TaskID: claim.Fence.TaskID, AttemptID: claim.Fence.AttemptID,
			LeaseID: claim.Fence.LeaseID, FencingEpoch: claim.Fence.FencingEpoch,
			Commands: []domain.ValidationResult{{CommandID: "c", Command: required[0], ExitCode: code}},
		}, runnerID); err != nil {
			t.Fatalf("SubmitEvidence: %v", err)
		}
	}
	check := func(want bool, why string) {
		t.Helper()
		ok, missing, err := f.store.RequiredChecksPassed(f.ctx, claim.Fence.AttemptID, required)
		if err != nil {
			t.Fatal(err)
		}
		if ok != want {
			t.Errorf("%s: passed = %v (missing %v), want %v", why, ok, missing, want)
		}
	}

	submit(0, "") // the agent's own "tests passed"
	check(false, "agent-reported pass")
	submit(0, runner.ID)
	check(true, "runner-observed pass")
	submit(1, runner.ID)
	check(false, "runner pass then runner failure")
	submit(0, "")
	check(false, "agent pass after a runner failure")
	submit(0, runner.ID)
	check(true, "runner pass after fixing")
}
