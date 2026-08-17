// Package scheduler is the control plane's background loop (DESIGN.md §7.7).
//
// Every step is transactional and idempotent, so replicas are safe without leader election:
// `FOR UPDATE SKIP LOCKED` means two schedulers cannot select the same row, and a step that
// runs twice produces the same state as running once (§28.3).
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// Options configures a scheduler.
type Options struct {
	Tick time.Duration
	// DetectEvery throttles conflict-graph recomputation, which is O(n²) in open tasks and
	// does not need to run at the same cadence as lease reconciliation.
	DetectEvery time.Duration
	Logger      *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Tick <= 0 {
		o.Tick = 2 * time.Second
	}
	if o.DetectEvery <= 0 {
		o.DetectEvery = 15 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Scheduler runs the reconcile / unblock / detect cycle for every project.
type Scheduler struct {
	store *db.Store
	svc   *coord.Service
	opts  Options

	mu         sync.Mutex
	lastDetect map[domain.ID]time.Time
	stalled    map[domain.ID]bool
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

	s.opts.Logger.Info("scheduler started", "tick", s.opts.Tick.String())
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
}

// Tick runs one full cycle across all projects.
func (s *Scheduler) Tick(ctx context.Context) error {
	projects, err := s.allProjects(ctx)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.TickProject(ctx, p); err != nil {
			s.opts.Logger.Error("project tick failed", "project", p.Slug, "error", err)
		}
	}
	if _, err := s.store.PruneIntents(ctx); err != nil {
		s.opts.Logger.Warn("prune intents failed", "error", err)
	}
	if _, err := s.store.MarkStaleRunners(ctx, 2*time.Minute); err != nil {
		s.opts.Logger.Warn("mark stale runners failed", "error", err)
	}
	return nil
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
	expired, err := s.store.ExpireAssignments(ctx)
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

	// 6. Budget.
	if err := s.checkBudget(ctx, project); err != nil {
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

// checkBudget emits an event when a project crosses a spend threshold. The router applies
// the actual downshift at routing time; this exists so humans find out before their work
// silently gets cheaper.
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

	switch {
	case policy.PauseAt > 0 && fraction >= policy.PauseAt:
		return s.store.AppendEvent(ctx, project.OrganizationID, project.ID, "",
			"project", project.ID, "budget.exhausted", domain.VisibilityTeamSummary,
			map[string]any{"cost_usd": spent, "reason": "pause threshold reached"})
	case policy.DownshiftAt > 0 && fraction >= policy.DownshiftAt:
		return s.store.AppendEvent(ctx, project.OrganizationID, project.ID, "",
			"project", project.ID, "budget.downshift", domain.VisibilityTeamSummary,
			map[string]any{"cost_usd": spent, "reason": "downshift threshold reached"})
	}
	return nil
}

func (s *Scheduler) emit(ctx context.Context, project domain.Project, taskID domain.ID, eventType string, payload map[string]any) {
	task, err := s.store.GetTask(ctx, taskID)
	if err == nil {
		payload["task_ref"] = task.Ref
	}
	if err := s.store.AppendEvent(ctx, project.OrganizationID, project.ID, "",
		"task", taskID, eventType, domain.VisibilityTeamSummary, payload); err != nil {
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
