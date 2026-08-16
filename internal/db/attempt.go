package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
)

const attemptColumns = `
	id::text, task_id::text, project_id::text, attempt_number,
	sponsor_principal::text, COALESCE(executor_principal::text, ''),
	COALESCE(runner_id::text, ''), COALESCE(session_id::text, ''),
	role, harness, COALESCE(model_profile_id::text, ''), model_alias, resolved_model,
	reasoning_effort, state, branch, worktree_path, base_commit_sha, commit_sha,
	changed_paths, tokens_in, tokens_out, cost_usd, turns,
	workflow_sha, project_config_sha, router_policy_ver, model_catalog_ver,
	COALESCE(failure_class, ''), COALESCE(failure_summary, ''),
	started_at, ended_at, last_event_at, created_at`

func scanAttempt(scan func(...any) error) (domain.Attempt, error) {
	var a domain.Attempt
	err := scan(&a.ID, &a.TaskID, &a.ProjectID, &a.AttemptNumber,
		&a.SponsorPrincipal, &a.ExecutorPrincipal, &a.RunnerID, &a.SessionID,
		&a.Role, &a.Harness, &a.ModelProfileID, &a.ModelAlias, &a.ResolvedModel,
		&a.ReasoningEffort, &a.State, &a.Branch, &a.WorktreePath, &a.BaseCommitSHA, &a.CommitSHA,
		&a.ChangedPaths, &a.TokensIn, &a.TokensOut, &a.CostUSD, &a.Turns,
		&a.WorkflowSHA, &a.ProjectConfigSHA, &a.RouterPolicyVer, &a.ModelCatalogVer,
		&a.FailureClass, &a.FailureSummary,
		&a.StartedAt, &a.EndedAt, &a.LastEventAt, &a.CreatedAt)
	return a, err
}

func (s *Store) GetAttempt(ctx context.Context, id domain.ID) (domain.Attempt, error) {
	a, err := scanAttempt(s.pool.QueryRow(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE id = $1::uuid`, id).Scan)
	return a, noRows(err)
}

func (s *Store) ListAttempts(ctx context.Context, taskID domain.ID) ([]domain.Attempt, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE task_id = $1::uuid
		  ORDER BY attempt_number`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attempt
	for rows.Next() {
		a, err := scanAttempt(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ActiveAttempt returns the task's live write attempt, if any.
func (s *Store) ActiveAttempt(ctx context.Context, taskID domain.ID) (domain.Attempt, error) {
	a, err := scanAttempt(s.pool.QueryRow(ctx, `
		SELECT `+attemptColumns+` FROM attempts
		 WHERE task_id = $1::uuid
		   AND state IN ('queued','preparing_workspace','starting_harness','running',
		                 'waiting_for_approval','waiting_for_input','paused_conflict')
		   AND role <> 'reviewer'
		 ORDER BY attempt_number DESC LIMIT 1`, taskID).Scan)
	return a, noRows(err)
}

// AttemptRoute records the model resolution for an attempt.
//
// Aliases resolve to concrete models at attempt start, not at task creation (DESIGN.md §21),
// so the same task retried tomorrow can route differently — and the recorded resolution plus
// the router policy version is what explains why.
type AttemptRoute struct {
	ProfileID       domain.ID
	Alias           string
	Model           string
	Harness         string
	Effort          domain.Effort
	RouterPolicyVer string
	ModelCatalogVer string
	WorktreePath    string
	Branch          string
	BaseCommitSHA   string
	RunnerID        domain.ID
}

// SetAttemptRoute applies the routing decision to a claimed attempt.
func (s *Store) SetAttemptRoute(ctx context.Context, id domain.ID, r AttemptRoute) (domain.Attempt, error) {
	a, err := scanAttempt(s.pool.QueryRow(ctx, `
		UPDATE attempts
		   SET model_profile_id = COALESCE($2::uuid, model_profile_id),
		       model_alias = COALESCE(NULLIF($3, ''), model_alias),
		       resolved_model = COALESCE(NULLIF($4, ''), resolved_model),
		       harness = COALESCE(NULLIF($5, ''), harness),
		       reasoning_effort = COALESCE(NULLIF($6, '')::text, reasoning_effort),
		       router_policy_ver = COALESCE(NULLIF($7, ''), router_policy_ver),
		       model_catalog_ver = COALESCE(NULLIF($8, ''), model_catalog_ver),
		       worktree_path = COALESCE(NULLIF($9, ''), worktree_path),
		       branch = COALESCE(NULLIF($10, ''), branch),
		       base_commit_sha = COALESCE(NULLIF($11, ''), base_commit_sha),
		       runner_id = COALESCE($12::uuid, runner_id),
		       last_event_at = now()
		 WHERE id = $1::uuid
		RETURNING `+attemptColumns,
		id, nullable(r.ProfileID), r.Alias, r.Model, r.Harness, string(r.Effort),
		r.RouterPolicyVer, r.ModelCatalogVer, r.WorktreePath, r.Branch, r.BaseCommitSHA,
		nullable(r.RunnerID)).Scan)
	return a, noRows(err)
}

// AttemptProgress is the metadata a runner reports as work proceeds.
//
// Every field is a measurement. There is no field for what the model said, and the driver
// stream adapters drop text before it can reach here.
type AttemptProgress struct {
	State        domain.AttemptState
	Branch       string
	WorktreePath string
	CommitSHA    string
	ChangedPaths []string
	TokensIn     int64
	TokensOut    int64
	CostUSD      float64
	Turns        int
}

// UpdateAttempt applies progress under the state machine, refusing illegal transitions.
func (s *Store) UpdateAttempt(ctx context.Context, id domain.ID, p AttemptProgress) (domain.Attempt, error) {
	var out domain.Attempt
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var from domain.AttemptState
		if err := tx.QueryRow(ctx,
			`SELECT state FROM attempts WHERE id = $1::uuid FOR UPDATE`, id).Scan(&from); err != nil {
			return noRows(err)
		}
		to := p.State
		if to == "" {
			to = from
		}
		if err := domain.AssertAttemptTransition(from, to); err != nil {
			return err
		}
		var err error
		out, err = scanAttempt(tx.QueryRow(ctx, `
			UPDATE attempts
			   SET state = $2,
			       branch = COALESCE(NULLIF($3, ''), branch),
			       worktree_path = COALESCE(NULLIF($4, ''), worktree_path),
			       commit_sha = COALESCE(NULLIF($5, ''), commit_sha),
			       changed_paths = CASE WHEN $6::text[] IS NULL OR cardinality($6::text[]) = 0
			                            THEN changed_paths ELSE $6::text[] END,
			       tokens_in = GREATEST(tokens_in, $7),
			       tokens_out = GREATEST(tokens_out, $8),
			       cost_usd = GREATEST(cost_usd, $9),
			       turns = GREATEST(turns, $10),
			       last_event_at = now(),
			       started_at = COALESCE(started_at,
			                    CASE WHEN $2 = 'running' THEN now() ELSE NULL END),
			       ended_at = CASE WHEN $2 IN ('succeeded','failed_retryable','failed_terminal',
			                                   'cancelled','stale')
			                       THEN now() ELSE ended_at END
			 WHERE id = $1::uuid
			RETURNING `+attemptColumns,
			id, to, p.Branch, p.WorktreePath, p.CommitSHA, p.ChangedPaths,
			p.TokensIn, p.TokensOut, p.CostUSD, p.Turns).Scan)
		return err
	})
	return out, err
}

// FinishAttempt records a terminal attempt state with a failure classification.
func (s *Store) FinishAttempt(ctx context.Context, id domain.ID, state domain.AttemptState, failureClass, failureSummary string) (domain.Attempt, error) {
	if !state.IsTerminal() {
		return domain.Attempt{}, fmt.Errorf("%w: %s is not terminal", domain.ErrInvalidArgument, state)
	}
	var out domain.Attempt
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var from domain.AttemptState
		if err := tx.QueryRow(ctx,
			`SELECT state FROM attempts WHERE id = $1::uuid FOR UPDATE`, id).Scan(&from); err != nil {
			return noRows(err)
		}
		if err := domain.AssertAttemptTransition(from, state); err != nil {
			return err
		}
		var err error
		out, err = scanAttempt(tx.QueryRow(ctx, `
			UPDATE attempts
			   SET state = $2, failure_class = $3, failure_summary = $4,
			       ended_at = now(), last_event_at = now()
			 WHERE id = $1::uuid
			RETURNING `+attemptColumns,
			id, state, nullableText(failureClass), nullableText(failureSummary)).Scan)
		return err
	})
	return out, err
}

// StalledAttempts lists live attempts whose last event is older than the threshold.
//
// Stall detection is separate from lease expiry on purpose (DESIGN.md §27.3): a hung harness
// should be visible on the dashboard long before the system reclaims its work.
func (s *Store) StalledAttempts(ctx context.Context, projectID domain.ID, stallAfter string) ([]domain.Attempt, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+attemptColumns+` FROM attempts
		 WHERE project_id = $1::uuid
		   AND state IN ('queued','preparing_workspace','starting_harness','running',
		                 'waiting_for_approval','waiting_for_input','paused_conflict')
		   AND last_event_at < now() - $2::interval
		 ORDER BY last_event_at`, projectID, stallAfter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attempt
	for rows.Next() {
		a, err := scanAttempt(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountActiveAttempts reports current concurrency, optionally scoped to one sponsor.
func (s *Store) CountActiveAttempts(ctx context.Context, projectID, sponsor domain.ID) (int, error) {
	q := strings.Builder{}
	q.WriteString(`
		SELECT count(*) FROM attempts
		 WHERE project_id = $1::uuid
		   AND state IN ('queued','preparing_workspace','starting_harness','running',
		                 'waiting_for_approval','waiting_for_input','paused_conflict')`)
	args := []any{projectID}
	if sponsor != "" {
		args = append(args, sponsor)
		q.WriteString(` AND sponsor_principal = $2::uuid`)
	}
	var n int
	err := s.pool.QueryRow(ctx, q.String(), args...).Scan(&n)
	return n, err
}

// SpendSince totals attempt cost in a window, for budget enforcement.
func (s *Store) SpendSince(ctx context.Context, projectID domain.ID, interval string) (float64, error) {
	var total float64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(cost_usd), 0) FROM attempts
		 WHERE project_id = $1::uuid AND created_at > now() - $2::interval`,
		projectID, interval).Scan(&total)
	return total, err
}
