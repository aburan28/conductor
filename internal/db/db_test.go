package db

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
	"github.com/adamburan/conductor/internal/resource"
)

// Integration tests need a live Postgres. `make db-up` starts one; without DATABASE_URL the
// suite skips rather than failing, so `go test ./...` stays useful on a machine with no
// database.
//
// Isolation is by data, not by schema: every test allocates its own organization and
// project, and all queries are project-scoped. That keeps the suite parallel-safe without
// paying for a CREATE DATABASE per test.

var (
	sharedStore *Store
	storeOnce   sync.Once
	storeErr    error
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration tests")
	}
	storeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		sharedStore, storeErr = Open(ctx, dsn)
		if storeErr != nil {
			return
		}
		storeErr = sharedStore.Migrate(ctx)
	})
	if storeErr != nil {
		t.Fatalf("store setup: %v", storeErr)
	}
	return sharedStore
}

// fixture is one isolated tenant with a project and two principals.
type fixture struct {
	store   *Store
	ctx     context.Context
	org     domain.Organization
	project domain.Project
	alice   domain.Principal
	bob     domain.Principal
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	org, err := s.CreateOrganization(ctx, uniq("org", suffix), "Test Org")
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	alice, err := s.CreatePrincipal(ctx, org.ID, domain.PrincipalHuman, "alice", "Alice", "")
	if err != nil {
		t.Fatalf("CreatePrincipal alice: %v", err)
	}
	bob, err := s.CreatePrincipal(ctx, org.ID, domain.PrincipalHuman, "bob", "Bob", "")
	if err != nil {
		t.Fatalf("CreatePrincipal bob: %v", err)
	}
	project, err := s.CreateProject(ctx, CreateProjectParams{
		OrganizationID: org.ID,
		Slug:           uniq("proj", suffix),
		DisplayName:    "Test Project",
		Config:         domain.DefaultProjectConfig(),
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	for _, p := range []domain.Principal{alice, bob} {
		if err := s.AddMember(ctx, project.ID, p.ID, domain.RoleContributor); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}
	return &fixture{store: s, ctx: ctx, org: org, project: project, alice: alice, bob: bob}
}

func uniq(prefix string, n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var buf []byte
	for n > 0 {
		buf = append([]byte{digits[n%36]}, buf...)
		n /= 36
	}
	return prefix + "-" + string(buf)
}

func (f *fixture) newTask(t *testing.T, title string, scopes ...domain.ScopeRequest) domain.Task {
	t.Helper()
	task, err := f.store.CreateTask(f.ctx, CreateTaskParams{
		ProjectID: f.project.ID,
		CreatedBy: f.alice.ID,
		Title:     title,
		Status:    domain.TaskReady,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task
}

func (f *fixture) claimParams(task domain.Task, holder domain.Principal, scopes ...domain.ScopeRequest) ClaimParams {
	return ClaimParams{
		TaskID:          task.ID,
		ProjectID:       f.project.ID,
		HolderPrincipal: holder.ID,
		Harness:         "test",
		ModelAlias:      "worker.fast",
		LeaseTTL:        90 * time.Second,
		Scopes:          scopes,
		ScopePolicy:     resource.DefaultPolicy(),
	}
}

// ---------------------------------------------------------------------------
// DESIGN.md §31.1 / §32.2 — exactly one active lease per task
// ---------------------------------------------------------------------------

func TestConcurrentClaimsYieldExactlyOneLease(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Contested task")

	const racers = 24
	var wg sync.WaitGroup
	results := make([]error, racers)
	claims := make([]ClaimResult, racers)

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines at once to maximize contention
			claims[i], results[i] = f.store.Claim(f.ctx, f.claimParams(task, f.alice))
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	var winner ClaimResult
	for i, err := range results {
		switch {
		case err == nil:
			winners++
			winner = claims[i]
		case errors.Is(err, domain.ErrAlreadyClaimed), errors.Is(err, domain.ErrIllegalTransition):
			// Expected loss.
		default:
			t.Errorf("racer %d got unexpected error: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d concurrent claims produced %d winners, want exactly 1", racers, winners)
	}

	// The database must agree there is one live lease.
	var live int
	if err := f.store.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM leases WHERE task_id = $1::uuid AND released_at IS NULL`,
		task.ID).Scan(&live); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if live != 1 {
		t.Errorf("live leases = %d, want 1", live)
	}
	if winner.Fence.FencingEpoch != 1 {
		t.Errorf("first claim epoch = %d, want 1", winner.Fence.FencingEpoch)
	}

	// And exactly one attempt was created, so the retry budget was charged once.
	fresh, err := f.store.GetTask(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if fresh.AttemptsCount != 1 {
		t.Errorf("attempts_count = %d, want 1", fresh.AttemptsCount)
	}
}

// ---------------------------------------------------------------------------
// DESIGN.md §31.4 / §10.3 — a stale worker cannot mutate canonical state
// ---------------------------------------------------------------------------

func TestStaleFenceIsRejected(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Fenced task")

	first, err := f.store.Claim(f.ctx, f.claimParams(task, f.alice))
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// The lease expires and the reconciler takes it back.
	if _, err := f.store.pool.Exec(f.ctx,
		`UPDATE leases SET expires_at = now() - interval '1 minute' WHERE id = $1::uuid`,
		first.Lease.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	reclaimed, err := f.store.ReconcileLeases(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ReconcileLeases: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("reclaimed %d leases, want 1", len(reclaimed))
	}

	// The old holder is now stale and must be refused on every mutation path.
	if err := f.store.AssertFence(f.ctx, first.Fence); !errors.Is(err, domain.ErrStaleFencing) {
		t.Errorf("AssertFence with stale epoch = %v, want ErrStaleFencing", err)
	}
	if _, err := f.store.HeartbeatLease(f.ctx, first.Fence, time.Minute); !errors.Is(err, domain.ErrStaleFencing) {
		t.Errorf("HeartbeatLease with stale epoch = %v, want ErrStaleFencing", err)
	}
	err = f.store.SubmitEvidence(f.ctx, domain.EvidenceManifest{
		TaskID: first.Fence.TaskID, AttemptID: first.Fence.AttemptID,
		LeaseID: first.Fence.LeaseID, FencingEpoch: first.Fence.FencingEpoch,
		CommitSHA: "deadbeef",
	}, "")
	if !errors.Is(err, domain.ErrStaleFencing) {
		t.Errorf("SubmitEvidence with stale epoch = %v, want ErrStaleFencing", err)
	}

	// A fresh claim gets a strictly higher epoch (DESIGN.md §31.3).
	second, err := f.store.Claim(f.ctx, f.claimParams(task, f.bob))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second.Fence.FencingEpoch <= first.Fence.FencingEpoch {
		t.Errorf("second epoch %d must exceed first %d",
			second.Fence.FencingEpoch, first.Fence.FencingEpoch)
	}
	if err := f.store.AssertFence(f.ctx, second.Fence); err != nil {
		t.Errorf("fresh fence should be valid: %v", err)
	}
}

// A reclaimed attempt must release its territory so a teammate is unblocked.
func TestReclamationReleasesReservations(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Holder")

	claim, err := f.store.Claim(f.ctx, f.claimParams(task, f.alice,
		domain.ScopeRequest{Resource: "dir:internal/router", Mode: domain.ModeWriteExclusive}))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claim.Reservations) != 1 {
		t.Fatalf("reservations = %d, want 1", len(claim.Reservations))
	}

	if _, err := f.store.pool.Exec(f.ctx,
		`UPDATE leases SET expires_at = now() - interval '1 minute' WHERE id = $1::uuid`,
		claim.Lease.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, err := f.store.ReconcileLeases(f.ctx, f.project.ID); err != nil {
		t.Fatalf("ReconcileLeases: %v", err)
	}

	held, err := f.store.ReservationsForTask(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("ReservationsForTask: %v", err)
	}
	if len(held) != 0 {
		t.Errorf("reclaimed task still holds %d reservation(s)", len(held))
	}

	// The task is claimable again, and the worktree reference survived for recovery.
	fresh, err := f.store.GetTask(f.ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if fresh.Status != domain.TaskReady {
		t.Errorf("status after reclamation = %s, want ready", fresh.Status)
	}
	attempt, err := f.store.GetAttempt(f.ctx, claim.Attempt.ID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	if attempt.State != domain.AttemptStale {
		t.Errorf("attempt state = %s, want stale", attempt.State)
	}
}

// ---------------------------------------------------------------------------
// DESIGN.md §31.2 — overlapping write scopes get a deterministic conflict
// ---------------------------------------------------------------------------

func TestOverlappingWriteScopesConflict(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "Router work")
	b := f.newTask(t, "Router tests")

	if _, err := f.store.Claim(f.ctx, f.claimParams(a, f.alice,
		domain.ScopeRequest{Resource: "dir:internal/router", Mode: domain.ModeWriteExclusive})); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// A path inside the reserved directory must be refused.
	_, err := f.store.Claim(f.ctx, f.claimParams(b, f.bob,
		domain.ScopeRequest{Resource: "path:internal/router/policy.go", Mode: domain.ModeWriteExclusive}))
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("overlapping write claim = %v, want ErrConflict", err)
	}

	// The probe explains who holds it, without needing a claim first.
	conflicts, err := f.store.CheckScopes(f.ctx, CheckScopesParams{
		ProjectID:   f.project.ID,
		ExcludeTask: b.ID,
		Viewer:      f.bob.ID,
		Requests: []domain.ScopeRequest{
			{Resource: "path:internal/router/policy.go", Mode: domain.ModeWriteExclusive}},
		Policy: resource.DefaultPolicy(),
	})
	if err != nil {
		t.Fatalf("CheckScopes: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(conflicts))
	}
	if conflicts[0].HolderOwner != "alice" || conflicts[0].HolderTaskRef != a.Ref {
		t.Errorf("conflict does not identify the holder: %+v", conflicts[0])
	}
	if !conflicts[0].Outcome.Blocks() {
		t.Errorf("outcome = %s, want a blocking outcome", conflicts[0].Outcome)
	}

	// A read of the same directory is allowed with a warning, not blocked.
	c := f.newTask(t, "Read router")
	claim, err := f.store.Claim(f.ctx, func() ClaimParams {
		p := f.claimParams(c, f.bob,
			domain.ScopeRequest{Resource: "dir:internal/router", Mode: domain.ModeReadShared})
		p.AllowWarnings = true
		return p
	}())
	if err != nil {
		t.Fatalf("read_shared claim should be permitted: %v", err)
	}
	if len(claim.Warnings) != 1 {
		t.Errorf("expected one advisory warning, got %d", len(claim.Warnings))
	}
}

// Two concurrent claims for overlapping protected scopes must not both succeed. This is the
// check-then-insert race that the project advisory lock exists to close.
func TestConcurrentOverlappingReservationsSerialize(t *testing.T) {
	f := newFixture(t)

	const racers = 12
	tasks := make([]domain.Task, racers)
	for i := range tasks {
		tasks[i] = f.newTask(t, "Migration work")
	}

	var wg sync.WaitGroup
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = f.store.Claim(f.ctx, f.claimParams(tasks[i], f.alice,
				domain.ScopeRequest{
					Resource: "migration:primary",
					Mode:     domain.ModeProtectedExclusive,
				}))
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, domain.ErrConflict):
		default:
			t.Errorf("racer %d unexpected error: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d concurrent protected reservations produced %d winners, want 1", racers, winners)
	}

	inFlight, err := f.store.ProtectedScopesInFlight(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ProtectedScopesInFlight: %v", err)
	}
	if inFlight != 1 {
		t.Errorf("protected reservations in flight = %d, want 1", inFlight)
	}
}

// ---------------------------------------------------------------------------
// DESIGN.md §32.2 — scheduler replicas do not double dispatch
// ---------------------------------------------------------------------------

func TestClaimNextDoesNotDoubleDispatch(t *testing.T) {
	f := newFixture(t)

	const tasks = 8
	for i := 0; i < tasks; i++ {
		f.newTask(t, "Queued work")
	}

	const schedulers = 6
	var wg sync.WaitGroup
	claimed := make([][]domain.ID, schedulers)
	start := make(chan struct{})

	for s := 0; s < schedulers; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			<-start
			for {
				res, err := f.store.ClaimNext(f.ctx, ClaimNextParams{
					ClaimParams:   f.claimParams(domain.Task{}, f.alice),
					MaxCandidates: tasks,
				})
				if err != nil {
					return // queue drained, or nothing this replica can take
				}
				claimed[s] = append(claimed[s], res.Task.ID)
			}
		}(s)
	}
	close(start)
	wg.Wait()

	seen := map[domain.ID]int{}
	total := 0
	for _, ids := range claimed {
		for _, id := range ids {
			seen[id]++
			total++
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("task %s was dispatched %d times", id, n)
		}
	}
	if total != tasks {
		t.Errorf("dispatched %d tasks, want %d", total, tasks)
	}
}

// ---------------------------------------------------------------------------
// Duplicate detection over HMAC'd signatures
// ---------------------------------------------------------------------------

func TestFindDuplicatesUsesFingerprintsNotText(t *testing.T) {
	f := newFixture(t)

	key, err := f.store.DedupeKeyForProject(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("DedupeKeyForProject: %v", err)
	}

	aliceIntent := privacy.Envelope{
		Summary: "Implement the team invite flow with email invitations and acceptance",
		Scopes:  []string{"dir:internal/invites"},
	}
	task, err := f.store.CreateTask(f.ctx, CreateTaskParams{
		ProjectID:   f.project.ID,
		CreatedBy:   f.alice.ID,
		Title:       "Team invite flow",
		Status:      domain.TaskReady,
		Visibility:  domain.VisibilityPrivate,
		Fingerprint: aliceIntent.Fingerprint(key),
		MinHash:     aliceIntent.Signature(key),
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Bob describes the same work differently.
	bobIntent := privacy.Envelope{
		Summary: "Build team invitation flow: send invite emails, accept invitations",
		Scopes:  []string{"dir:internal/invites"},
	}
	candidates, err := f.store.FindDuplicates(f.ctx, f.project.ID,
		bobIntent.Fingerprint(key), bobIntent.Signature(key), "", 0.40, "")
	if err != nil {
		t.Fatalf("FindDuplicates: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("reworded duplicate was not detected")
	}
	if candidates[0].TaskID != task.ID {
		t.Errorf("top candidate = %s, want %s", candidates[0].TaskID, task.ID)
	}
	if candidates[0].Exact {
		t.Error("a reworded intent should be similar, not an exact fingerprint match")
	}

	// The identical intent is an exact match, which is a stronger signal.
	exact, err := f.store.FindDuplicates(f.ctx, f.project.ID,
		aliceIntent.Fingerprint(key), aliceIntent.Signature(key), "", 0.40, "")
	if err != nil {
		t.Fatalf("FindDuplicates exact: %v", err)
	}
	if len(exact) == 0 || !exact[0].Exact {
		t.Error("identical intent should produce an exact fingerprint match")
	}

	// Unrelated work must not collide.
	unrelated := privacy.Envelope{
		Summary: "Upgrade the Postgres connection pool and add query timeouts",
		Scopes:  []string{"dir:internal/db"},
	}
	none, err := f.store.FindDuplicates(f.ctx, f.project.ID,
		unrelated.Fingerprint(key), unrelated.Signature(key), "", 0.40, "")
	if err != nil {
		t.Fatalf("FindDuplicates unrelated: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unrelated work matched %d task(s)", len(none))
	}
}

// ---------------------------------------------------------------------------
// State machine enforcement at the database boundary
// ---------------------------------------------------------------------------

func TestIllegalTransitionsAreRefused(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "State machine")

	if _, err := f.store.UpdateTaskStatus(f.ctx, task.ID, domain.TaskDone); !errors.Is(err, domain.ErrIllegalTransition) {
		t.Errorf("ready -> done = %v, want ErrIllegalTransition", err)
	}
	if _, err := f.store.UpdateTaskStatus(f.ctx, task.ID, domain.TaskCancelled); err != nil {
		t.Errorf("ready -> cancelled should be legal: %v", err)
	}
	if _, err := f.store.Claim(f.ctx, f.claimParams(task, f.alice)); !errors.Is(err, domain.ErrIllegalTransition) {
		t.Errorf("claiming a cancelled task = %v, want ErrIllegalTransition", err)
	}
}

func TestDependencyGatesClaim(t *testing.T) {
	f := newFixture(t)
	blocker := f.newTask(t, "Blocker")

	dependent, err := f.store.CreateTask(f.ctx, CreateTaskParams{
		ProjectID: f.project.ID,
		CreatedBy: f.alice.ID,
		Title:     "Dependent",
		Status:    domain.TaskReady,
		DependsOn: []domain.ID{blocker.ID},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if _, err := f.store.Claim(f.ctx, f.claimParams(dependent, f.alice)); !errors.Is(err, domain.ErrNotPermitted) {
		t.Errorf("claim with unmet dependency = %v, want ErrNotPermitted", err)
	}

	// Drive the blocker to done, then the dependent becomes claimable.
	for _, st := range []domain.TaskStatus{domain.TaskClaimed, domain.TaskRunning, domain.TaskVerifying, domain.TaskDone} {
		if _, err := f.store.UpdateTaskStatus(f.ctx, blocker.ID, st); err != nil {
			t.Fatalf("blocker -> %s: %v", st, err)
		}
	}
	if _, err := f.store.Claim(f.ctx, f.claimParams(dependent, f.alice)); err != nil {
		t.Errorf("claim after dependency satisfied: %v", err)
	}
}

func TestDependencyCycleIsRejected(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "A")
	b, err := f.store.CreateTask(f.ctx, CreateTaskParams{
		ProjectID: f.project.ID, CreatedBy: f.alice.ID, Title: "B",
		Status: domain.TaskReady, DependsOn: []domain.ID{a.ID},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := f.store.AddDependency(f.ctx, a.ID, b.ID); !errors.Is(err, domain.ErrDependencyCycle) {
		t.Errorf("cycle = %v, want ErrDependencyCycle", err)
	}
}

// ---------------------------------------------------------------------------
// Clean release frees territory immediately
// ---------------------------------------------------------------------------

func TestReleaseFreesTerritory(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "First")
	b := f.newTask(t, "Second")

	claim, err := f.store.Claim(f.ctx, f.claimParams(a, f.alice,
		domain.ScopeRequest{Resource: "path:internal/scheduler/scheduler.go", Mode: domain.ModeWriteExclusive}))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, err := f.store.Release(f.ctx, ReleaseParams{
		Fence:          claim.Fence,
		Reason:         "handing back",
		AttemptState:   domain.AttemptCancelled,
		NextTaskStatus: domain.TaskReady,
	}); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if _, err := f.store.Claim(f.ctx, f.claimParams(b, f.bob,
		domain.ScopeRequest{Resource: "path:internal/scheduler/scheduler.go", Mode: domain.ModeWriteExclusive})); err != nil {
		t.Errorf("released territory should be immediately claimable: %v", err)
	}

	// The stale fence is dead even though the lease was released cleanly.
	if err := f.store.AssertFence(f.ctx, claim.Fence); err == nil {
		t.Error("released lease should not pass the fencing check")
	}
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

func TestEventsAreSequencedAndSanitized(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Events")

	if _, err := f.store.Claim(f.ctx, f.claimParams(task, f.alice)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := f.store.AppendEvent(f.ctx, f.org.ID, f.project.ID, f.alice.ID,
		"task", task.ID, "task.progress", domain.VisibilityTeamSummary, map[string]any{
			"task_ref": task.Ref,
			"phase":    "implementing",
			"prompt":   "this must never be stored",
		}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	events, err := f.store.ListEvents(f.ctx, f.project.ID, 50)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) < 2 {
		t.Fatalf("events = %d, want at least 2", len(events))
	}

	seqByAggregate := map[domain.ID][]int64{}
	for _, e := range events {
		if _, ok := e.Payload["prompt"]; ok {
			t.Error("a prompt reached the event log")
		}
		seqByAggregate[e.AggregateID] = append(seqByAggregate[e.AggregateID], e.SequenceNumber)
	}
	for id, seqs := range seqByAggregate {
		seen := map[int64]bool{}
		for _, n := range seqs {
			if seen[n] {
				t.Errorf("aggregate %s has duplicate sequence number %d", id, n)
			}
			seen[n] = true
		}
	}
}
