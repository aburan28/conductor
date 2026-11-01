package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/aburan28/conductor/internal/domain"
)

// Objective tasks (docs/DELPHI.md, "Objectives as Conductor tasks"): delphi's objective tree
// mirrored as a task per node, linked through tasks.parent_id, with the tree's decisions on
// each task's timeline. Delphi drives these tasks, not runners, so they stay proposed until
// the node settles and then move once, atomically, to its outcome.

// TaskTimeline returns a task's events, oldest first; with subtree, its descendants' too
// (through parent_id), so an objective's whole tree reads as one timeline.
func (s *Store) TaskTimeline(ctx context.Context, projectID, taskID domain.ID, subtree bool, limit int) ([]domain.Event, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE tree(id) AS (
		    SELECT id FROM tasks WHERE id = $2::uuid AND project_id = $1::uuid
		    UNION
		    SELECT t.id FROM tasks t JOIN tree ON t.parent_id = tree.id WHERE $3 AND t.project_id = $1::uuid
		)
		SELECT `+eventColumns+` FROM domain_events
		 WHERE project_id = $1::uuid AND aggregate_type = 'task' AND aggregate_id IN (SELECT id FROM tree)
		 ORDER BY occurred_at, id LIMIT $4`, projectID, taskID, subtree, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Event{}
	for rows.Next() {
		e, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// settleVia is the legal walk from proposed to each outcome. Only the final status is
// written (updateTaskStatusTx), so no runner ever sees the task ready or claimed.
var settleVia = map[domain.TaskStatus][]domain.TaskStatus{
	domain.TaskDone:      {domain.TaskReady, domain.TaskClaimed, domain.TaskRunning, domain.TaskVerifying},
	domain.TaskFailed:    {domain.TaskReady, domain.TaskClaimed},
	domain.TaskCancelled: nil,
}

// SettleObjectiveTask moves a proposed objective task to done, failed or cancelled in one
// transaction, recording reason on the status event.
func (s *Store) SettleObjectiveTask(ctx context.Context, id domain.ID, to domain.TaskStatus, reason string) (domain.Task, error) {
	via, ok := settleVia[to]
	if !ok {
		return domain.Task{}, fmt.Errorf("%w: an objective task settles as done, failed or cancelled, not %s", domain.ErrInvalidArgument, to)
	}
	if t, err := s.GetTask(ctx, id); err != nil {
		return t, err
	} else if t.Status == to {
		return t, nil // already settled: a resent settle changes nothing
	}
	var out domain.Task
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var from domain.TaskStatus
		if err := tx.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1::uuid FOR UPDATE`, id).Scan(&from); err != nil {
			return noRows(err)
		}
		if from != domain.TaskProposed {
			return fmt.Errorf("%w: objective task is %s; only a proposed one settles", domain.ErrIllegalTransition, from)
		}
		var err error
		out, err = updateTaskStatusTx(ctx, tx, id, to, reason, via...)
		return err
	})
	return out, err
}
