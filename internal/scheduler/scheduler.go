// Package scheduler is the control plane's background loop (DESIGN.md §7.7).
//
// Every step is transactional and idempotent, so replicas are safe without leader election:
// `FOR UPDATE SKIP LOCKED` means two schedulers cannot select the same row, and a step that
// runs twice produces the same state as running once (§28.3). What would differ between
// replicas — whether the control plane was down, which budget level was last announced — is
// kept in the database, not in the process.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/metrics"
)

var (
	tickDuration = metrics.Default.NewHistogram("conductor_scheduler_tick_duration_seconds",
		"Duration of one scheduler pass over every project.", metrics.DurationBuckets)
	tickErrors = metrics.Default.NewCounter("conductor_scheduler_errors_total",
		"Scheduler failures, by stage (tick, project, prune, budget, queue).", "stage")
	lastTick = metrics.Default.NewGauge("conductor_scheduler_last_tick_timestamp_seconds",
		"Unix time this process last completed a scheduler pass.")
	leasesReclaimed = metrics.Default.NewCounter("conductor_leases_reclaimed_total",
		"Leases reclaimed after expiring.")
	leasesExtended = metrics.Default.NewCounter("conductor_leases_extended_total",
		"Open leases extended to cover a control-plane outage.")
	leasesActive = metrics.Default.NewGauge("conductor_leases_active",
		"Open leases across all projects, as of the last scheduler pass.")
	retentionDeleted = metrics.Default.NewCounter("conductor_retention_deleted_total",
		"Rows deleted by retention, by table.", "table")
)

// Options configures a scheduler.
type Options struct {
	Tick time.Duration
	// DetectEvery throttles conflict-graph recomputation, which is O(n²) in open tasks and
	// does not need to run at the same cadence as lease reconciliation.
	DetectEvery time.Duration
	// TickTimeout bounds one pass. A hung query fails the pass (and the database's own
	// statement_timeout fails the query) instead of stalling every later pass behind it.
	TickTimeout time.Duration
	// OutageAfter is how long no replica may have ticked before the next tick treats the
	// silence as an outage and extends open leases by it (db.BeginSchedulerTick). It must
	// exceed the longest normal gap between passes, TickTimeout plus a tick or two.
	OutageAfter time.Duration
	// Retention says how long rows are kept; PruneEvery how often retention runs.
	Retention  db.RetentionPolicy
	PruneEvery time.Duration
	// Holder names this process in the shared heartbeat (default host:pid).
	Holder string
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Tick <= 0 {
		o.Tick = 2 * time.Second
	}
	if o.DetectEvery <= 0 {
		o.DetectEvery = 15 * time.Second
	}
	if o.TickTimeout <= 0 {
		o.TickTimeout = 30 * time.Second
	}
	if o.OutageAfter <= 0 {
		o.OutageAfter = max(o.TickTimeout+3*o.Tick, 30*time.Second)
	}
	if o.PruneEvery <= 0 {
		o.PruneEvery = 10 * time.Minute
	}
	if o.Holder == "" {
		host, _ := os.Hostname()
		o.Holder = host + ":" + strconv.Itoa(os.Getpid())
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// DefaultRetention is what conductord keeps unless told otherwise.
func DefaultRetention() db.RetentionPolicy {
	const day = 24 * time.Hour
	return db.RetentionPolicy{
		Events:            90 * day,
		Audit:             365 * day,
		OutboxDelivered:   7 * day,
		OutboxUndelivered: 30 * day,
		Idempotency:       day,
		// Budgets look back 30 days; a usage window shorter than that would silently
		// under-count spend.
		Usage:     180 * day,
		CheckRuns: 30 * day,
	}
}

// Scheduler runs the reconcile / unblock / detect cycle for every project.
type Scheduler struct {
	store *db.Store
	svc   *coord.Service
	opts  Options

	mu         sync.Mutex
	lastDetect map[domain.ID]time.Time
	stalled    map[domain.ID]bool
	lastPrune  time.Time
}

func New(store *db.Store, svc *coord.Service, opts Options) *Scheduler {
	return &Scheduler{
		store:      store,
		svc:        svc,
		opts:       opts.withDefaults(),
		lastDetect: map[domain.ID]time.Time{},
		stalled:    map[domain.ID]bool{},
	}
}

// Run ticks until the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.opts.Tick)
	defer ticker.Stop()

	s.opts.Logger.Info("scheduler started", "tick", s.opts.Tick.String(),
		"tick_timeout", s.opts.TickTimeout.String(), "outage_after", s.opts.OutageAfter.String())
	for {
		select {
		case <-ctx.Done():
			s.opts.Logger.Info("scheduler stopped")
			return ctx.Err()
		case <-ticker.C:
			if err := s.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				// A failing tick is logged and retried. Exiting the loop would leave leases
				// unreclaimed, which is strictly worse than a noisy log.
				s.opts.Logger.Error("scheduler tick failed", "error", err)
			}
		}
	}
}

// TickReport summarizes one pass, for tests and observability.
type TickReport struct {
	Projects        int
	LeasesReclaimed int
	TasksUnblocked  int
	TasksBlocked    int
	StallsDetected  int
	ConflictEdges   int
	SessionsReaped  int
	OffersExpired   int
	TicketsExpired  int
	TicketsGranted  int
}

// Tick runs one full cycle across all projects, bounded by TickTimeout.
func (s *Scheduler) Tick(ctx context.Context) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, s.opts.TickTimeout)
	defer cancel()
	err := s.tick(ctx)
	tickDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		tickErrors.Inc("tick")
	} else {
		lastTick.Set(float64(time.Now().Unix()))
	}
	return err
}

func (s *Scheduler) tick(ctx context.Context) error {
	// Outage recovery comes first, and nothing is reclaimed without it: if the shared
	// heartbeat cannot be read, there is no telling whether workers could renew their leases.
	if _, err := s.RecoverOutage(ctx); err != nil {
		return fmt.Errorf("record scheduler tick: %w", err)
	}

	projects, err := s.allProjects(ctx)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.TickProject(ctx, p); err != nil {
			tickErrors.Inc("project")
			s.opts.Logger.Error("project tick failed", "project", p.Slug, "error", err)
		}
	}
	if _, err := s.store.PruneIntents(ctx); err != nil {
		s.opts.Logger.Warn("prune intents failed", "error", err)
	}
	if _, err := s.store.MarkStaleRunners(ctx, 2*time.Minute); err != nil {
		s.opts.Logger.Warn("mark stale runners failed", "error", err)
	}
	if s.pruneDue() {
		if err := s.Prune(ctx); err != nil {
			tickErrors.Inc("prune")
			s.opts.Logger.Warn("retention failed", "error", err)
		}
	}
	if n, err := s.store.ActiveLeaseCount(ctx); err == nil {
		leasesActive.Set(float64(n))
	}
	return nil
}

// RecoverOutage stamps the shared scheduler heartbeat and, if no replica has stamped it for
// longer than OutageAfter, extends open leases by the gap (db.BeginSchedulerTick). Every
// tick starts with it. conductord also calls it once before it starts accepting requests —
// even with --no-scheduler — because a worker whose lease expired during the outage would
// otherwise be told lease_not_held by its first heartbeat, before any tick had run.
func (s *Scheduler) RecoverOutage(ctx context.Context) (db.OutageRecovery, error) {
	recovery, err := s.store.BeginSchedulerTick(ctx, s.opts.Holder, s.opts.OutageAfter)
	if err != nil {
		return recovery, err
	}
	if recovery.LeasesExtended > 0 {
		leasesExtended.Add(float64(recovery.LeasesExtended))
		s.opts.Logger.Warn("no scheduler ran for a while; extended open leases so their holders can renew",
			"gap", recovery.Gap.Round(time.Second).String(), "first_tick", recovery.FirstTick,
			"leases", recovery.LeasesExtended)
	}
	return recovery, nil
}

func (s *Scheduler) pruneDue() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.lastPrune) < s.opts.PruneEvery {
		return false
	}
	s.lastPrune = time.Now()
	return true
}

// Prune applies the retention policy once. Each kind is deleted in bounded batches, so a
// large backlog (the first run on an old database) drains over several passes rather than
// in one long transaction. Replicas may both prune; deleting what is already gone is a no-op.
func (s *Scheduler) Prune(ctx context.Context) error {
	report, err := s.store.Prune(ctx, s.opts.Retention)
	for table, n := range report {
		retentionDeleted.Add(float64(n), table)
	}
	if total := report.Total(); total > 0 {
		s.opts.Logger.Info("retention pruned rows", "rows", total, "by_table", map[string]int64(report))
	}
	return err
}

// TickProject runs one cycle for a single project.
func (s *Scheduler) TickProject(ctx context.Context, project domain.Project) (TickReport, error) {
	report := TickReport{Projects: 1}
	cfg := project.Config

	// 1. Reconcile: reclaim expired leases, release their territory, requeue or fail.
	reclaimed, err := s.store.ReconcileLeases(ctx, project.ID)
	if err != nil {
		return report, err
	}
	report.LeasesReclaimed = len(reclaimed)
	leasesReclaimed.Add(float64(len(reclaimed)))
	for _, r := range reclaimed {
		s.opts.Logger.Warn("lease reclaimed",
			"task", r.TaskRef, "attempt", r.AttemptID, "next_status", r.NextStatus,
			// The worktree is deliberately retained; log it so a human can adopt the branch.
			"worktree", r.Worktree, "branch", r.Branch)
	}

	// 2. Reap sessions that stopped heartbeating.
	reaped, err := s.store.ReapSessions(ctx, cfg.OfflineGrace.OrDefault(45*time.Second))
	if err != nil {
		return report, err
	}
	report.SessionsReaped = int(reaped)

	// 2b. Close offers nobody answered, and offers held by a session that just went stale.
	//     Reaping runs first for a reason: an offer is only released once the session
	//     holding it is known to be gone, and until then the open-offer index would keep any
	//     other session from being given the same task.
	expired, err := s.store.ExpireAssignments(ctx, project.ID)
	if err != nil {
		return report, err
	}
	report.OffersExpired = int(expired)

	// 3. Stall detection, which is separate from lease expiry on purpose: a hung harness
	//    should be visible to a human long before the system reclaims its work (§27.3).
	stalls, err := s.detectStalls(ctx, project)
	if err != nil {
		return report, err
	}
	report.StallsDetected = stalls

	// 4. Dependency gating in both directions.
	unblocked, err := s.store.UnblockReadyTasks(ctx, project.ID)
	if err != nil {
		return report, err
	}
	report.TasksUnblocked = len(unblocked)
	for _, id := range unblocked {
		s.emit(ctx, project, id, "task.unblocked", map[string]any{"status": "ready"})
	}

	blocked, err := s.store.BlockTasksWithUnmetDependencies(ctx, project.ID)
	if err != nil {
		return report, err
	}
	report.TasksBlocked = len(blocked)
	for _, id := range blocked {
		s.emit(ctx, project, id, "task.blocked",
			map[string]any{"status": "blocked_dependency", "reason": "dependency"})
	}

	// 5. Conflict graph, throttled.
	if s.shouldDetect(project.ID) {
		edges, err := s.svc.DetectConflicts(ctx, project.ID)
		if err != nil {
			return report, err
		}
		report.ConflictEdges = edges
	}

	// 6. Admission queue: expire abandoned tickets, release slots whose session or attempt
	//    ended, and grant what the freed slots allow. Only when the project caps something —
	//    an unlimited project has no queue to service.
	if project.Config.Queue.Enabled() || project.Config.Queue.MaxConcurrentAttempts > 0 {
		expired, granted, err := s.store.ReconcileQueue(ctx, project.ID, project.Config.Queue)
		if err != nil {
			tickErrors.Inc("queue")
			s.opts.Logger.Warn("queue reconcile failed", "project", project.Slug, "error", err)
		} else {
			report.TicketsExpired = int(expired)
			report.TicketsGranted = int(granted)
			if granted > 0 {
				s.emit(ctx, project, "", "queue.granted", map[string]any{"granted": int(granted)})
			}
		}
	}

	// 7. Budget.
	if err := s.checkBudget(ctx, project); err != nil {
		tickErrors.Inc("budget")
		s.opts.Logger.Warn("budget check failed", "project", project.Slug, "error", err)
	}
	return report, nil
}

// detectStalls emits an event the first time an attempt goes quiet, and clears the flag when
// it comes back. Emitting on every tick would bury the dashboard in duplicates.
func (s *Scheduler) detectStalls(ctx context.Context, project domain.Project) (int, error) {
	stallAfter := project.Config.StalledTurnTimeout.OrDefault(900 * time.Second)
	attempts, err := s.store.StalledAttempts(ctx, project.ID, stallAfter.String())
	if err != nil {
		return 0, err
	}

	current := map[domain.ID]bool{}
	newly := 0
	for _, a := range attempts {
		current[a.ID] = true

		s.mu.Lock()
		already := s.stalled[a.ID]
		s.stalled[a.ID] = true
		s.mu.Unlock()

		if already {
			continue
		}
		newly++
		task, err := s.store.GetTask(ctx, a.TaskID)
		if err != nil {
			continue
		}
		s.opts.Logger.Warn("attempt stalled",
			"task", task.Ref, "attempt", a.ID, "harness", a.Harness,
			"silent_for", time.Since(a.LastEventAt).Round(time.Second).String())
		_ = s.store.AppendEvent(ctx, project.OrganizationID, project.ID, "",
			"attempt", a.ID, "attempt.stalled", domain.VisibilityTeamSummary, map[string]any{
				"task_ref": task.Ref, "harness": a.Harness,
				"reason": "no harness event within the stall window",
			})
	}

	// Forget attempts that recovered or ended, so a later stall is reported again.
	s.mu.Lock()
	for id := range s.stalled {
		if !current[id] {
			delete(s.stalled, id)
		}
	}
	s.mu.Unlock()
	return newly, nil
}

func (s *Scheduler) shouldDetect(projectID domain.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.lastDetect[projectID]
	if time.Since(last) < s.opts.DetectEvery {
		return false
	}
	s.lastDetect[projectID] = time.Now()
	return true
}

// checkBudget announces when a project crosses a spend threshold. The router applies the
// actual downshift at routing time; this exists so humans find out before their work
// silently gets cheaper.
//
// The level is evaluated every tick but announced only when it changes, and the last level
// announced is kept in the database: once per crossing, not once per tick, per restart, or
// per replica.
func (s *Scheduler) checkBudget(ctx context.Context, project domain.Project) error {
	policy := project.Config.Budget
	if policy.MonthlyUSD <= 0 {
		return nil
	}
	spent, err := s.store.SpendSince(ctx, project.ID, "30 days")
	if err != nil {
		return err
	}
	fraction := spent / policy.MonthlyUSD

	level, reason := db.BudgetLevelNone, ""
	switch {
	case policy.PauseAt > 0 && fraction >= policy.PauseAt:
		level, reason = db.BudgetLevelExhausted, "pause threshold reached"
	case policy.DownshiftAt > 0 && fraction >= policy.DownshiftAt:
		level, reason = db.BudgetLevelDownshift, "downshift threshold reached"
	}
	changed, err := s.store.SetBudgetAlertLevel(ctx, project.OrganizationID, project.ID, level,
		map[string]any{"cost_usd": spent, "reason": reason})
	if err != nil {
		return err
	}
	if changed && level != db.BudgetLevelNone {
		s.opts.Logger.Warn("project budget threshold crossed",
			"project", project.Slug, "level", level, "cost_usd", spent, "monthly_usd", policy.MonthlyUSD)
	}
	return nil
}

func (s *Scheduler) emit(ctx context.Context, project domain.Project, taskID domain.ID, eventType string, payload map[string]any) {
	aggregateType, aggregateID := "project", project.ID
	if taskID != "" {
		aggregateType, aggregateID = "task", taskID
		if task, err := s.store.GetTask(ctx, taskID); err == nil {
			payload["task_ref"] = task.Ref
		}
	}
	if err := s.store.AppendEvent(ctx, project.OrganizationID, project.ID, "",
		aggregateType, aggregateID, eventType, domain.VisibilityTeamSummary, payload); err != nil {
		s.opts.Logger.Warn("emit event failed", "type", eventType, "error", err)
	}
}

// allProjects lists every project the scheduler must service.
func (s *Scheduler) allProjects(ctx context.Context) ([]domain.Project, error) {
	rows, err := s.store.Pool().Query(ctx, `SELECT id::text FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]domain.Project, 0, len(ids))
	for _, id := range ids {
		p, err := s.store.GetProject(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}
