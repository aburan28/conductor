package db

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/quota"
)

// ---------------------------------------------------------------------------
// Usage limits (docs/USAGE_LIMITS.md)
// ---------------------------------------------------------------------------

// QuotaReport is what one POST of readings produced.
type QuotaReport struct {
	Recorded int           `json:"recorded"`
	Alerts   []quota.Alert `json:"alerts"`
}

// QuotaEventTarget is the project whose event stream hears about a level being crossed. It
// is optional: readings belong to the principal, and a report from outside any project still
// records them and their marks.
type QuotaEventTarget struct {
	OrganizationID domain.ID
	ProjectID      domain.ID
}

// RecordQuota stores a principal's readings and raises each level at most once per window.
//
// Each reading replaces the stored one for its window unless the stored one is newer, so a
// late or replayed report cannot move a window backwards. Only readings that did land are
// considered for alerts, and the decision is taken against the persisted marks under a row
// lock, so two sidecars reporting the same login at the same moment raise it once.
func (s *Store) RecordQuota(ctx context.Context, principalID domain.ID, snaps []quota.Snapshot,
	t quota.Thresholds, target QuotaEventTarget) (QuotaReport, error) {
	report := QuotaReport{Alerts: []quota.Alert{}}
	now := s.Now().UTC()
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		report.Recorded, report.Alerts = 0, []quota.Alert{}
		for _, snap := range snaps {
			tag, err := tx.Exec(ctx, `
				INSERT INTO quota_snapshots (principal_id, machine, harness, account, window_name,
				        window_minutes, used_percent, used_value, limit_value, unit, resets_at,
				        limit_reached, plan, source, source_kind, observed_at)
				VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
				ON CONFLICT (principal_id, machine, harness, account, window_name) DO UPDATE SET
				    window_minutes = EXCLUDED.window_minutes,
				    used_percent   = EXCLUDED.used_percent,
				    used_value     = EXCLUDED.used_value,
				    limit_value    = EXCLUDED.limit_value,
				    unit           = EXCLUDED.unit,
				    resets_at      = EXCLUDED.resets_at,
				    limit_reached  = EXCLUDED.limit_reached,
				    plan           = EXCLUDED.plan,
				    source         = EXCLUDED.source,
				    source_kind    = EXCLUDED.source_kind,
				    observed_at    = EXCLUDED.observed_at,
				    updated_at     = now()
				WHERE quota_snapshots.observed_at <= EXCLUDED.observed_at`,
				principalID, snap.Machine, snap.Harness, snap.Account, snap.Window,
				snap.WindowMinutes, snap.UsedPercent, snap.Used, snap.Limit, snap.Unit, utcPtr(snap.ResetsAt),
				snap.LimitReached, snap.Plan, snap.Source, string(snap.SourceKind), snap.ObservedAt.UTC())
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				continue // an older reading than the one already stored
			}
			report.Recorded++

			alert, err := raiseQuotaAlert(ctx, tx, principalID, snap, t, now)
			if err != nil {
				return err
			}
			if alert == nil {
				continue
			}
			report.Alerts = append(report.Alerts, alert.Alert)
			if target.ProjectID == "" {
				continue
			}
			if err := appendEvents(ctx, tx, target.OrganizationID, target.ProjectID, "",
				quotaEvent(alert.id, alert.Alert, now)); err != nil {
				return err
			}
		}
		return nil
	})
	return report, err
}

type raisedAlert struct {
	quota.Alert
	id domain.ID
}

// raiseQuotaAlert applies quota.Decide to one reading against its window's persisted marks.
func raiseQuotaAlert(ctx context.Context, tx pgx.Tx, principalID domain.ID, snap quota.Snapshot,
	t quota.Thresholds, now time.Time) (*raisedAlert, error) {
	rows, err := tx.Query(ctx, `
		SELECT level, window_resets_at, alerted_at FROM quota_alerts
		 WHERE principal_id = $1::uuid AND machine = $2 AND harness = $3 AND account = $4
		   AND window_name = $5
		   FOR UPDATE`,
		principalID, snap.Machine, snap.Harness, snap.Account, snap.Window)
	if err != nil {
		return nil, err
	}
	marks := map[quota.Level]*quota.Mark{}
	for rows.Next() {
		var lvl string
		var m quota.Mark
		if err := rows.Scan(&lvl, &m.ResetsAt, &m.At); err != nil {
			rows.Close()
			return nil, err
		}
		marks[quota.Level(lvl)] = &m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	raise, mark := quota.Decide(marks, snap, t, now)
	var out *raisedAlert
	for _, lvl := range mark {
		var id domain.ID
		var err error
		if marks[lvl] == nil {
			// A concurrent report may have inserted this mark since the read above (there
			// was no row to lock); losing that race means the other report raised it.
			err = tx.QueryRow(ctx, `
				INSERT INTO quota_alerts (principal_id, machine, harness, account, window_name,
				        level, window_resets_at, alerted_at)
				VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT DO NOTHING
				RETURNING id::text`,
				principalID, snap.Machine, snap.Harness, snap.Account, snap.Window,
				string(lvl), utcPtr(snap.ResetsAt), now).Scan(&id)
		} else {
			err = tx.QueryRow(ctx, `
				UPDATE quota_alerts SET window_resets_at = $7, alerted_at = $8
				 WHERE principal_id = $1::uuid AND machine = $2 AND harness = $3 AND account = $4
				   AND window_name = $5 AND level = $6
				RETURNING id::text`,
				principalID, snap.Machine, snap.Harness, snap.Account, snap.Window,
				string(lvl), utcPtr(snap.ResetsAt), now).Scan(&id)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if lvl == raise {
			out = &raisedAlert{Alert: quota.Alert{Level: lvl, Snapshot: snap}, id: id}
		}
	}
	return out, nil
}

// quotaEvent describes a crossed level to the project. It says which tool and window, how
// far, and when it resets — and nothing about whose login it was: no actor, no account, no
// machine. The aggregate is the alert row itself, whose id identifies nobody.
func quotaEvent(alertID domain.ID, a quota.Alert, now time.Time) eventSpec {
	typ := "quota.warning"
	if a.Level == quota.LevelExhausted {
		typ = "quota.exhausted"
	}
	payload := map[string]any{
		"harness":  a.Snapshot.Harness,
		"kind":     a.Snapshot.Window,
		"severity": string(a.Level),
		"reason":   "usage_limit",
	}
	if pct, ok := a.Snapshot.Percent(now); ok {
		payload["percent_hint"] = int(math.Round(pct))
	}
	if a.Snapshot.ResetsAt != nil {
		payload["expires_at"] = a.Snapshot.ResetsAt.UTC()
	}
	return eventSpec{
		aggregateType: "quota", aggregateID: alertID, eventType: typ,
		visibility: domain.VisibilityTeamSummary, payload: payload,
	}
}

// ListQuota returns every reading a principal reported, newest window state per row.
func (s *Store) ListQuota(ctx context.Context, principalID domain.ID) ([]quota.Snapshot, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT machine, harness, account, window_name, window_minutes, used_percent, used_value,
		       limit_value, unit, resets_at, limit_reached, plan, source, source_kind, observed_at
		  FROM quota_snapshots
		 WHERE principal_id = $1::uuid
		 ORDER BY harness, account, machine, window_name`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []quota.Snapshot{}
	for rows.Next() {
		var q quota.Snapshot
		var kind string
		if err := rows.Scan(&q.Machine, &q.Harness, &q.Account, &q.Window, &q.WindowMinutes,
			&q.UsedPercent, &q.Used, &q.Limit, &q.Unit, &q.ResetsAt, &q.LimitReached, &q.Plan,
			&q.Source, &kind, &q.ObservedAt); err != nil {
			return nil, err
		}
		q.SourceKind = quota.SourceKind(kind)
		out = append(out, q)
	}
	return out, rows.Err()
}

// QuotaTeamSnapshot is one recent reading of a project member's login, stripped of who and
// which: the principal is replaced by an opaque per-query ordinal so readings can be grouped
// into logins without the caller learning whose they are.
type QuotaTeamSnapshot struct {
	Login    int
	Snapshot quota.Snapshot
}

// ListProjectQuota returns the readings observed since a moment from every member of a
// project, for counting. Callers must never return these rows as they are.
func (s *Store) ListProjectQuota(ctx context.Context, projectID domain.ID, since time.Time) ([]QuotaTeamSnapshot, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT dense_rank() OVER (ORDER BY q.principal_id, q.machine, q.harness, q.account)::int,
		       q.harness, q.window_name, q.window_minutes, q.used_percent, q.used_value,
		       q.limit_value, q.resets_at, q.limit_reached, q.observed_at
		  FROM quota_snapshots q
		  JOIN project_memberships m ON m.principal_id = q.principal_id AND m.project_id = $1::uuid
		 WHERE q.observed_at >= $2`, projectID, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaTeamSnapshot
	for rows.Next() {
		var r QuotaTeamSnapshot
		q := &r.Snapshot
		if err := rows.Scan(&r.Login, &q.Harness, &q.Window, &q.WindowMinutes, &q.UsedPercent,
			&q.Used, &q.Limit, &q.ResetsAt, &q.LimitReached, &q.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
