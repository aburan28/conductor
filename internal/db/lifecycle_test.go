package db

import (
	"errors"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/resource"
)

// The claim loop end to end at the store level: a claim stays alive exactly as long as the
// session working it, finished work keeps its territory until it lands, and a merge (or
// `task done`) is what lets it go.

func (f *fixture) sessionWithTTL(t *testing.T, who domain.Principal, ttl time.Duration, worktree string) domain.Session {
	t.Helper()
	s, err := f.store.RegisterSession(f.ctx, RegisterSessionParams{
		ProjectID: f.project.ID, PrincipalID: who.ID, Harness: "claude", TTL: ttl,
		WorktreePath: worktree,
	})
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	return s
}

func (f *fixture) leaseLive(t *testing.T, id domain.ID) bool {
	t.Helper()
	l, err := f.store.GetLease(f.ctx, id)
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	return l.Active(time.Now())
}

func path(p string) domain.ScopeRequest {
	return domain.ScopeRequest{Resource: "path:" + p, Mode: domain.ModeWriteExclusive}
}

func TestSessionHeartbeatKeepsItsLeaseAlive(t *testing.T) {
	f := newFixture(t)
	const ttl = 600 * time.Millisecond
	task := f.newTask(t, "Interactive work")
	session := f.sessionWithTTL(t, f.alice, ttl, "")

	params := f.claimParams(task, f.alice, path("internal/lease/keep.go"))
	params.SessionID, params.LeaseTTL = session.ID, ttl
	claim, err := f.store.Claim(f.ctx, params)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Keep the session alive well past three TTLs, reconciling as the scheduler would.
	deadline := time.Now().Add(3*ttl + 300*time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := f.store.HeartbeatSession(f.ctx, HeartbeatSessionParams{SessionID: session.ID, TTL: ttl}); err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
		if reclaimed, err := f.store.ReconcileLeases(f.ctx, f.project.ID); err != nil {
			t.Fatalf("reconcile: %v", err)
		} else if len(reclaimed) > 0 {
			t.Fatalf("a lease under a live session was reclaimed: %+v", reclaimed)
		}
		time.Sleep(ttl / 4)
	}
	if err := f.store.AssertFence(f.ctx, claim.Fence); err != nil {
		t.Fatalf("after 3x TTL with a live session the claim should still hold: %v", err)
	}
	if _, err := f.store.Claim(f.ctx, f.claimParams(f.newTask(t, "Other"), f.bob, path("internal/lease/keep.go"))); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("the territory should still be held, got %v", err)
	}

	// The session dies: no more heartbeats. The lease lapses one TTL later and the reconciler
	// takes it back.
	time.Sleep(ttl + 200*time.Millisecond)
	reclaimed, err := f.store.ReconcileLeases(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].LeaseID != claim.Lease.ID {
		t.Fatalf("reclaimed = %+v, want the dead session's lease", reclaimed)
	}
	if err := f.store.AssertFence(f.ctx, claim.Fence); err == nil {
		t.Error("a reclaimed lease must not pass the fence")
	}
}

func TestClaimRefusesAnotherPrincipalsSession(t *testing.T) {
	f := newFixture(t)
	bobSession := f.sessionWithTTL(t, f.bob, time.Minute, "")
	task := f.newTask(t, "Alice's work")
	params := f.claimParams(task, f.alice)
	params.SessionID = bobSession.ID
	if _, err := f.store.Claim(f.ctx, params); !errors.Is(err, domain.ErrNotPermitted) {
		t.Fatalf("binding a claim to someone else's session = %v, want ErrNotPermitted", err)
	}
}

func TestWrapAdoptsAClaimMadeBeforeIt(t *testing.T) {
	f := newFixture(t)
	const ttl = 500 * time.Millisecond
	task := f.newTask(t, "Claim then wrap")
	params := f.claimParams(task, f.alice)
	params.WorktreePath, params.LeaseTTL = "/work/checkout-a", 2*time.Second
	claim, err := f.store.Claim(f.ctx, params)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// A session in another checkout, or someone else's, does not take it.
	elsewhere := f.sessionWithTTL(t, f.alice, ttl, "")
	if got, err := f.store.AdoptLeases(f.ctx, AdoptLeasesParams{SessionID: elsewhere.ID, WorktreePath: "/work/checkout-b", TTL: ttl}); err != nil || len(got) != 0 {
		t.Fatalf("adopt from another checkout = %v, %v; want nothing", got, err)
	}
	bob := f.sessionWithTTL(t, f.bob, ttl, "")
	if got, err := f.store.AdoptLeases(f.ctx, AdoptLeasesParams{SessionID: bob.ID, WorktreePath: "/work/checkout-a", TTL: ttl}); err != nil || len(got) != 0 {
		t.Fatalf("adopt by another principal = %v, %v; want nothing", got, err)
	}

	session := f.sessionWithTTL(t, f.alice, ttl, "")
	got, err := f.store.AdoptLeases(f.ctx, AdoptLeasesParams{SessionID: session.ID, WorktreePath: "/work/checkout-a", TTL: ttl})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if len(got) != 1 || got[0].TaskRef != task.Ref {
		t.Fatalf("adopted = %+v, want %s", got, task.Ref)
	}
	if s, _ := f.store.GetSession(f.ctx, session.ID); s.ActiveTaskID != task.ID {
		t.Errorf("session active task = %q, want %s", s.ActiveTaskID, task.ID)
	}
	// From here the session's heartbeat carries it past what the claim alone would last.
	for i := 0; i < 8; i++ {
		if _, err := f.store.HeartbeatSession(f.ctx, HeartbeatSessionParams{SessionID: session.ID, TTL: ttl}); err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
		time.Sleep(ttl / 2)
	}
	if !f.leaseLive(t, claim.Lease.ID) {
		t.Fatal("an adopted claim should live as long as its session heartbeats")
	}
	// A second session cannot take a claim a live session is carrying.
	again := f.sessionWithTTL(t, f.alice, ttl, "")
	if got, _ := f.store.AdoptLeases(f.ctx, AdoptLeasesParams{SessionID: again.ID, WorktreePath: "/work/checkout-a", TTL: ttl}); len(got) != 0 {
		t.Errorf("a claim held by a live session was adopted again: %+v", got)
	}
}

func TestHeartbeatRecordsObservedPaths(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Observed")
	session := f.sessionWithTTL(t, f.alice, time.Minute, "")
	params := f.claimParams(task, f.alice)
	params.SessionID = session.ID
	if _, err := f.store.Claim(f.ctx, params); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := f.store.HeartbeatSession(f.ctx, HeartbeatSessionParams{SessionID: session.ID,
		ChangedPaths: []string{"cmd/x/main.go", "README.md"}}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	footprints, err := f.store.OpenFootprints(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("OpenFootprints: %v", err)
	}
	for _, fp := range footprints {
		if fp.TaskID == task.ID {
			if len(fp.ChangedPaths) != 2 {
				t.Fatalf("observed paths = %v, want the two reported", fp.ChangedPaths)
			}
			return
		}
	}
	t.Fatal("task missing from footprints")
}

// finish runs a claim through to a successful attempt, as FinishWork does.
func (f *fixture) finish(t *testing.T, claim ClaimResult) domain.Task {
	t.Helper()
	if _, err := f.store.UpdateAttempt(f.ctx, claim.Attempt.ID, AttemptProgress{State: domain.AttemptRunning}); err != nil {
		t.Fatalf("attempt running: %v", err)
	}
	if _, err := f.store.UpdateTaskStatus(f.ctx, claim.Task.ID, domain.TaskRunning); err != nil {
		t.Fatalf("task running: %v", err)
	}
	task, err := f.store.Release(f.ctx, ReleaseParams{Fence: claim.Fence, Reason: "succeeded",
		AttemptState: domain.AttemptSucceeded, NextTaskStatus: domain.TaskVerifying})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	return task
}

func TestFinishedWorkHoldsItsTerritoryUntilDone(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "Lands later")
	claim, err := f.store.Claim(f.ctx, f.claimParams(a, f.alice, path("internal/merge/held.go")))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got := f.finish(t, claim); got.Status != domain.TaskVerifying {
		t.Fatalf("status = %s, want verifying", got.Status)
	}

	// The work is finished but unmerged: the next person is told so, not handed the file.
	conflicts, err := f.store.CheckScopes(f.ctx, CheckScopesParams{ProjectID: f.project.ID, Viewer: f.bob.ID,
		Requests: []domain.ScopeRequest{path("internal/merge/held.go")}, Policy: resource.DefaultPolicy()})
	if err != nil {
		t.Fatalf("CheckScopes: %v", err)
	}
	if len(conflicts) != 1 || !conflicts[0].PendingMerge() || conflicts[0].HolderStatus != domain.TaskVerifying {
		t.Fatalf("conflicts = %+v, want one pending-merge hold", conflicts)
	}
	b := f.newTask(t, "Next")
	if _, err := f.store.Claim(f.ctx, f.claimParams(b, f.bob, path("internal/merge/held.go"))); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("claiming a file in an unmerged change = %v, want a conflict", err)
	}

	// Done releases it.
	if _, err := f.store.UpdateTaskStatus(f.ctx, a.ID, domain.TaskDone); err != nil {
		t.Fatalf("done: %v", err)
	}
	if left, _ := f.store.ReservationsForTask(f.ctx, a.ID); len(left) != 0 {
		t.Errorf("a done task still holds %d reservation(s)", len(left))
	}
	if _, err := f.store.Claim(f.ctx, f.claimParams(b, f.bob, path("internal/merge/held.go"))); err != nil {
		t.Errorf("territory should be free once the task is done: %v", err)
	}
}

func TestReopenKeepsTheHoldAndRequeues(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "Changes requested")
	claim, err := f.store.Claim(f.ctx, f.claimParams(a, f.alice, path("internal/merge/reopen.go")))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	f.finish(t, claim)
	task, err := f.store.UpdateTaskStatus(f.ctx, a.ID, domain.TaskReady)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if task.Status != domain.TaskReady {
		t.Fatalf("status = %s", task.Status)
	}
	// The branch still carries the edits, so the territory stays with the task.
	if left, _ := f.store.ReservationsForTask(f.ctx, a.ID); len(left) == 0 {
		t.Error("reopening dropped the task's territory")
	}
	// And it is claimable again.
	if _, err := f.store.Claim(f.ctx, f.claimParams(a, f.bob)); err != nil {
		t.Errorf("a reopened task should be claimable: %v", err)
	}
}

func TestCancelEndsTheLiveLease(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "Cancelled mid-flight")
	claim, err := f.store.Claim(f.ctx, f.claimParams(a, f.alice, path("internal/cancel/x.go")))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := f.store.UpdateTaskStatus(f.ctx, a.ID, domain.TaskCancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if f.leaseLive(t, claim.Lease.ID) {
		t.Error("cancelling a task must end its lease")
	}
	if err := f.store.AssertFence(f.ctx, claim.Fence); err == nil {
		t.Error("the cancelled task's fence still passes")
	}
	if left, _ := f.store.ReservationsForTask(f.ctx, a.ID); len(left) != 0 {
		t.Errorf("a cancelled task still holds %d reservation(s)", len(left))
	}
}

func TestMergedPullRequestCompletesTheTask(t *testing.T) {
	cases := []struct {
		name   string
		finish bool
	}{
		{"from verifying", true},
		{"while still running", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			a := f.newTask(t, "Merge me")
			claim, err := f.store.Claim(f.ctx, f.claimParams(a, f.alice, path("internal/merge/done.go")))
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if tc.finish {
				f.finish(t, claim)
			} else if _, err := f.store.UpdateTaskStatus(f.ctx, a.ID, domain.TaskRunning); err != nil {
				t.Fatalf("running: %v", err)
			}
			url := "https://github.com/acme/widgets/pull/12"
			if linked, err := f.store.LinkPullRequest(f.ctx, a.ID, url); err != nil || !linked {
				t.Fatalf("link = %v, %v", linked, err)
			}
			out, err := f.store.PullRequestMerged(f.ctx, a.ID, url)
			if err != nil {
				t.Fatalf("merged: %v", err)
			}
			if out.Status != domain.TaskDone || !out.Changed {
				t.Fatalf("outcome = %+v, want done", out)
			}
			task, _ := f.store.GetTask(f.ctx, a.ID)
			if task.Status != domain.TaskDone || task.PullRequestState != PullRequestMerged || task.PullRequestURL != url {
				t.Errorf("task = %s %q %q", task.Status, task.PullRequestState, task.PullRequestURL)
			}
			if f.leaseLive(t, claim.Lease.ID) {
				t.Error("the lease outlived the merge")
			}
			if left, _ := f.store.ReservationsForTask(f.ctx, a.ID); len(left) != 0 {
				t.Errorf("merged task still holds %d reservation(s)", len(left))
			}
			// A second delivery of the same merge changes nothing.
			if again, err := f.store.PullRequestMerged(f.ctx, a.ID, url); err != nil || again.Changed {
				t.Errorf("redelivery = %+v, %v", again, err)
			}
		})
	}
}

func TestFinishingAfterTheMergeCompletes(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "Merged before finish")
	claim, err := f.store.Claim(f.ctx, f.claimParams(a, f.alice))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// The merge is recorded while the task is still claimed (it walks to done) ...
	if _, err := f.store.PullRequestMerged(f.ctx, a.ID, "https://github.com/acme/w/pull/3"); err != nil {
		t.Fatalf("merged: %v", err)
	}
	task, _ := f.store.GetTask(f.ctx, a.ID)
	if task.Status != domain.TaskDone {
		t.Fatalf("status = %s, want done", task.Status)
	}
	// ... and the old fence can no longer publish into it.
	if err := f.store.AssertFence(f.ctx, claim.Fence); err == nil {
		t.Error("a fence from before the merge still passes")
	}
}

func TestClosedPullRequestSendsWorkBack(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "Rejected")
	claim, err := f.store.Claim(f.ctx, f.claimParams(a, f.alice, path("internal/merge/closed.go")))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	f.finish(t, claim)
	out, err := f.store.PullRequestClosed(f.ctx, a.ID, "https://github.com/acme/w/pull/4")
	if err != nil {
		t.Fatalf("closed: %v", err)
	}
	if out.Status != domain.TaskReady || !out.Changed {
		t.Fatalf("outcome = %+v, want ready", out)
	}
	if left, _ := f.store.ReservationsForTask(f.ctx, a.ID); len(left) != 0 {
		t.Errorf("an abandoned change still holds %d reservation(s)", len(left))
	}

	// A task still being worked is left alone.
	b := f.newTask(t, "Still going")
	if _, err := f.store.Claim(f.ctx, f.claimParams(b, f.alice)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if out, err := f.store.PullRequestClosed(f.ctx, b.ID, "https://github.com/acme/w/pull/5"); err != nil || out.Changed {
		t.Errorf("closing an in-progress task's PR = %+v, %v; want unchanged", out, err)
	}
}

func TestReleaseNotifiesWhoeverWasWaiting(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "Holder")
	claim, err := f.store.Claim(f.ctx, f.claimParams(a, f.alice, path("internal/notify/x.go")))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Bob was refused the file.
	if _, err := f.store.RecordIntent(f.ctx, domain.Intent{
		ProjectID: f.project.ID, PrincipalID: f.bob.ID, Fingerprint: "waiting", MinHash: []int64{},
		Scopes: []domain.ScopeRequest{path("internal/notify/x.go")}, Outcome: domain.OutcomeBlockConflict,
	}, 15*time.Minute); err != nil {
		t.Fatalf("RecordIntent: %v", err)
	}
	if _, err := f.store.Release(f.ctx, ReleaseParams{Fence: claim.Fence, NextTaskStatus: domain.TaskReady}); err != nil {
		t.Fatalf("release: %v", err)
	}
	events, err := f.store.ListEvents(f.ctx, f.project.ID, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for _, e := range events {
		if e.Type == "scope.released" {
			if e.Payload["principal"] != "bob" || e.Payload["task_ref"] != a.Ref {
				t.Fatalf("scope.released payload = %v", e.Payload)
			}
			return
		}
	}
	t.Fatal("no scope.released event for the waiting party")
}

// Recording a pull request's end twice records it once; a closed pull request that is seen
// open again was reopened and is linked again, but a merge never moves back.
func TestPullRequestEndsAreRecordedOnce(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "Closed twice")
	claim, err := f.store.Claim(f.ctx, f.claimParams(a, f.alice))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	f.finish(t, claim)
	url := "https://github.com/acme/w/pull/30"
	first, err := f.store.PullRequestClosed(f.ctx, a.ID, url)
	if err != nil || !first.Recorded || !first.Changed {
		t.Fatalf("first close = %+v, %v", first, err)
	}
	again, err := f.store.PullRequestClosed(f.ctx, a.ID, url)
	if err != nil || again.Recorded || again.Changed {
		t.Fatalf("second close = %+v, %v; want nothing recorded", again, err)
	}
	if linked, err := f.store.LinkPullRequest(f.ctx, a.ID, url); err != nil || !linked {
		t.Errorf("a reopened pull request was not linked again: %v, %v", linked, err)
	}

	b := f.newTask(t, "Merged, then listed open by a stale poll")
	if _, err := f.store.Claim(f.ctx, f.claimParams(b, f.alice)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if out, err := f.store.PullRequestMerged(f.ctx, b.ID, url+"1"); err != nil || !out.Recorded {
		t.Fatalf("merge = %+v, %v", out, err)
	}
	if out, err := f.store.PullRequestMerged(f.ctx, b.ID, url+"1"); err != nil || out.Recorded {
		t.Errorf("second merge = %+v, %v; want nothing recorded", out, err)
	}
	if linked, _ := f.store.LinkPullRequest(f.ctx, b.ID, url+"1"); linked {
		t.Error("a merged pull request was moved back to open")
	}
}
