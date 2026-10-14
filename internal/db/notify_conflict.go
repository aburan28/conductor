package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aburan28/conductor/internal/domain"
)

// Conflict announcements: the domain events that say someone ran into someone else's work —
// conflict.blocked (a check or start-work refused by another task's territory),
// conflict.suggest_join (similar work already in flight), and conflict.detected (a new edge in
// the merge-risk graph). They are what notification channels exist for, and each is written at
// most once per window, by whichever replica gets there first: conflict_alerts holds what was
// announced, in the same transaction as the event.

// ConflictAlertWindow is how long one announcement covers a (requester, task, outcome): an
// agent re-checking the same blocked territory every few seconds produces one event, and a
// person still blocked a quarter of an hour later hears about it again.
const ConflictAlertWindow = 15 * time.Minute

// Conflict announcement outcomes, and the event types they are written as.
const (
	ConflictAlertBlocked     = "blocked"
	ConflictAlertSuggestJoin = "suggest_join"
	ConflictAlertDetected    = "detected"
)

// ConflictAnnouncement is a request refused, or redirected, by another task.
type ConflictAnnouncement struct {
	OrganizationID domain.ID
	ProjectID      domain.ID
	// Requester is the principal whose check was refused; the event's actor.
	Requester domain.ID
	// TaskID is the task in the way: the territory's holder, or the similar work.
	TaskID  domain.ID
	Outcome string // ConflictAlertBlocked or ConflictAlertSuggestJoin
	Payload map[string]any
}

// AnnounceConflict appends a conflict.<outcome> event about TaskID unless the same requester
// was told about the same task with the same outcome within window. It reports whether it
// wrote one.
//
// The event's aggregate is the task in the way, so the API's projection shows it at that
// task's visibility (coord.ProjectEvents): a private holder's announcement carries its
// territory and nothing about its intent.
func (s *Store) AnnounceConflict(ctx context.Context, a ConflictAnnouncement, window time.Duration) (bool, error) {
	if window <= 0 {
		window = ConflictAlertWindow
	}
	announced := false
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		// Expired announcements go first, so the insert below can claim the slot again; this
		// also keeps the table to what is inside the window.
		if _, err := tx.Exec(ctx, `
			DELETE FROM conflict_alerts
			 WHERE project_id = $1::uuid AND outcome IN ('blocked', 'suggest_join')
			   AND announced_at < now() - make_interval(secs => $2)`,
			a.ProjectID, window.Seconds()); err != nil {
			return err
		}
		// Replicas racing on one slot serialize on its primary key; only the insert that
		// lands writes the event.
		tag, err := tx.Exec(ctx, `
			INSERT INTO conflict_alerts (project_id, outcome, subject, task_id)
			VALUES ($1::uuid, $2, $3::uuid, $4::uuid)
			ON CONFLICT DO NOTHING`,
			a.ProjectID, a.Outcome, a.Requester, a.TaskID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		announced = true
		return appendEvents(ctx, tx, a.OrganizationID, a.ProjectID, a.Requester,
			eventSpec{"task", a.TaskID, "conflict." + a.Outcome, domain.VisibilityTeamSummary, a.Payload})
	})
	return announced, err
}

// announceDetectedTx writes conflict.detected for an edge the graph has not announced while
// it has been open. Low-severity edges (one shared file in a large change, a read beside a
// write) stay on the conflict radar without an event.
//
// An edge involves two tasks, so the event is written at the narrower of their visibilities:
// the projection narrows it further by the first task's, and together that means neither
// task's private details leave through the other.
func announceDetectedTx(ctx context.Context, tx pgx.Tx, orgID domain.ID, edgeID domain.ID, e domain.ConflictEdge) error {
	if !e.Severity.AtLeast(domain.SeverityMedium) {
		return nil
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO conflict_alerts (project_id, outcome, subject, task_id, kind)
		VALUES ($1::uuid, 'detected', $2::uuid, $3::uuid, $4)
		ON CONFLICT DO NOTHING`, e.ProjectID, e.TaskA, e.TaskB, string(e.Kind))
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	vis := domain.VisibilityTeamArtifacts
	rows, err := tx.Query(ctx, `SELECT visibility FROM tasks WHERE id IN ($1::uuid, $2::uuid)`, e.TaskA, e.TaskB)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v domain.Visibility
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		if !v.AtLeast(vis) {
			vis = v
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	payload := map[string]any{
		"conflict_id": edgeID, "task_id": e.TaskA, "task_ref": e.Detail.TaskARef,
		"with_task_ref": e.Detail.TaskBRef, "kind": string(e.Kind), "severity": string(e.Severity),
		"suggestion": string(e.Suggestion), "reason": e.Detail.Reason,
	}
	if len(e.Detail.Resources) > 0 {
		payload["resources"] = e.Detail.Resources
	}
	if len(e.Detail.SharedPaths) > 0 {
		// Files both attempts changed: what the attempts touched, which follows the tasks'
		// summary visibility, so it travels under the key that the projection treats so.
		payload["changed_paths"] = e.Detail.SharedPaths
	}
	return appendEvents(ctx, tx, orgID, e.ProjectID, "",
		eventSpec{"conflict", edgeID, "conflict.detected", vis, payload})
}

// forgetClosedDetectionsTx drops announcements of conflicts that are no longer open or
// acknowledged, so a conflict that recurs later is announced again.
func forgetClosedDetectionsTx(ctx context.Context, tx pgx.Tx, projectID domain.ID) error {
	_, err := tx.Exec(ctx, `
		DELETE FROM conflict_alerts a
		 WHERE a.project_id = $1::uuid AND a.outcome = 'detected'
		   AND NOT EXISTS (SELECT 1 FROM conflict_edges e
		                    WHERE e.project_id = a.project_id AND e.task_a = a.subject
		                      AND e.task_b = a.task_id AND e.kind = a.kind
		                      AND e.state IN ('open', 'acknowledged'))`, projectID)
	return err
}
