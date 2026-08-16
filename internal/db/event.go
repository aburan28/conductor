package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
)

// eventSpec is one event to append. Payloads pass through the privacy allowlist before they
// are stored, so an event type that carries an unexpected key is narrowed at the boundary
// rather than leaking into the shared stream.
type eventSpec struct {
	aggregateType string
	aggregateID   domain.ID
	eventType     string
	visibility    domain.Visibility
	payload       map[string]any
}

// appendEvents writes domain events and their outbox rows inside the caller's transaction.
//
// Being in the same transaction as the state change is the point (DESIGN.md §23.3): an event
// that describes a claim that did not commit would drive dashboards and integrations into a
// state the ledger never had.
func appendEvents(ctx context.Context, tx pgx.Tx, orgID, projectID, actor domain.ID, specs ...eventSpec) error {
	for _, spec := range specs {
		payload, dropped := privacy.SanitizeEventPayload(spec.payload)
		if len(dropped) > 0 {
			// Record that something was stripped, without recording what it was.
			payload["dropped_keys"] = len(dropped)
		}
		body, err := marshalJSON(payload)
		if err != nil {
			return err
		}
		vis := spec.visibility
		if vis == "" {
			vis = domain.VisibilityTeamSummary
		}

		if err := insertEventWithSequence(ctx, tx, orgID, projectID, actor, spec, vis, body); err != nil {
			return err
		}
	}
	return nil
}

// insertEventWithSequence assigns the next per-aggregate sequence number, retrying through a
// savepoint if a concurrent writer took the number first.
//
// The gapless sequence is what lets a consumer detect a missed event rather than silently
// skipping one, so it is worth the retry rather than falling back to a global counter.
func insertEventWithSequence(
	ctx context.Context,
	tx pgx.Tx,
	orgID, projectID, actor domain.ID,
	spec eventSpec,
	vis domain.Visibility,
	body []byte,
) error {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		sp, err := tx.Begin(ctx) // nested transaction == SAVEPOINT
		if err != nil {
			return err
		}
		_, err = sp.Exec(ctx, `
			INSERT INTO domain_events (organization_id, project_id, aggregate_type,
			        aggregate_id, sequence_number, event_type, actor_principal, visibility, payload)
			SELECT $1::uuid, $2::uuid, $3, $4::uuid,
			       COALESCE(MAX(sequence_number), 0) + 1,
			       $5, $6::uuid, $7, $8
			  FROM domain_events
			 WHERE aggregate_type = $3 AND aggregate_id = $4::uuid`,
			orgID, projectID, spec.aggregateType, spec.aggregateID,
			spec.eventType, nullable(actor), vis, body)
		if err == nil {
			if _, err = sp.Exec(ctx, `
				INSERT INTO outbox_events (event_id, project_id)
				SELECT id, project_id FROM domain_events
				 WHERE aggregate_type = $1 AND aggregate_id = $2::uuid
				 ORDER BY sequence_number DESC LIMIT 1`,
				spec.aggregateType, spec.aggregateID); err == nil {
				return sp.Commit(ctx)
			}
		}
		_ = sp.Rollback(ctx)
		if !isUniqueViolation(err, "") {
			return err
		}
		// Lost the race for this sequence number; recompute and try again.
	}
	return fmt.Errorf("append event %s: exhausted sequence retries", spec.eventType)
}

// AppendEvent writes a single event outside an existing transaction.
func (s *Store) AppendEvent(ctx context.Context, orgID, projectID, actor domain.ID, aggregateType string, aggregateID domain.ID, eventType string, vis domain.Visibility, payload map[string]any) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		return appendEvents(ctx, tx, orgID, projectID, actor,
			eventSpec{aggregateType, aggregateID, eventType, vis, payload})
	})
}

const eventColumns = `
	id::text, organization_id::text, project_id::text, aggregate_type, aggregate_id::text,
	sequence_number, event_type, COALESCE(actor_principal::text, ''), visibility,
	payload, occurred_at`

func scanEvent(scan func(...any) error) (domain.Event, error) {
	var e domain.Event
	var raw []byte
	if err := scan(&e.ID, &e.OrganizationID, &e.ProjectID, &e.AggregateType, &e.AggregateID,
		&e.SequenceNumber, &e.Type, &e.ActorPrincipal, &e.Visibility,
		&raw, &e.OccurredAt); err != nil {
		return domain.Event{}, err
	}
	if err := decodeJSON(raw, &e.Payload); err != nil {
		return domain.Event{}, err
	}
	return e, nil
}

// ListEvents returns recent project events, newest first.
func (s *Store) ListEvents(ctx context.Context, projectID domain.ID, limit int) ([]domain.Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+eventColumns+` FROM domain_events
		  WHERE project_id = $1::uuid
		  ORDER BY occurred_at DESC, id DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		e, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventsSince returns events that occurred after a timestamp, oldest first. The SSE stream
// polls this; the cursor is (occurred_at, id) so events sharing a timestamp are not skipped.
func (s *Store) EventsSince(ctx context.Context, projectID domain.ID, since time.Time, lastID domain.ID, limit int) ([]domain.Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+eventColumns+` FROM domain_events
		 WHERE project_id = $1::uuid
		   AND (occurred_at > $2 OR (occurred_at = $2 AND ($3 = '' OR id::text > $3)))
		 ORDER BY occurred_at, id LIMIT $4`,
		projectID, since, lastID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		e, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
