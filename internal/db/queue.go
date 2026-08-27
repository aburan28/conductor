package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
)

// Admission queue (DESIGN.md §7.7 concurrency, domain.AdmissionTicket).
//
// Every state change here runs under the project advisory lock — the same lock claims and
// budget shares take — so two sessions arriving at the last free slot cannot both be
// admitted, and a grant cannot interleave with the release that made room for it.

const ticketColumns = `
	t.id::text, t.project_id::text, t.principal_id::text, COALESCE(p.handle, ''),
	COALESCE(t.session_id::text, ''), COALESCE(t.task_id::text, ''), COALESCE(k.ref, ''),
	t.kind, t.harness, t.model, t.priority, t.state, t.note,
	t.requested_at, t.granted_at, t.heartbeat_at, t.expires_at, t.released_at`

const ticketFrom = ` FROM admission_tickets t
	LEFT JOIN principals p ON p.id = t.principal_id
	LEFT JOIN tasks k ON k.id = t.task_id`

func scanTicket(scan func(...any) error) (domain.AdmissionTicket, error) {
	var t domain.AdmissionTicket
	err := scan(&t.ID, &t.ProjectID, &t.PrincipalID, &t.Principal,
		&t.SessionID, &t.TaskID, &t.TaskRef,
		&t.Kind, &t.Harness, &t.Model, &t.Priority, &t.State, &t.Note,
		&t.RequestedAt, &t.GrantedAt, &t.HeartbeatAt, &t.ExpiresAt, &t.ReleasedAt)
	return t, err
}

// EnqueueParams asks for admission.
type EnqueueParams struct {
	ProjectID   domain.ID
	PrincipalID domain.ID
	SessionID   domain.ID
	TaskID      domain.ID
	Kind        domain.TicketKind
	Harness     string
	Model       string
	Priority    int
	Note        string
	Policy      domain.QueuePolicy
}

// Enqueue takes a ticket and grants it immediately when a slot is free, otherwise queues it.
// The returned ticket's State says which.
func (s *Store) Enqueue(ctx context.Context, p EnqueueParams) (domain.AdmissionTicket, error) {
	if p.Kind != domain.TicketSession && p.Kind != domain.TicketAttempt {
		return domain.AdmissionTicket{}, fmt.Errorf("%w: kind=%q", domain.ErrInvalidEnum, p.Kind)
	}
	ttl := p.Policy.TicketTTL()
	var out domain.AdmissionTicket
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockProject(ctx, tx, p.ProjectID); err != nil {
			return err
		}
		var id domain.ID
		if err := tx.QueryRow(ctx, `
			INSERT INTO admission_tickets (project_id, principal_id, session_id, task_id, kind,
			                               harness, model, priority, note, expires_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, now() + $10::interval)
			RETURNING id::text`,
			p.ProjectID, p.PrincipalID, nullable(p.SessionID), nullable(p.TaskID), p.Kind,
			p.Harness, p.Model, p.Priority, p.Note, queueInterval(ttl)).Scan(&id); err != nil {
			return err
		}
		if _, err := grantTicketsTx(ctx, tx, p.ProjectID, p.Policy); err != nil {
			return err
		}
		var err error
		out, err = getTicketTx(ctx, tx, id)
		return err
	})
	return out, err
}

// queueInterval renders a duration as a Postgres interval literal.
func queueInterval(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int(d.Seconds()))
}

// GetTicket reads one ticket with its queue position.
func (s *Store) GetTicket(ctx context.Context, id domain.ID) (domain.AdmissionTicket, error) {
	var out domain.AdmissionTicket
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = getTicketTx(ctx, tx, id)
		return err
	})
	return out, err
}

func getTicketTx(ctx context.Context, tx pgx.Tx, id domain.ID) (domain.AdmissionTicket, error) {
	t, err := scanTicket(tx.QueryRow(ctx, `SELECT `+ticketColumns+ticketFrom+` WHERE t.id = $1::uuid`, id).Scan)
	if err != nil {
		return domain.AdmissionTicket{}, noRows(err)
	}
	if t.State == domain.TicketQueued {
		if err := tx.QueryRow(ctx, `
			SELECT count(*) + 1 FROM admission_tickets
			 WHERE project_id = $1::uuid AND kind = $2 AND state = 'queued'
			   AND (priority > $3 OR (priority = $3 AND requested_at < $4))`,
			t.ProjectID, t.Kind, t.Priority, t.RequestedAt).Scan(&t.Position); err != nil {
			return domain.AdmissionTicket{}, err
		}
	}
	return t, nil
}

// HeartbeatTicket keeps a ticket alive. Queued tickets heartbeat too: a waiter that went
// away is dropped rather than eventually granted a slot nobody will use.
func (s *Store) HeartbeatTicket(ctx context.Context, id domain.ID, ttl time.Duration) (domain.AdmissionTicket, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE admission_tickets
		   SET heartbeat_at = now(), expires_at = now() + $2::interval
		 WHERE id = $1::uuid AND state IN ('queued','granted')`, id, queueInterval(ttl))
	if err != nil {
		return domain.AdmissionTicket{}, err
	}
	if tag.RowsAffected() == 0 {
		return domain.AdmissionTicket{}, fmt.Errorf("%w: ticket is no longer open", domain.ErrNotFound)
	}
	return s.GetTicket(ctx, id)
}

// ReleaseTicket ends a ticket: a granted one frees its slot, a queued one leaves the line.
// Whatever it frees is handed on immediately.
func (s *Store) ReleaseTicket(ctx context.Context, id domain.ID, state domain.TicketState, policy domain.QueuePolicy) (domain.AdmissionTicket, error) {
	if state != domain.TicketReleased && state != domain.TicketCancelled {
		state = domain.TicketReleased
	}
	var out domain.AdmissionTicket
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var projectID domain.ID
		if err := tx.QueryRow(ctx, `SELECT project_id::text FROM admission_tickets WHERE id = $1::uuid`, id).Scan(&projectID); err != nil {
			return noRows(err)
		}
		if err := lockProject(ctx, tx, projectID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE admission_tickets SET state = $2, released_at = now()
			 WHERE id = $1::uuid AND state IN ('queued','granted')`, id, state); err != nil {
			return err
		}
		if _, err := grantTicketsTx(ctx, tx, projectID, policy); err != nil {
			return err
		}
		var err error
		out, err = getTicketTx(ctx, tx, id)
		return err
	})
	return out, err
}

// ReleaseTicketsForSession ends whatever tickets a session held, when the session closes.
func (s *Store) ReleaseTicketsForSession(ctx context.Context, sessionID domain.ID, policy domain.QueuePolicy) error {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text FROM admission_tickets
		 WHERE session_id = $1::uuid AND state IN ('queued','granted')`, sessionID)
	if err != nil {
		return err
	}
	var ids []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.ReleaseTicket(ctx, id, domain.TicketReleased, policy); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	return nil
}

// ReconcileQueue is the scheduler's pass: expire tickets nobody heartbeats, release tickets
// whose session closed or whose attempt ended, then grant whatever the freed slots allow.
// It returns how many tickets were expired and how many granted.
func (s *Store) ReconcileQueue(ctx context.Context, projectID domain.ID, policy domain.QueuePolicy) (expired, granted int64, err error) {
	err = s.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockProject(ctx, tx, projectID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE admission_tickets SET state = 'expired', released_at = now()
			 WHERE project_id = $1::uuid AND state IN ('queued','granted') AND expires_at < now()`, projectID)
		if err != nil {
			return err
		}
		expired = tag.RowsAffected()
		// A ticket bound to a session that is gone, or to an attempt that ended, is done
		// whether or not anyone said so.
		tag, err = tx.Exec(ctx, `
			UPDATE admission_tickets t SET state = 'released', released_at = now()
			 WHERE t.project_id = $1::uuid AND t.state = 'granted'
			   AND ((t.session_id IS NOT NULL AND EXISTS (
			           SELECT 1 FROM sessions s WHERE s.id = t.session_id
			              AND (s.closed_at IS NOT NULL OR s.state IN ('stale','closed'))))
			     OR (t.kind = 'attempt' AND t.task_id IS NOT NULL AND NOT EXISTS (
			           SELECT 1 FROM attempts a WHERE a.task_id = t.task_id AND a.state IN (
			               'queued','preparing_workspace','starting_harness','running',
			               'waiting_for_approval','waiting_for_input','paused_conflict'))
			         AND t.granted_at < now() - interval '2 minutes'))`, projectID)
		if err != nil {
			return err
		}
		expired += tag.RowsAffected()
		granted, err = grantTicketsTx(ctx, tx, projectID, policy)
		return err
	})
	return expired, granted, err
}

// grantTicketsTx admits queued tickets in priority-then-arrival order while the policy has
// room. Session tickets count against max_active_sessions and max_sessions_per_principal
// (granted session tickets plus live sessions that never took a ticket both occupy slots);
// attempt tickets count against max_concurrent_attempts.
func grantTicketsTx(ctx context.Context, tx pgx.Tx, projectID domain.ID, policy domain.QueuePolicy) (int64, error) {
	var granted int64
	rows, err := tx.Query(ctx, `
		SELECT id::text, principal_id::text, kind FROM admission_tickets
		 WHERE project_id = $1::uuid AND state = 'queued'
		 ORDER BY priority DESC, requested_at`, projectID)
	if err != nil {
		return 0, err
	}
	type waiting struct {
		id, principal domain.ID
		kind          domain.TicketKind
	}
	var queue []waiting
	for rows.Next() {
		var w waiting
		if err := rows.Scan(&w.id, &w.principal, &w.kind); err != nil {
			rows.Close()
			return 0, err
		}
		queue = append(queue, w)
	}
	rows.Close()
	if len(queue) == 0 {
		return 0, nil
	}

	// Occupancy, counted once per pass and adjusted as tickets are granted.
	var activeSessions, activeAttempts int
	perPrincipal := map[domain.ID]int{}
	if policy.MaxActiveSessions > 0 || policy.MaxSessionsPerPrincipal > 0 {
		// Live sessions that hold a granted ticket count once; sessions that never took a
		// ticket (wrap without queueing, MCP sessions) count too.
		prow, err := tx.Query(ctx, `
			SELECT principal_id::text, count(*) FROM (
			    SELECT s.principal_id FROM sessions s
			     WHERE s.project_id = $1::uuid AND s.closed_at IS NULL AND s.state NOT IN ('stale','closed')
			    UNION ALL
			    SELECT t.principal_id FROM admission_tickets t
			     WHERE t.project_id = $1::uuid AND t.kind = 'session' AND t.state = 'granted'
			       AND (t.session_id IS NULL OR NOT EXISTS (
			             SELECT 1 FROM sessions s WHERE s.id = t.session_id AND s.closed_at IS NULL))
			) x GROUP BY 1`, projectID)
		if err != nil {
			return 0, err
		}
		for prow.Next() {
			var pid domain.ID
			var n int
			if err := prow.Scan(&pid, &n); err != nil {
				prow.Close()
				return 0, err
			}
			perPrincipal[pid] = n
			activeSessions += n
		}
		prow.Close()
	}
	if policy.MaxConcurrentAttempts > 0 {
		if err := tx.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM attempts WHERE project_id = $1::uuid AND state IN (
			            'queued','preparing_workspace','starting_harness','running',
			            'waiting_for_approval','waiting_for_input','paused_conflict'))
			     + (SELECT count(*) FROM admission_tickets WHERE project_id = $1::uuid
			           AND kind = 'attempt' AND state = 'granted'
			           AND granted_at > now() - interval '2 minutes'
			           AND NOT EXISTS (SELECT 1 FROM attempts a WHERE a.task_id = admission_tickets.task_id
			                              AND a.state IN ('queued','preparing_workspace','starting_harness','running')))`,
			projectID).Scan(&activeAttempts); err != nil {
			return 0, err
		}
	}

	for _, w := range queue {
		admit := false
		switch w.kind {
		case domain.TicketSession:
			admit = (policy.MaxActiveSessions <= 0 || activeSessions < policy.MaxActiveSessions) &&
				(policy.MaxSessionsPerPrincipal <= 0 || perPrincipal[w.principal] < policy.MaxSessionsPerPrincipal)
			if admit {
				activeSessions++
				perPrincipal[w.principal]++
			}
		case domain.TicketAttempt:
			admit = policy.MaxConcurrentAttempts <= 0 || activeAttempts < policy.MaxConcurrentAttempts
			if admit {
				activeAttempts++
			}
		}
		if !admit {
			// Strict FIFO within a kind: a blocked head of line is not overtaken, because
			// overtaking is how a busy teammate never gets a slot.
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE admission_tickets SET state = 'granted', granted_at = now(),
			       heartbeat_at = now(), expires_at = now() + $2::interval
			 WHERE id = $1::uuid`, w.id, queueInterval(policy.TicketTTL())); err != nil {
			return granted, err
		}
		granted++
	}
	return granted, nil
}

// QueueSnapshot is what the queue looks like right now.
type QueueSnapshot struct {
	ActiveSessions int
	ActiveAttempts int
	Tickets        []domain.AdmissionTicket
}

// ListQueue returns open tickets (queued first, in grant order, then granted) plus recent
// closed ones, with positions filled in.
func (s *Store) ListQueue(ctx context.Context, projectID domain.ID, includeClosed bool) (QueueSnapshot, error) {
	var snap QueueSnapshot
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM sessions WHERE project_id = $1::uuid AND closed_at IS NULL AND state NOT IN ('stale','closed')),
		       (SELECT count(*) FROM attempts WHERE project_id = $1::uuid AND state IN (
		            'queued','preparing_workspace','starting_harness','running',
		            'waiting_for_approval','waiting_for_input','paused_conflict'))`,
		projectID).Scan(&snap.ActiveSessions, &snap.ActiveAttempts); err != nil {
		return snap, err
	}
	where := `WHERE t.project_id = $1::uuid AND t.state IN ('queued','granted')`
	if includeClosed {
		where = `WHERE t.project_id = $1::uuid AND (t.state IN ('queued','granted') OR t.released_at > now() - interval '1 hour')`
	}
	rows, err := s.pool.Query(ctx, `SELECT `+ticketColumns+ticketFrom+` `+where+`
		 ORDER BY CASE t.state WHEN 'queued' THEN 0 WHEN 'granted' THEN 1 ELSE 2 END,
		          t.priority DESC, t.requested_at`, projectID)
	if err != nil {
		return snap, err
	}
	defer rows.Close()
	positions := map[domain.TicketKind]int{}
	for rows.Next() {
		t, err := scanTicket(rows.Scan)
		if err != nil {
			return snap, err
		}
		if t.State == domain.TicketQueued {
			positions[t.Kind]++
			t.Position = positions[t.Kind]
		}
		snap.Tickets = append(snap.Tickets, t)
	}
	if snap.Tickets == nil {
		snap.Tickets = []domain.AdmissionTicket{}
	}
	return snap, rows.Err()
}

// InFlightByModel counts live attempts per harness:model, which is what dispatch limits
// are measured against.
func (s *Store) InFlightByModel(ctx context.Context, projectID domain.ID) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT harness, resolved_model, count(*) FROM attempts
		 WHERE project_id = $1::uuid AND state IN (
		       'queued','preparing_workspace','starting_harness','running',
		       'waiting_for_approval','waiting_for_input','paused_conflict')
		 GROUP BY 1, 2`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var harness, model string
		var n int
		if err := rows.Scan(&harness, &model, &n); err != nil {
			return nil, err
		}
		out[harness+":"+model] = n
	}
	return out, rows.Err()
}
