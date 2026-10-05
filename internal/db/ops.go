package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
)

// Operational state: component liveness shared by replicas, outage recovery for leases,
// persisted alert levels, and retention.

// Component names in service_heartbeats.
const (
	ComponentScheduler    = "scheduler"
	ComponentGitHubPoller = "github_poller"
)

// Heartbeat is one component's last recorded run, by whichever replica ran it.
type Heartbeat struct {
	Component string    `json:"component"`
	LastRunAt time.Time `json:"last_run_at"`
	Holder    string    `json:"holder,omitempty"`
	LastError string    `json:"-"`
}

// GetHeartbeat returns a component's heartbeat; found is false when it has never run.
func (s *Store) GetHeartbeat(ctx context.Context, component string) (Heartbeat, bool, error) {
	h := Heartbeat{Component: component}
	err := s.pool.QueryRow(ctx, `
		SELECT last_run_at, holder, last_error FROM service_heartbeats WHERE component = $1`,
		component).Scan(&h.LastRunAt, &h.Holder, &h.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, false, nil
	}
	return h, err == nil, err
}

// RecordHeartbeat stamps a component as having run now, with the error its run ended in
// ("" for success).
func (s *Store) RecordHeartbeat(ctx context.Context, component, holder, lastError string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO service_heartbeats (component, last_run_at, holder, last_error)
		VALUES ($1, clock_timestamp(), $2, $3)
		ON CONFLICT (component) DO UPDATE
		   SET last_run_at = EXCLUDED.last_run_at, holder = EXCLUDED.holder,
		       last_error = EXCLUDED.last_error`,
		component, holder, lastError)
	return err
}

// OutageRecovery reports what BeginSchedulerTick found.
type OutageRecovery struct {
	// Gap is how long it had been since any scheduler replica last recorded a tick. It is
	// zero on the first tick a database has ever seen.
	Gap time.Duration
	// FirstTick is true when no scheduler had ever ticked against this database.
	FirstTick bool
	// LeasesExtended counts open leases moved forward to cover the outage.
	LeasesExtended int
}

// BeginSchedulerTick records that a scheduler is alive and, when no replica has ticked for
// longer than outageAfter, extends every open lease to cover the gap before anything can
// reclaim it.
//
// Lease expiry is measured on the database's clock, and only a running control plane can
// accept the heartbeats that renew a lease. When every replica is down (an upgrade, a crash,
// a Postgres maintenance window), workers keep working but cannot renew, so without this
// the first tick after the outage would reclaim every lease whose TTL the outage outlasted
// and fence off all in-flight work (DESIGN.md §27.1). A gap in the shared heartbeat is the
// evidence that renewals were impossible; a dead worker on a healthy control plane leaves
// no gap and is reclaimed on time.
//
// Only leases that were still live when the last tick ran are extended, by exactly the gap:
// each keeps the time it had left when the outage began. On a database no scheduler has
// ticked against (a fresh install, or the first start after upgrading to a version with
// this table) the gap is unknown, so each open lease is instead given one full TTL — as
// measured by its own last renewal — from now.
//
// The heartbeat row is locked for the duration, so replicas starting together after an
// outage extend once between them.
func (s *Store) BeginSchedulerTick(ctx context.Context, holder string, outageAfter time.Duration) (OutageRecovery, error) {
	var out OutageRecovery
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO service_heartbeats (component, holder) VALUES ($1, $2)
			ON CONFLICT (component) DO NOTHING`, ComponentScheduler, holder)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			out.FirstTick = true
			tag, err := tx.Exec(ctx, `
				UPDATE leases
				   SET expires_at = now() + (expires_at - heartbeat_at)
				 WHERE released_at IS NULL
				   AND expires_at > heartbeat_at
				   AND expires_at < now() + (expires_at - heartbeat_at)`)
			if err != nil {
				return err
			}
			out.LeasesExtended = int(tag.RowsAffected())
			return nil
		}

		var gapSeconds float64
		if err := tx.QueryRow(ctx, `
			SELECT GREATEST(EXTRACT(EPOCH FROM now() - last_run_at), 0)::float8
			  FROM service_heartbeats WHERE component = $1 FOR UPDATE`,
			ComponentScheduler).Scan(&gapSeconds); err != nil {
			return err
		}
		out.Gap = time.Duration(gapSeconds * float64(time.Second))
		if out.Gap > outageAfter {
			tag, err := tx.Exec(ctx, `
				UPDATE leases
				   SET expires_at = expires_at + make_interval(secs => $1)
				 WHERE released_at IS NULL
				   AND expires_at > now() - make_interval(secs => $1)`, gapSeconds)
			if err != nil {
				return err
			}
			out.LeasesExtended = int(tag.RowsAffected())
		}
		_, err = tx.Exec(ctx, `
			UPDATE service_heartbeats SET last_run_at = now(), holder = $2
			 WHERE component = $1`, ComponentScheduler, holder)
		return err
	})
	return out, err
}

// ActiveLeaseCount counts open leases across all projects.
func (s *Store) ActiveLeaseCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM leases WHERE released_at IS NULL`).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// Budget alerts
// ---------------------------------------------------------------------------

// Budget alert levels, lowest first.
const (
	BudgetLevelNone      = ""
	BudgetLevelDownshift = "downshift"
	BudgetLevelExhausted = "exhausted"
)

// SetBudgetAlertLevel records a project's current budget level and, only when it differs
// from the level last recorded, appends the matching event in the same transaction.
//
// Persisting the level is what keeps the alert to one event per crossing: the scheduler
// re-evaluates every tick on every replica, and an in-memory "already announced" flag would
// announce again after each restart and once per replica. changed reports whether the level
// moved; moving back to none is recorded silently, so the next crossing is announced again.
func (s *Store) SetBudgetAlertLevel(ctx context.Context, orgID, projectID domain.ID, level string, payload map[string]any) (changed bool, err error) {
	err = s.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO budget_alert_levels (project_id, level) VALUES ($1::uuid, $2)
			ON CONFLICT (project_id) DO UPDATE
			   SET level = EXCLUDED.level, changed_at = now()
			 WHERE budget_alert_levels.level <> EXCLUDED.level`, projectID, level)
		if err != nil {
			return err
		}
		changed = tag.RowsAffected() == 1
		if !changed || level == BudgetLevelNone {
			return nil
		}
		// budget.downshift and budget.exhausted, the event types the dashboard knows.
		return appendEvents(ctx, tx, orgID, projectID, "",
			eventSpec{"project", projectID, "budget." + level, domain.VisibilityTeamSummary, payload})
	})
	return changed, err
}

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

// RetentionPolicy says how long each kind of row is kept. A zero duration keeps that kind
// forever.
type RetentionPolicy struct {
	// Events is the age past which domain events are deleted. The newest event of every
	// aggregate is always kept: sequence numbers continue from it, and a consumer detects a
	// gap by them.
	Events time.Duration
	// Audit is the audit log's window, normally much longer than Events.
	Audit time.Duration
	// OutboxDelivered applies to outbox rows a consumer has delivered; OutboxUndelivered caps
	// rows nothing ever delivered, so a deployment without a consumer stays bounded.
	OutboxDelivered   time.Duration
	OutboxUndelivered time.Duration
	// Idempotency is how long a stored response can be replayed.
	Idempotency time.Duration
	// Usage applies to usage buckets, by bucket start.
	Usage time.Duration
	// CheckRuns applies to the GitHub check-run dedupe records.
	CheckRuns time.Duration

	// BatchSize rows go per DELETE, and at most MaxBatches per kind per call, so one pass
	// never holds long locks or runs into the statement timeout; the next pass continues.
	BatchSize  int
	MaxBatches int
}

// PruneReport counts rows deleted, by table.
type PruneReport map[string]int64

// Total sums the report.
func (r PruneReport) Total() int64 {
	var n int64
	for _, v := range r {
		n += v
	}
	return n
}

// Prune deletes rows past their retention window, in bounded batches.
func (s *Store) Prune(ctx context.Context, p RetentionPolicy) (PruneReport, error) {
	if p.BatchSize <= 0 {
		p.BatchSize = 1000
	}
	if p.MaxBatches <= 0 {
		p.MaxBatches = 20
	}
	report := PruneReport{}
	// Order matters: outbox rows first, because events with an undelivered outbox row are
	// kept for the consumer, and expired setup states before anything slow.
	steps := []struct {
		table string
		keep  time.Duration
		sql   string
	}{
		{"github_setup_states", time.Hour, `
			DELETE FROM github_setup_states WHERE state_hash IN (
			  SELECT state_hash FROM github_setup_states
			   WHERE expires_at < now() - make_interval(secs => $1) LIMIT $2)`},
		{"outbox_events_delivered", p.OutboxDelivered, `
			DELETE FROM outbox_events WHERE id IN (
			  SELECT id FROM outbox_events
			   WHERE delivered_at IS NOT NULL AND delivered_at < now() - make_interval(secs => $1)
			   LIMIT $2)`},
		{"outbox_events_undelivered", p.OutboxUndelivered, `
			DELETE FROM outbox_events WHERE id IN (
			  SELECT id FROM outbox_events
			   WHERE delivered_at IS NULL AND created_at < now() - make_interval(secs => $1)
			   LIMIT $2)`},
		{"domain_events", p.Events, `
			DELETE FROM domain_events WHERE id IN (
			  SELECT e.id FROM domain_events e
			   WHERE e.occurred_at < now() - make_interval(secs => $1)
			     AND EXISTS (SELECT 1 FROM domain_events n
			                  WHERE n.aggregate_type = e.aggregate_type
			                    AND n.aggregate_id = e.aggregate_id
			                    AND n.sequence_number > e.sequence_number)
			     AND NOT EXISTS (SELECT 1 FROM outbox_events o
			                      WHERE o.event_id = e.id AND o.delivered_at IS NULL)
			   LIMIT $2)`},
		{"audit_log", p.Audit, `
			DELETE FROM audit_log WHERE id IN (
			  SELECT id FROM audit_log WHERE created_at < now() - make_interval(secs => $1) LIMIT $2)`},
		{"idempotency_keys", p.Idempotency, `
			DELETE FROM idempotency_keys WHERE ctid = ANY(ARRAY(
			  SELECT ctid FROM idempotency_keys WHERE created_at < now() - make_interval(secs => $1) LIMIT $2))`},
		{"usage_buckets", p.Usage, `
			DELETE FROM usage_buckets WHERE id IN (
			  SELECT id FROM usage_buckets WHERE bucket_start < now() - make_interval(secs => $1) LIMIT $2)`},
		{"github_check_runs", p.CheckRuns, `
			DELETE FROM github_check_runs WHERE (repository, head_sha) IN (
			  SELECT repository, head_sha FROM github_check_runs
			   WHERE updated_at < now() - make_interval(secs => $1) LIMIT $2)`},
	}
	for _, step := range steps {
		if step.keep <= 0 {
			continue
		}
		for i := 0; i < p.MaxBatches; i++ {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			tag, err := s.pool.Exec(ctx, step.sql, step.keep.Seconds(), p.BatchSize)
			if err != nil {
				return report, fmt.Errorf("prune %s: %w", step.table, err)
			}
			report[step.table] += tag.RowsAffected()
			if tag.RowsAffected() < int64(p.BatchSize) {
				break
			}
		}
	}
	return report, nil
}
