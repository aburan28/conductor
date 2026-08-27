package db

import (
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// projectWithQueue sets the fixture project's admission caps.
func (f *fixture) setQueuePolicy(t *testing.T, q domain.QueuePolicy) {
	t.Helper()
	cfg := f.project.Config
	cfg.Queue = q
	if err := f.store.UpdateProjectConfig(f.ctx, f.project.ID, cfg); err != nil {
		t.Fatalf("UpdateProjectConfig: %v", err)
	}
	f.project.Config = cfg
}

func (f *fixture) enqueueSession(t *testing.T, who domain.Principal, note string) domain.AdmissionTicket {
	t.Helper()
	ticket, err := f.store.Enqueue(f.ctx, EnqueueParams{
		ProjectID: f.project.ID, PrincipalID: who.ID, Kind: domain.TicketSession,
		Harness: "claude", Note: note, Policy: f.project.Config.Queue,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return ticket
}

// With a two-session cap and no live sessions, the first two session tickets are granted at
// once and the third waits — then advances when a granted one is released.
func TestQueueGrantsUpToTheCapThenWaits(t *testing.T) {
	f := newFixture(t)
	f.setQueuePolicy(t, domain.QueuePolicy{MaxActiveSessions: 2, TicketTTLSeconds: 90})

	a := f.enqueueSession(t, f.alice, "first")
	b := f.enqueueSession(t, f.alice, "second")
	c := f.enqueueSession(t, f.bob, "third")

	if a.State != domain.TicketGranted || b.State != domain.TicketGranted {
		t.Fatalf("first two should be granted, got %s and %s", a.State, b.State)
	}
	if c.State != domain.TicketQueued {
		t.Fatalf("third should wait, got %s", c.State)
	}
	if c.Position != 1 {
		t.Fatalf("third at position %d, want 1", c.Position)
	}

	// Release the first; the waiting ticket is granted on release.
	if _, err := f.store.ReleaseTicket(f.ctx, a.ID, domain.TicketReleased, f.project.Config.Queue); err != nil {
		t.Fatalf("ReleaseTicket: %v", err)
	}
	got, err := f.store.GetTicket(f.ctx, c.ID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.State != domain.TicketGranted {
		t.Fatalf("third should be granted after a release, got %s", got.State)
	}
}

// A per-principal cap keeps one person from taking every slot even when the global cap has
// room, and a different person's ticket is still granted.
func TestQueuePerPrincipalCap(t *testing.T) {
	f := newFixture(t)
	f.setQueuePolicy(t, domain.QueuePolicy{MaxActiveSessions: 5, MaxSessionsPerPrincipal: 1, TicketTTLSeconds: 90})

	a1 := f.enqueueSession(t, f.alice, "alice-1")
	a2 := f.enqueueSession(t, f.alice, "alice-2")
	b1 := f.enqueueSession(t, f.bob, "bob-1")

	if a1.State != domain.TicketGranted {
		t.Fatalf("alice's first should be granted, got %s", a1.State)
	}
	if a2.State != domain.TicketQueued {
		t.Fatalf("alice's second should wait (per-principal cap), got %s", a2.State)
	}
	if b1.State != domain.TicketGranted {
		t.Fatalf("bob's first should be granted (different principal), got %s", b1.State)
	}
}

// Reconcile expires a ticket nobody heartbeats and hands its slot to the next in line.
func TestQueueReconcileExpiresAndGrants(t *testing.T) {
	f := newFixture(t)
	f.setQueuePolicy(t, domain.QueuePolicy{MaxActiveSessions: 1, TicketTTLSeconds: 90})

	granted := f.enqueueSession(t, f.alice, "holder")
	waiting := f.enqueueSession(t, f.bob, "waiter")
	if granted.State != domain.TicketGranted || waiting.State != domain.TicketQueued {
		t.Fatalf("setup: %s / %s", granted.State, waiting.State)
	}

	// Age the granted ticket past its TTL.
	if _, err := f.store.pool.Exec(f.ctx,
		`UPDATE admission_tickets SET expires_at = now() - interval '1 minute' WHERE id = $1::uuid`,
		granted.ID); err != nil {
		t.Fatalf("age ticket: %v", err)
	}
	expired, grantedN, err := f.store.ReconcileQueue(f.ctx, f.project.ID, f.project.Config.Queue)
	if err != nil {
		t.Fatalf("ReconcileQueue: %v", err)
	}
	if expired != 1 || grantedN != 1 {
		t.Fatalf("reconcile expired=%d granted=%d, want 1/1", expired, grantedN)
	}
	got, _ := f.store.GetTicket(f.ctx, waiting.ID)
	if got.State != domain.TicketGranted {
		t.Fatalf("waiter should be granted after reconcile, got %s", got.State)
	}
}

// A live session that never took a ticket still occupies a slot, so a session ticket queued
// behind it waits.
func TestQueueCountsLiveSessions(t *testing.T) {
	f := newFixture(t)
	f.setQueuePolicy(t, domain.QueuePolicy{MaxActiveSessions: 1, TicketTTLSeconds: 90})
	f.newSession(t, f.alice, domain.SessionCapabilities{Model: "sonnet"})

	ticket := f.enqueueSession(t, f.bob, "queued-behind-live")
	if ticket.State != domain.TicketQueued {
		t.Fatalf("ticket should wait behind the live session, got %s", ticket.State)
	}
	snap, err := f.store.ListQueue(f.ctx, f.project.ID, false)
	if err != nil {
		t.Fatalf("ListQueue: %v", err)
	}
	if snap.ActiveSessions != 1 {
		t.Fatalf("active sessions = %d, want 1", snap.ActiveSessions)
	}
}

func TestQueueListPositions(t *testing.T) {
	f := newFixture(t)
	f.setQueuePolicy(t, domain.QueuePolicy{MaxActiveSessions: 0, MaxSessionsPerPrincipal: 0, MaxConcurrentAttempts: 1, TicketTTLSeconds: 90})
	// Session cap is unlimited (0), so session tickets are granted immediately; use attempt
	// tickets, which the attempt cap of 1 gates.
	task := f.newTask(t, "queued work")
	t1, err := f.store.Enqueue(f.ctx, EnqueueParams{ProjectID: f.project.ID, PrincipalID: f.alice.ID, TaskID: task.ID, Kind: domain.TicketAttempt, Policy: f.project.Config.Queue})
	if err != nil {
		t.Fatalf("Enqueue attempt: %v", err)
	}
	task2 := f.newTask(t, "queued work 2")
	t2, err := f.store.Enqueue(f.ctx, EnqueueParams{ProjectID: f.project.ID, PrincipalID: f.bob.ID, TaskID: task2.ID, Kind: domain.TicketAttempt, Policy: f.project.Config.Queue})
	if err != nil {
		t.Fatalf("Enqueue attempt 2: %v", err)
	}
	if t1.State != domain.TicketGranted {
		t.Fatalf("first attempt ticket should be granted, got %s", t1.State)
	}
	if t2.State != domain.TicketQueued || t2.Position != 1 {
		t.Fatalf("second attempt ticket = %s pos %d, want queued/1", t2.State, t2.Position)
	}
	_ = time.Second
}
