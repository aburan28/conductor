package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
)

// PendingMerge reports whether a task in this status has finished its work but not landed
// it: the changes sit in a branch or pull request that has not merged. Such a task keeps its
// scope reservations as a hold, so nobody is handed files whose new contents are still in
// flight.
func PendingMerge(s domain.TaskStatus) bool {
	return s == domain.TaskVerifying || s == domain.TaskReviewRequired || s == domain.TaskMerging
}

// Pull request states, as GitHub reports them.
const (
	PullRequestOpen   = "open"
	PullRequestMerged = "merged"
	PullRequestClosed = "closed"
)

// LinkPullRequest records the pull request a task's work travels in. It is idempotent, and it
// never moves a link backwards: a pull request already known to have merged or closed stays
// that way even if a stale poll still lists it as open.
func (s *Store) LinkPullRequest(ctx context.Context, taskID domain.ID, url string) (bool, error) {
	if url == "" {
		return false, nil
	}
	var linked bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var orgID, projectID domain.ID
		var ref string
		err := tx.QueryRow(ctx, `
			UPDATE tasks
			   SET pull_request_url = $2, pull_request_state = 'open', updated_at = now()
			 WHERE id = $1::uuid
			   AND (pull_request_url <> $2 OR pull_request_state = '')
			   AND status NOT IN ('done','cancelled','superseded')
			RETURNING organization_id::text, project_id::text, ref`, taskID, url,
		).Scan(&orgID, &projectID, &ref)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		linked = true
		return appendEvents(ctx, tx, orgID, projectID, "",
			eventSpec{"task", taskID, "task.pull_request_linked", domain.VisibilityTeamSummary,
				map[string]any{"task_ref": ref, "pull_request": url, "state": PullRequestOpen}})
	})
	return linked, err
}

// OpenPullRequestTask is a task whose pull request was last seen open.
type OpenPullRequestTask struct {
	TaskID domain.ID
	Ref    string
	URL    string
}

// TasksWithOpenPullRequests lists a project's tasks whose pull request was last seen open. The
// GitHub poller uses it to notice a merge it never received a webhook for: a pull request
// that drops off the open list was either merged or closed.
func (s *Store) TasksWithOpenPullRequests(ctx context.Context, projectID domain.ID) ([]OpenPullRequestTask, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, ref, pull_request_url FROM tasks
		 WHERE project_id = $1::uuid AND pull_request_state = 'open'
		   AND status NOT IN ('done','cancelled','superseded')
		 ORDER BY updated_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenPullRequestTask
	for rows.Next() {
		var t OpenPullRequestTask
		if err := rows.Scan(&t.TaskID, &t.Ref, &t.URL); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// mergePath is the chain of legal transitions a task walks when its pull request merges.
//
// A merge is the strongest evidence of completion there is — the work is on the default
// branch — so a task completes from wherever its work stood, through the ordinary edges
// rather than around them: a person who claimed, worked, and merged without ever calling a
// finish step still ends at done. A task that was never claimed (proposed, ready) has no
// path: a pull request that merged against an unclaimed task did not do that task's work
// under Conductor, and silently completing it would be a guess.
func mergePath(from domain.TaskStatus) []domain.TaskStatus {
	switch from {
	case domain.TaskVerifying, domain.TaskReviewRequired, domain.TaskMerging:
		return []domain.TaskStatus{domain.TaskDone}
	case domain.TaskRunning:
		return []domain.TaskStatus{domain.TaskVerifying, domain.TaskDone}
	case domain.TaskClaimed, domain.TaskBlockedConflict, domain.TaskBlockedDependency, domain.TaskBlockedInput:
		return []domain.TaskStatus{domain.TaskRunning, domain.TaskVerifying, domain.TaskDone}
	}
	return nil
}

// PullRequestOutcome is what recording a pull request's end did to its task.
type PullRequestOutcome struct {
	TaskRef string            `json:"task_ref"`
	From    domain.TaskStatus `json:"from"`
	Status  domain.TaskStatus `json:"status"`
	// Changed is false when the task was left where it was (already finished, never claimed,
	// or a closed pull request against work still in progress).
	Changed bool `json:"changed"`
}

// PullRequestMerged records that a task's pull request merged and completes the task: done,
// its lease (if one is still live) ended, its territory released. Idempotent — a second
// delivery of the same merge finds the task already done.
func (s *Store) PullRequestMerged(ctx context.Context, taskID domain.ID, url string) (PullRequestOutcome, error) {
	var out PullRequestOutcome
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var from domain.TaskStatus
		if err := tx.QueryRow(ctx, `
			UPDATE tasks
			   SET pull_request_url = COALESCE(NULLIF($2, ''), pull_request_url),
			       pull_request_state = 'merged', updated_at = now()
			 WHERE id = $1::uuid
			RETURNING ref, status`, taskID, url).Scan(&out.TaskRef, &from); err != nil {
			return noRows(err)
		}
		out.From, out.Status = from, from
		path := mergePath(from)
		if len(path) == 0 {
			return nil
		}
		task, err := updateTaskStatusTx(ctx, tx, taskID, domain.TaskDone, "pull request merged",
			path[:len(path)-1]...)
		if err != nil {
			return fmt.Errorf("complete %s on merge: %w", out.TaskRef, err)
		}
		out.Status, out.Changed = task.Status, true
		return nil
	})
	return out, err
}

// PullRequestClosed records that a task's pull request closed without merging.
//
// The policy, deliberately: a task whose work was waiting on that pull request (verifying,
// review_required, merging) goes back to ready and its pending-merge hold is released — the
// changes were rejected or abandoned, so nothing is in flight any more and the next claimant
// reserves afresh. A task still being worked (a live lease) is left alone: the holder may
// open another pull request, and the closed one is only recorded. A pull request that is
// reopened is linked again by the next webhook or poll.
func (s *Store) PullRequestClosed(ctx context.Context, taskID domain.ID, url string) (PullRequestOutcome, error) {
	var out PullRequestOutcome
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var from domain.TaskStatus
		if err := tx.QueryRow(ctx, `
			UPDATE tasks
			   SET pull_request_url = COALESCE(NULLIF($2, ''), pull_request_url),
			       pull_request_state = 'closed', updated_at = now()
			 WHERE id = $1::uuid
			RETURNING ref, status`, taskID, url).Scan(&out.TaskRef, &from); err != nil {
			return noRows(err)
		}
		out.From, out.Status = from, from
		if !PendingMerge(from) {
			return nil
		}
		task, err := updateTaskStatusTx(ctx, tx, taskID, domain.TaskReady, "pull request closed without merging")
		if err != nil {
			return err
		}
		if err := releaseTaskReservationsTx(ctx, tx, taskID); err != nil {
			return err
		}
		out.Status, out.Changed = task.Status, true
		return nil
	})
	return out, err
}
