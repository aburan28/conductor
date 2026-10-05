package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/resource"
)

// Scheduler tests run against a schema of their own. The scheduler's state is global by
// design — one shared heartbeat, leases extended across every project, retention across
// every tenant — so in the shared test database it would reach into rows other packages'
// tests are asserting on at the same moment.

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func isolatedStore(t *testing.T) *db.Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping scheduler integration tests")
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("schedtest_%d", time.Now().UnixNano())
	if _, err := admin.Pool().Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Pool().Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	store, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open schema: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

type fixture struct {
	t       *testing.T
	ctx     context.Context
	store   *db.Store
	org     domain.Organization
	alice   domain.Principal
	project domain.Project
}

func newFixture(t *testing.T, cfg domain.ProjectConfig) *fixture {
	t.Helper()
	store := isolatedStore(t)
	ctx := context.Background()
	org, err := store.CreateOrganization(ctx, "org", "Org")
	if err != nil {
		t.Fatal(err)
	}
	alice, err := store.CreatePrincipal(ctx, org.ID, domain.PrincipalHuman, "alice", "Alice", "")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, db.CreateProjectParams{
		OrganizationID: org.ID, Slug: "proj", DisplayName: "Proj", Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, project.ID, alice.ID, domain.RoleContributor); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, ctx: ctx, store: store, org: org, alice: alice, project: project}
}

func (f *fixture) scheduler(holder string) *Scheduler {
	return New(f.store, coord.New(f.store), Options{Logger: quiet, Holder: holder})
}

// claim creates a ready task and claims it, returning the claim.
func (f *fixture) claim(title string) db.ClaimResult {
	f.t.Helper()
	task, err := f.store.CreateTask(f.ctx, db.CreateTaskParams{
		ProjectID: f.project.ID, CreatedBy: f.alice.ID, Title: title, Status: domain.TaskReady,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := f.store.Claim(f.ctx, db.ClaimParams{
		TaskID: task.ID, ProjectID: f.project.ID, HolderPrincipal: f.alice.ID,
		Harness: "test", ModelAlias: "worker.fast", LeaseTTL: 90 * time.Second,
		ScopePolicy: resource.DefaultPolicy(),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.store.Pool().Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) countEvents(eventType string) int {
	f.t.Helper()
	var n int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT count(*) FROM domain_events WHERE project_id = $1::uuid AND event_type = $2`,
		f.project.ID, eventType).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) tick(s *Scheduler) {
	f.t.Helper()
	if err := s.Tick(f.ctx); err != nil {
		f.t.Fatalf("tick: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Outage recovery
// ---------------------------------------------------------------------------

// The control plane was down for five minutes. A worker's lease ran out during that time
// only because nothing could accept its heartbeat; the first tick back must extend it, not
// reclaim it and fence the worker off.
func TestOutageExtendsLeasesInsteadOfReclaiming(t *testing.T) {
	f := newFixture(t, domain.DefaultProjectConfig())
	s := f.scheduler("replica-a")
	f.tick(s) // the scheduler was running before the outage

	live := f.claim("in flight across the outage")
	f.exec(`UPDATE leases SET heartbeat_at = now() - interval '100 seconds',
	                          expires_at   = now() - interval '10 seconds' WHERE id = $1::uuid`, live.Lease.ID)
	f.exec(`UPDATE service_heartbeats SET last_run_at = now() - interval '5 minutes' WHERE component = 'scheduler'`)

	f.tick(s)

	lease, err := f.store.GetLease(f.ctx, live.Lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ReleasedAt != nil {
		t.Fatalf("the outage reclaimed a live worker's lease (%s)", lease.ReleaseReason)
	}
	if time.Until(lease.ExpiresAt) < 4*time.Minute {
		t.Errorf("lease expires in %s; want the 5-minute gap added back", time.Until(lease.ExpiresAt).Round(time.Second))
	}
	if err := f.store.AssertFence(f.ctx, live.Fence); err != nil {
		t.Errorf("the worker's fence is no longer valid after the outage: %v", err)
	}
	if n := f.countEvents("lease.expired"); n != 0 {
		t.Errorf("%d lease.expired events after an outage", n)
	}

	// Without an outage, a worker that stopped heartbeating is still reclaimed on time.
	dead := f.claim("worker died")
	f.exec(`UPDATE leases SET expires_at = now() - interval '1 second' WHERE id = $1::uuid`, dead.Lease.ID)
	f.tick(s)
	lease, err = f.store.GetLease(f.ctx, dead.Lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ReleasedAt == nil || lease.ReleaseReason != "expired" {
		t.Errorf("a dead worker's lease was not reclaimed: released=%v reason=%q", lease.ReleasedAt, lease.ReleaseReason)
	}
}

// A database no scheduler has ever ticked against (first start after upgrading) gives every
// open lease one full TTL from now, since the length of the outage is unknown.
func TestFirstTickGivesOpenLeasesAFullTTL(t *testing.T) {
	f := newFixture(t, domain.DefaultProjectConfig())
	c := f.claim("claimed under the previous version")
	f.exec(`UPDATE leases SET heartbeat_at = now() - interval '120 seconds',
	                          expires_at   = now() - interval '30 seconds' WHERE id = $1::uuid`, c.Lease.ID)

	rec, err := f.scheduler("replica-a").RecoverOutage(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.FirstTick || rec.LeasesExtended != 1 {
		t.Errorf("recovery = %+v", rec)
	}
	lease, _ := f.store.GetLease(f.ctx, c.Lease.ID)
	if left := time.Until(lease.ExpiresAt); left < 80*time.Second || left > 100*time.Second {
		t.Errorf("lease expires in %s, want about one 90s TTL", left)
	}
}

// Replicas restarting together after an outage extend leases once between them, and a
// replica that stayed up means there was no outage at all.
func TestReplicasExtendOnce(t *testing.T) {
	f := newFixture(t, domain.DefaultProjectConfig())
	a, b := f.scheduler("replica-a"), f.scheduler("replica-b")
	f.tick(a)
	c := f.claim("shared")
	f.exec(`UPDATE service_heartbeats SET last_run_at = now() - interval '10 minutes' WHERE component = 'scheduler'`)

	var wg sync.WaitGroup
	results := make([]db.OutageRecovery, 2)
	for i, s := range []*Scheduler{a, b} {
		wg.Add(1)
		go func(i int, s *Scheduler) {
			defer wg.Done()
			var err error
			results[i], err = s.RecoverOutage(f.ctx)
			if err != nil {
				t.Error(err)
			}
		}(i, s)
	}
	wg.Wait()
	if total := results[0].LeasesExtended + results[1].LeasesExtended; total != 1 {
		t.Errorf("leases extended %d times between two replicas, want once: %+v", total, results)
	}
	lease, _ := f.store.GetLease(f.ctx, c.Lease.ID)
	if left := time.Until(lease.ExpiresAt); left > 12*time.Minute {
		t.Errorf("lease extended twice: expires in %s", left)
	}

	// b keeps ticking; a dead worker on a's watch is reclaimed by b without any extension.
	f.exec(`UPDATE leases SET expires_at = now() - interval '1 second' WHERE id = $1::uuid`, c.Lease.ID)
	rec, err := b.RecoverOutage(f.ctx)
	if err != nil || rec.LeasesExtended != 0 {
		t.Errorf("recovery with a live replica = %+v %v", rec, err)
	}
}

// ---------------------------------------------------------------------------
// Budget alerts
// ---------------------------------------------------------------------------

func TestBudgetEventsOnlyOnLevelChange(t *testing.T) {
	cfg := domain.DefaultProjectConfig()
	cfg.Budget = domain.BudgetPolicy{MonthlyUSD: 10, DownshiftAt: 0.5, PauseAt: 0.9}
	f := newFixture(t, cfg)
	c := f.claim("spends money")
	setSpend := func(usd float64) {
		f.exec(`UPDATE attempts SET cost_usd = $2 WHERE id = $1::uuid`, c.Attempt.ID, usd)
	}

	setSpend(6) // 60%: past downshift
	s := f.scheduler("replica-a")
	for i := 0; i < 3; i++ {
		f.tick(s)
	}
	if n := f.countEvents("budget.downshift"); n != 1 {
		t.Fatalf("%d budget.downshift events after three ticks over the threshold, want 1", n)
	}

	// A restart, and a second replica, do not announce it again.
	f.tick(f.scheduler("replica-a"))
	f.tick(f.scheduler("replica-b"))
	if n := f.countEvents("budget.downshift"); n != 1 {
		t.Errorf("%d budget.downshift events after a restart and a second replica, want 1", n)
	}

	setSpend(9.5) // 95%: past pause
	f.tick(s)
	f.tick(s)
	if n := f.countEvents("budget.exhausted"); n != 1 {
		t.Errorf("%d budget.exhausted events, want 1", n)
	}

	// Spend falls back under (a new month, a raised budget); the next crossing is news.
	setSpend(0)
	f.tick(s)
	setSpend(6)
	f.tick(s)
	if n := f.countEvents("budget.downshift"); n != 2 {
		t.Errorf("%d budget.downshift events after falling back and crossing again, want 2", n)
	}
}

// ---------------------------------------------------------------------------
// Stall detection
// ---------------------------------------------------------------------------

func TestStallReportedOncePerEpisode(t *testing.T) {
	cfg := domain.DefaultProjectConfig()
	cfg.StalledTurnTimeout = domain.Duration(time.Minute)
	f := newFixture(t, cfg)
	c := f.claim("hangs")
	s := f.scheduler("replica-a")

	f.exec(`UPDATE attempts SET last_event_at = now() - interval '5 minutes' WHERE id = $1::uuid`, c.Attempt.ID)
	rep, err := s.TickProject(f.ctx, f.project)
	if err != nil || rep.StallsDetected != 1 {
		t.Fatalf("first pass: %+v %v", rep, err)
	}
	f.tick(s)
	f.tick(s)
	if n := f.countEvents("attempt.stalled"); n != 1 {
		t.Errorf("%d attempt.stalled events for one stall, want 1", n)
	}
	// A restarted scheduler and a second replica know it was already announced.
	f.tick(f.scheduler("replica-a"))
	f.tick(f.scheduler("replica-b"))
	if n := f.countEvents("attempt.stalled"); n != 1 {
		t.Errorf("%d attempt.stalled events after a restart and a second replica, want 1", n)
	}

	// It recovers, then stalls again: a new episode is reported.
	f.exec(`UPDATE attempts SET last_event_at = now() WHERE id = $1::uuid`, c.Attempt.ID)
	f.tick(s)
	f.exec(`UPDATE attempts SET last_event_at = now() - interval '5 minutes' WHERE id = $1::uuid`, c.Attempt.ID)
	f.tick(s)
	if n := f.countEvents("attempt.stalled"); n != 2 {
		t.Errorf("%d attempt.stalled events after a second stall, want 2", n)
	}

	// Replicas ticking at the same moment announce a new stall once between them.
	f.exec(`UPDATE attempts SET last_event_at = now() WHERE id = $1::uuid`, c.Attempt.ID)
	f.tick(s)
	f.exec(`UPDATE attempts SET last_event_at = now() - interval '5 minutes' WHERE id = $1::uuid`, c.Attempt.ID)
	var wg sync.WaitGroup
	for _, holder := range []string{"replica-a", "replica-b", "replica-c"} {
		wg.Add(1)
		go func(r *Scheduler) {
			defer wg.Done()
			if _, err := r.TickProject(f.ctx, f.project); err != nil {
				t.Error(err)
			}
		}(f.scheduler(holder))
	}
	wg.Wait()
	if n := f.countEvents("attempt.stalled"); n != 3 {
		t.Errorf("%d attempt.stalled events after three replicas raced on a third stall, want 3", n)
	}
}

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

func TestPruneHonoursRetention(t *testing.T) {
	f := newFixture(t, domain.DefaultProjectConfig())
	ctx := f.ctx

	// One aggregate with three events, all old. The newest must survive: sequence numbers
	// continue from it.
	agg := f.project.ID
	for i := 0; i < 3; i++ {
		if err := f.store.AppendEvent(ctx, f.org.ID, f.project.ID, "", "project", agg, "test.old", domain.VisibilityTeamSummary, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh event on another aggregate stays.
	if err := f.store.AppendEvent(ctx, f.org.ID, f.project.ID, "", "task", f.alice.ID, "test.fresh", domain.VisibilityTeamSummary, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE domain_events SET occurred_at = now() - interval '100 days' WHERE event_type = 'test.old'`)
	// The first old event was delivered long ago; the second has a pending outbox row that is
	// still inside the undelivered cap, so it waits for its consumer.
	f.exec(`UPDATE outbox_events o SET delivered_at = now() - interval '20 days', created_at = now() - interval '100 days'
	          FROM domain_events e WHERE o.event_id = e.id AND e.event_type = 'test.old' AND e.sequence_number = 1`)
	f.exec(`UPDATE outbox_events o SET created_at = now() - interval '5 days'
	          FROM domain_events e WHERE o.event_id = e.id AND e.event_type = 'test.old' AND e.sequence_number = 2`)
	// The third's outbox row was never delivered and is past the cap.
	f.exec(`UPDATE outbox_events o SET created_at = now() - interval '40 days'
	          FROM domain_events e WHERE o.event_id = e.id AND e.event_type = 'test.old' AND e.sequence_number = 3`)

	f.store.Audit(ctx, f.org.ID, f.project.ID, f.alice.ID, "test.old", "project", f.project.ID, nil)
	f.store.Audit(ctx, f.org.ID, f.project.ID, f.alice.ID, "test.recent", "project", f.project.ID, nil)
	f.exec(`UPDATE audit_log SET created_at = now() - interval '400 days' WHERE action = 'test.old'`)
	f.exec(`UPDATE audit_log SET created_at = now() - interval '200 days' WHERE action = 'test.recent'`)

	for _, k := range []string{"old", "new"} {
		if err := f.store.RememberIdempotent(ctx, k, f.alice.ID, "h", 200, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(`UPDATE idempotency_keys SET created_at = now() - interval '2 days' WHERE key = 'old'`)

	s := New(f.store, coord.New(f.store), Options{Logger: quiet, Retention: DefaultRetention()})
	if err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}

	count := func(sql string, args ...any) int {
		var n int
		if err := f.store.Pool().QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if seqs := count(`SELECT count(*) FROM domain_events WHERE event_type = 'test.old'`); seqs != 2 {
		t.Errorf("%d old events left, want 2 (the pending one and the aggregate's newest)", seqs)
	}
	if n := count(`SELECT count(*) FROM domain_events WHERE event_type = 'test.old' AND sequence_number IN (2, 3)`); n != 2 {
		t.Error("retention deleted the aggregate's newest event or one still waiting for its consumer")
	}
	if n := count(`SELECT count(*) FROM domain_events WHERE event_type = 'test.fresh'`); n != 1 {
		t.Error("retention deleted a fresh event")
	}
	if n := count(`SELECT count(*) FROM outbox_events o JOIN domain_events e ON e.id = o.event_id
	                WHERE e.event_type = 'test.old' AND e.sequence_number = 3`); n != 0 {
		t.Error("an undelivered outbox row past the cap was kept")
	}
	if n := count(`SELECT count(*) FROM audit_log WHERE action LIKE 'test.%'`); n != 1 {
		t.Errorf("%d audit rows left, want 1 (inside the year)", n)
	}
	if n := count(`SELECT count(*) FROM idempotency_keys`); n != 1 {
		t.Errorf("%d idempotency keys left, want 1", n)
	}

	// Next sequence number continues after the kept newest event, not from 1.
	if err := f.store.AppendEvent(ctx, f.org.ID, f.project.ID, "", "project", agg, "test.after", domain.VisibilityTeamSummary, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT sequence_number FROM domain_events WHERE event_type = 'test.after'`); n != 4 {
		t.Errorf("sequence after pruning = %d, want 4", n)
	}
}

// A zero window keeps rows forever.
func TestPruneZeroKeepsEverything(t *testing.T) {
	f := newFixture(t, domain.DefaultProjectConfig())
	for i := 0; i < 2; i++ {
		_ = f.store.AppendEvent(f.ctx, f.org.ID, f.project.ID, "", "project", f.project.ID, "test.old", domain.VisibilityTeamSummary, map[string]any{})
	}
	f.exec(`UPDATE domain_events SET occurred_at = now() - interval '10 years'`)
	report, err := f.store.Prune(f.ctx, db.RetentionPolicy{})
	if err != nil || report.Total() != 0 {
		t.Errorf("prune with no windows = %v %v", report, err)
	}
}
