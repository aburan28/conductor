package db

import (
	"errors"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// Assignment lifecycle at the store level. The API suite proves the routing decision; these
// prove the invariants that keep an offer from stranding a task.

func (f *fixture) newSession(t *testing.T, principal domain.Principal, caps domain.SessionCapabilities) domain.Session {
	t.Helper()
	session, err := f.store.RegisterSession(f.ctx, RegisterSessionParams{
		ProjectID: f.project.ID, PrincipalID: principal.ID, Harness: "claude",
		Capabilities: caps, TTL: 90 * time.Second,
	})
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	return session
}

func (f *fixture) offer(t *testing.T, task domain.Task, session domain.Session) domain.Assignment {
	t.Helper()
	a, err := f.store.CreateAssignment(f.ctx, CreateAssignmentParams{
		TaskID: task.ID, ProjectID: f.project.ID, SessionID: session.ID,
		Requirement: domain.CapabilityRequirement{Tier: domain.TierT4, Effort: domain.EffortXHigh},
		CreatedBy:   f.alice.ID, TTL: 15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}
	return a
}

// Two coordinators racing to place the same task must produce one offer, not two sessions
// both told the work is theirs.
func TestOnlyOneOpenOfferPerTask(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Coordinate the migration")
	first := f.newSession(t, f.alice, domain.SessionCapabilities{Model: "opus", Tier: domain.TierT4})
	second := f.newSession(t, f.bob, domain.SessionCapabilities{Model: "opus", Tier: domain.TierT4})

	f.offer(t, task, first)

	_, err := f.store.CreateAssignment(f.ctx, CreateAssignmentParams{
		TaskID: task.ID, ProjectID: f.project.ID, SessionID: second.ID, CreatedBy: f.alice.ID,
	})
	if !errors.Is(err, domain.ErrAlreadyClaimed) {
		t.Fatalf("second offer err = %v, want ErrAlreadyClaimed", err)
	}

	// Once the first offer is closed the task can be placed again.
	if _, err := f.store.RespondToAssignment(f.ctx, f.offerID(t, task), domain.AssignmentDeclined, "busy"); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if _, err := f.store.CreateAssignment(f.ctx, CreateAssignmentParams{
		TaskID: task.ID, ProjectID: f.project.ID, SessionID: second.ID, CreatedBy: f.alice.ID,
	}); err != nil {
		t.Fatalf("re-offer after decline: %v", err)
	}
}

func (f *fixture) offerID(t *testing.T, task domain.Task) domain.ID {
	t.Helper()
	open, err := f.store.OpenAssignments(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("OpenAssignments: %v", err)
	}
	for _, a := range open {
		if a.TaskID == task.ID {
			return a.ID
		}
	}
	t.Fatalf("no open offer for %s", task.Ref)
	return ""
}

// A laptop that closes mid-offer must not hold a task hostage behind the open-offer index.
func TestOffersHeldByAStaleSessionAreReleased(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Long-running review")
	session := f.newSession(t, f.alice, domain.SessionCapabilities{Model: "opus", Tier: domain.TierT4})
	f.offer(t, task, session)

	// The session stops heartbeating and the reaper marks it stale.
	if _, err := f.store.pool.Exec(f.ctx,
		`UPDATE sessions SET heartbeat_at = now() - interval '10 minutes',
		                     expires_at = now() - interval '9 minutes' WHERE id = $1::uuid`,
		session.ID); err != nil {
		t.Fatalf("age session: %v", err)
	}
	if _, err := f.store.ReapSessions(f.ctx, time.Minute); err != nil {
		t.Fatalf("ReapSessions: %v", err)
	}

	n, err := f.store.ExpireAssignments(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ExpireAssignments: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired %d offers, want 1", n)
	}
	open, err := f.store.OpenAssignments(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("OpenAssignments: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("%d offer(s) still open after the session went stale", len(open))
	}
}

func TestUnansweredOffersExpireOnTheirOwn(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Quick question")
	session := f.newSession(t, f.alice, domain.SessionCapabilities{Model: "opus", Tier: domain.TierT4})

	if _, err := f.store.CreateAssignment(f.ctx, CreateAssignmentParams{
		TaskID: task.ID, ProjectID: f.project.ID, SessionID: session.ID,
		CreatedBy: f.alice.ID,
	}); err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}
	// A non-positive TTL is normalized to the default, so age the row rather than asking for
	// an expiry in the past.
	if _, err := f.store.pool.Exec(f.ctx,
		`UPDATE task_assignments SET expires_at = now() - interval '1 minute' WHERE task_id = $1::uuid`,
		task.ID); err != nil {
		t.Fatalf("age offer: %v", err)
	}

	if _, err := f.store.ExpireAssignments(f.ctx, f.project.ID); err != nil {
		t.Fatalf("ExpireAssignments: %v", err)
	}
	assignments, err := f.store.SessionInbox(f.ctx, session.ID, true)
	if err != nil {
		t.Fatalf("SessionInbox: %v", err)
	}
	if len(assignments) != 1 || assignments[0].State != domain.AssignmentExpired {
		t.Fatalf("assignments = %+v, want one expired", assignments)
	}
}

// Claiming settles the offer. An offer that outlived the opening it was made for would block
// every later attempt to place the task.
func TestClaimingATaskClosesItsOffer(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Implement the retry policy")
	session := f.newSession(t, f.alice, domain.SessionCapabilities{Model: "opus", Tier: domain.TierT4})
	f.offer(t, task, session)

	if err := f.store.FulfilledByAttempt(f.ctx, task.ID, session.ID); err != nil {
		t.Fatalf("FulfilledByAttempt: %v", err)
	}
	inbox, err := f.store.SessionInbox(f.ctx, session.ID, true)
	if err != nil {
		t.Fatalf("SessionInbox: %v", err)
	}
	if len(inbox) != 1 || inbox[0].State != domain.AssignmentFulfilled {
		t.Fatalf("assignments = %+v, want one fulfilled", inbox)
	}
}

func TestCancelClosesOpenOffers(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Superseded work")
	session := f.newSession(t, f.alice, domain.SessionCapabilities{Model: "opus", Tier: domain.TierT4})
	f.offer(t, task, session)

	n, err := f.store.CancelAssignmentsForTask(f.ctx, task.ID, "task was claimed")
	if err != nil {
		t.Fatalf("CancelAssignmentsForTask: %v", err)
	}
	if n != 1 {
		t.Fatalf("cancelled %d, want 1", n)
	}
	open, err := f.store.OpenAssignments(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("OpenAssignments: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("%d offer(s) survived cancellation", len(open))
	}
}

// Capabilities survive the round trip, including the resolved half the catalog filled in.
func TestSessionCapabilitiesRoundTrip(t *testing.T) {
	f := newFixture(t)
	caps := domain.SessionCapabilities{
		Model: "test-opus", Alias: "planner.frontier", Effort: domain.EffortHigh,
		MaxEffort: domain.EffortXHigh, Tier: domain.TierT4,
		Capabilities: []string{"architecture"}, ContextWindow: 1000000, Resolved: true,
		Roles: []string{"planner"},
	}
	session := f.newSession(t, f.alice, caps)
	if session.Capabilities.MaxEffort != domain.EffortXHigh {
		t.Errorf("ceiling = %s, want xhigh", session.Capabilities.MaxEffort)
	}

	fetched, err := f.store.GetSession(f.ctx, session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if fetched.Capabilities.Tier != domain.TierT4 || !fetched.Capabilities.Resolved {
		t.Errorf("capabilities = %+v, want the stored record", fetched.Capabilities)
	}

	updated, err := f.store.SetSessionCapabilities(f.ctx, session.ID, domain.SessionCapabilities{
		Model: "test-haiku", Effort: domain.EffortLow, Tier: domain.TierT1, Resolved: true,
	})
	if err != nil {
		t.Fatalf("SetSessionCapabilities: %v", err)
	}
	if updated.Capabilities.Tier != domain.TierT1 || updated.Capabilities.MaxEffort != "" {
		t.Errorf("capabilities = %+v, want the new record to replace the old one", updated.Capabilities)
	}

	live, err := f.store.LiveSessions(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("LiveSessions: %v", err)
	}
	if len(live) != 1 || live[0].Capabilities.Model != "test-haiku" {
		t.Errorf("live sessions = %+v, want the updated capabilities", live)
	}
}
