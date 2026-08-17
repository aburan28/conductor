package db

import (
	"context"
	"fmt"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// Assignments are offers of work to a specific live session (DESIGN.md §7.7).
//
// The table is small on purpose. An assignment says which task, which session, what floor it
// had to meet, and what the session did about it — nothing else. Everything the receiving
// session needs to actually do the work already exists as a task card and a handoff bundle.

const assignmentColumns = `
	a.id::text, a.task_id::text, a.project_id::text, a.session_id::text,
	COALESCE(a.handoff_id::text, ''), a.requirement, a.state, a.rationale, a.response,
	COALESCE(a.created_by::text, ''), a.created_at, a.expires_at, a.responded_at,
	COALESCE(t.ref, '')`

func scanAssignment(scan func(...any) error) (domain.Assignment, error) {
	var a domain.Assignment
	var req []byte
	if err := scan(&a.ID, &a.TaskID, &a.ProjectID, &a.SessionID, &a.HandoffID, &req,
		&a.State, &a.Rationale, &a.Response, &a.CreatedBy,
		&a.CreatedAt, &a.ExpiresAt, &a.RespondedAt, &a.TaskRef); err != nil {
		return domain.Assignment{}, err
	}
	if err := decodeJSON(req, &a.Requirement); err != nil {
		return domain.Assignment{}, err
	}
	return a, nil
}

type CreateAssignmentParams struct {
	TaskID      domain.ID
	ProjectID   domain.ID
	SessionID   domain.ID
	HandoffID   domain.ID
	Requirement domain.CapabilityRequirement
	Rationale   string
	CreatedBy   domain.ID
	TTL         time.Duration
}

// CreateAssignment offers a task to a session.
//
// The unique partial index on (task_id) WHERE state IN ('offered','accepted') is what makes
// this safe under concurrency: two coordinators racing to place the same task produce one
// offer and one conflict error, never two sessions both told the work is theirs.
func (s *Store) CreateAssignment(ctx context.Context, p CreateAssignmentParams) (domain.Assignment, error) {
	if p.TTL <= 0 {
		p.TTL = 15 * time.Minute
	}
	req, err := marshalJSON(p.Requirement)
	if err != nil {
		return domain.Assignment{}, err
	}
	row := s.pool.QueryRow(ctx, `
		WITH inserted AS (
		    INSERT INTO task_assignments (task_id, project_id, session_id, handoff_id,
		                                  requirement, rationale, created_by, expires_at)
		    VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7::uuid,
		            now() + $8::interval)
		    RETURNING *
		)
		SELECT `+assignmentColumns+`
		  FROM inserted a JOIN tasks t ON t.id = a.task_id`,
		p.TaskID, p.ProjectID, p.SessionID, nullable(p.HandoffID), req, p.Rationale,
		nullable(p.CreatedBy), p.TTL.String())
	a, err := scanAssignment(row.Scan)
	if isUniqueViolation(err, "task_assignments_one_open") {
		return domain.Assignment{}, fmt.Errorf("%w: this task already has an open offer",
			domain.ErrAlreadyClaimed)
	}
	return a, err
}

// GetAssignment reads one assignment.
func (s *Store) GetAssignment(ctx context.Context, id domain.ID) (domain.Assignment, error) {
	a, err := scanAssignment(s.pool.QueryRow(ctx, `
		SELECT `+assignmentColumns+`
		  FROM task_assignments a JOIN tasks t ON t.id = a.task_id
		 WHERE a.id = $1::uuid`, id).Scan)
	return a, noRows(err)
}

// SessionInbox lists a session's assignments, newest first. Open offers only unless all.
func (s *Store) SessionInbox(ctx context.Context, sessionID domain.ID, all bool) ([]domain.Assignment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+assignmentColumns+`
		  FROM task_assignments a JOIN tasks t ON t.id = a.task_id
		 WHERE a.session_id = $1::uuid
		   AND ($2 OR a.state IN ('offered','accepted'))
		 ORDER BY a.created_at DESC
		 LIMIT 50`, sessionID, all)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectAssignments(rows)
}

// OpenAssignments lists every offer still outstanding in a project, which is what the
// selector needs in order to know how loaded each session already is.
func (s *Store) OpenAssignments(ctx context.Context, projectID domain.ID) ([]domain.Assignment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+assignmentColumns+`
		  FROM task_assignments a JOIN tasks t ON t.id = a.task_id
		 WHERE a.project_id = $1::uuid AND a.state IN ('offered','accepted')
		 ORDER BY a.created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectAssignments(rows)
}

// RespondToAssignment records a session's answer to an offer.
//
// Open covers both `offered` and `accepted`: a session that took work and then hit something
// it cannot do must be able to hand it back. What cannot be answered is a closed offer —
// declined, expired, cancelled, or fulfilled — so a late accept never quietly revives work
// someone else has since picked up.
func (s *Store) RespondToAssignment(ctx context.Context, id domain.ID, state domain.AssignmentState, note string) (domain.Assignment, error) {
	a, err := scanAssignment(s.pool.QueryRow(ctx, `
		WITH updated AS (
		    UPDATE task_assignments
		       SET state = $2, response = $3, responded_at = now()
		     WHERE id = $1::uuid AND state IN ('offered','accepted')
		    RETURNING *
		)
		SELECT `+assignmentColumns+`
		  FROM updated a JOIN tasks t ON t.id = a.task_id`, id, string(state), note).Scan)
	return a, noRows(err)
}

// CancelAssignmentsForTask closes any open offer on a task. Called when the task is claimed,
// closed, or re-assigned, so an offer never outlives the reason it was made.
func (s *Store) CancelAssignmentsForTask(ctx context.Context, taskID domain.ID, note string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE task_assignments
		   SET state = 'cancelled', response = $2, responded_at = now()
		 WHERE task_id = $1::uuid AND state IN ('offered','accepted')`, taskID, note)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ExpireAssignments closes offers nobody answered, and offers held by sessions that stopped
// heartbeating.
//
// The second half matters more than the first. A laptop that closes mid-offer would otherwise
// hold a task hostage behind the unique open-offer index, and no other session could be given
// it — the exact failure the session TTL exists to prevent for leases.
func (s *Store) ExpireAssignments(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE task_assignments a
		   SET state = 'expired', responded_at = now(),
		       response = CASE WHEN a.expires_at < now()
		                       THEN 'offer expired'
		                       ELSE 'session went stale' END
		  FROM sessions s
		 WHERE s.id = a.session_id
		   AND a.state IN ('offered','accepted')
		   AND (a.expires_at < now() OR s.state IN ('stale','closed') OR s.closed_at IS NOT NULL)`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// FulfilledByAttempt marks the offer satisfied once the session actually claims the task.
func (s *Store) FulfilledByAttempt(ctx context.Context, taskID, sessionID domain.ID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE task_assignments
		   SET state = 'fulfilled', responded_at = now()
		 WHERE task_id = $1::uuid AND session_id = $2::uuid
		   AND state IN ('offered','accepted')`, taskID, sessionID)
	return err
}

func collectAssignments(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]domain.Assignment, error) {
	var out []domain.Assignment
	for rows.Next() {
		a, err := scanAssignment(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
