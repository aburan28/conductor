package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/resource"
)

const leaseColumns = `
	id::text, task_id::text, attempt_id::text, project_id::text, fencing_epoch,
	holder_principal::text, COALESCE(session_id::text, ''),
	acquired_at, heartbeat_at, expires_at, released_at, COALESCE(release_reason, '')`

func scanLease(scan func(...any) error) (domain.Lease, error) {
	var l domain.Lease
	err := scan(&l.ID, &l.TaskID, &l.AttemptID, &l.ProjectID, &l.FencingEpoch,
		&l.HolderPrincipal, &l.SessionID,
		&l.AcquiredAt, &l.HeartbeatAt, &l.ExpiresAt, &l.ReleasedAt, &l.ReleaseReason)
	return l, err
}

// ClaimParams describes an attempt to take exclusive execution rights on a task.
type ClaimParams struct {
	TaskID           domain.ID
	ProjectID        domain.ID
	HolderPrincipal  domain.ID
	SponsorPrincipal domain.ID
	SessionID        domain.ID
	RunnerID         domain.ID
	Role             domain.AgentRole
	Harness          string
	ModelProfileID   domain.ID
	ModelAlias       string
	ResolvedModel    string
	ReasoningEffort  domain.Effort
	Branch           string
	WorktreePath     string
	BaseCommitSHA    string
	WorkflowSHA      string
	ProjectConfigSHA string
	RouterPolicyVer  string
	ModelCatalogVer  string
	LeaseTTL         time.Duration
	Scopes           []domain.ScopeRequest
	ScopePolicy      resource.Policy
	// AllowWarnings lets the claim proceed when overlaps are advisory rather than blocking.
	AllowWarnings bool
	// MemberTokenBudget is the per-member token allowance from project policy
	// (BudgetPolicy.MemberTokens). When > 0, a sponsor whose rolling-window balance is
	// spent cannot start new work — and the remedy is collaborative: a teammate shares
	// budget with them (ShareBudget). 0 disables the check.
	MemberTokenBudget int64
}

// ClaimResult is what a successful claim hands back. The fence must accompany every
// subsequent mutation.
type ClaimResult struct {
	Task         domain.Task               `json:"task"`
	Attempt      domain.Attempt            `json:"attempt"`
	Lease        domain.Lease              `json:"lease"`
	Fence        domain.Fence              `json:"fence"`
	Reservations []domain.ScopeReservation `json:"reservations"`
	Warnings     []ScopeConflict           `json:"warnings,omitempty"`
}

// Claim implements the atomic claim algorithm of DESIGN.md §10.2.
//
// The whole point is that steps 1-10 either all happen or none do. A partially applied claim
// is the failure mode that produces two agents editing the same file while each believes it
// holds the lock.
func (s *Store) Claim(ctx context.Context, p ClaimParams) (ClaimResult, error) {
	if p.LeaseTTL <= 0 {
		p.LeaseTTL = 90 * time.Second
	}
	if p.Role == "" {
		p.Role = domain.RoleImplementer
	}
	if p.SponsorPrincipal == "" {
		p.SponsorPrincipal = p.HolderPrincipal
	}

	var out ClaimResult
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		// Serialize on the project so the reservation check below cannot interleave with a
		// competing claim's insert.
		if err := lockProject(ctx, tx, p.ProjectID); err != nil {
			return err
		}

		// (1) Lock the task row.
		var status domain.TaskStatus
		var epoch int64
		var attemptsCount, maxAttempts int
		var projectID domain.ID
		if err := tx.QueryRow(ctx, `
			SELECT status, fencing_epoch, attempts_count, max_attempts, project_id::text
			  FROM tasks WHERE id = $1::uuid FOR UPDATE`, p.TaskID,
		).Scan(&status, &epoch, &attemptsCount, &maxAttempts, &projectID); err != nil {
			return noRows(err)
		}
		if projectID != p.ProjectID {
			return domain.ErrNotFound
		}

		// (2) Confirm the task is claimable and its dependencies are satisfied.
		if !domain.CanTransitionTask(status, domain.TaskClaimed) {
			return fmt.Errorf("%w: cannot claim a task in state %s", domain.ErrIllegalTransition, status)
		}
		var unmet int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM task_dependencies d
			  JOIN tasks dep ON dep.id = d.depends_on_id
			 WHERE d.task_id = $1::uuid AND dep.status <> 'done'`, p.TaskID).Scan(&unmet); err != nil {
			return err
		}
		if unmet > 0 {
			return fmt.Errorf("%w: %d unmet dependency(ies)", domain.ErrNotPermitted, unmet)
		}
		if attemptsCount >= maxAttempts {
			return fmt.Errorf("%w: retry budget exhausted (%d/%d)",
				domain.ErrNotPermitted, attemptsCount, maxAttempts)
		}

		// (2b) Per-member token budget. Checked under the same project lock the share path
		// takes, so a grant and a claim cannot race past each other. The balance only has
		// to be positive to start: an attempt that overruns is recorded in full and shows
		// up as a negative balance, which a teammate's grant can repair.
		if p.MemberTokenBudget > 0 {
			position, err := memberBudgetTx(ctx, tx, p.ProjectID, p.SponsorPrincipal, p.MemberTokenBudget)
			if err != nil {
				return err
			}
			if position.Remaining <= 0 {
				return fmt.Errorf(
					"%w: member token budget spent (%d of %d tokens used in the current window); a teammate can help with `conductor budget share`",
					domain.ErrBudgetExhausted,
					position.Spent+position.SharedOut-position.SharedIn, p.MemberTokenBudget)
			}
		}

		// (3) Retire any lease that has already expired, so a dead holder does not block a
		// live claimant.
		if _, err := tx.Exec(ctx, `
			UPDATE leases SET released_at = now(), release_reason = 'expired'
			 WHERE task_id = $1::uuid AND released_at IS NULL AND expires_at <= now()`,
			p.TaskID); err != nil {
			return err
		}
		var liveLeases int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM leases WHERE task_id = $1::uuid AND released_at IS NULL`,
			p.TaskID).Scan(&liveLeases); err != nil {
			return err
		}
		if liveLeases > 0 {
			return domain.ErrAlreadyClaimed
		}

		// (4-5) Evaluate the requested reservations and reject hard conflicts.
		holders, err := loadActiveHolders(ctx, tx, p.ProjectID, p.TaskID)
		if err != nil {
			return err
		}
		conflicts, parsed, err := evaluateScopes(p.Scopes, holders, p.ScopePolicy)
		if err != nil {
			return err
		}
		redactHolderTitles(conflicts, holders, p.HolderPrincipal)
		if Blocking(conflicts) {
			out.Warnings = conflicts
			return fmt.Errorf("%w: %d blocking overlap(s)", domain.ErrConflict, len(conflicts))
		}
		if len(conflicts) > 0 && !p.AllowWarnings {
			out.Warnings = conflicts
			return fmt.Errorf("%w: %d advisory overlap(s)", domain.ErrConflict, len(conflicts))
		}
		out.Warnings = conflicts

		// (6) Increment the fencing epoch. Every claim gets a strictly higher epoch than any
		// worker that came before, which is what makes a resurrected zombie harmless.
		newEpoch := epoch + 1

		// (7) Insert the attempt, then the lease.
		attemptNumber := attemptsCount + 1
		attempt, err := scanAttempt(tx.QueryRow(ctx, `
			INSERT INTO attempts (task_id, project_id, attempt_number, sponsor_principal,
			        executor_principal, runner_id, session_id, role, harness,
			        model_profile_id, model_alias, resolved_model, reasoning_effort, state,
			        branch, worktree_path, base_commit_sha, workflow_sha, project_config_sha,
			        router_policy_ver, model_catalog_ver)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5::uuid, $6::uuid, $7::uuid, $8, $9,
			        $10::uuid, $11, $12, $13, 'queued', $14, $15, $16, $17, $18, $19, $20)
			RETURNING `+attemptColumns,
			p.TaskID, p.ProjectID, attemptNumber, p.SponsorPrincipal,
			nullable(p.HolderPrincipal), nullable(p.RunnerID), nullable(p.SessionID),
			p.Role, p.Harness, nullable(p.ModelProfileID), p.ModelAlias, p.ResolvedModel,
			effortOrDefault(p.ReasoningEffort), p.Branch, p.WorktreePath, p.BaseCommitSHA,
			p.WorkflowSHA, p.ProjectConfigSHA, p.RouterPolicyVer, p.ModelCatalogVer,
		).Scan)
		if err != nil {
			// The partial unique index permits only one live write attempt per task, so a
			// violation here means a concurrent claimant won the race.
			if isUniqueViolation(err, "one_active_write_attempt_per_task") {
				return domain.ErrAlreadyClaimed
			}
			return err
		}

		lease, err := scanLease(tx.QueryRow(ctx, `
			INSERT INTO leases (task_id, attempt_id, project_id, fencing_epoch,
			                    holder_principal, session_id, expires_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::uuid, $6::uuid, now() + $7::interval)
			RETURNING `+leaseColumns,
			p.TaskID, attempt.ID, p.ProjectID, newEpoch,
			p.HolderPrincipal, nullable(p.SessionID), p.LeaseTTL.String(),
		).Scan)
		if err != nil {
			if isUniqueViolation(err, "one_active_lease_per_task") {
				return domain.ErrAlreadyClaimed
			}
			return err
		}

		// (8) Insert the scope reservations under the new lease.
		for i, req := range p.Scopes {
			mode := req.Mode
			if mode == "" {
				mode = domain.ModeWriteExclusive
			}
			var r domain.ScopeReservation
			err := tx.QueryRow(ctx, `
				INSERT INTO scope_reservations (project_id, task_id, attempt_id, lease_id,
				        principal_id, resource_type, resource_key, mode, source, alternative_group)
				VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::uuid, $6, $7, $8, 'declared', $9)
				RETURNING id::text, project_id::text, task_id::text,
				          COALESCE(attempt_id::text, ''), COALESCE(lease_id::text, ''),
				          principal_id::text, resource_type, resource_key, mode, source,
				          COALESCE(alternative_group, ''), active, created_at`,
				p.ProjectID, p.TaskID, attempt.ID, lease.ID, p.HolderPrincipal,
				parsed[i].Type, parsed[i].Key, mode, nullableText(req.AlternativeGroup),
			).Scan(&r.ID, &r.ProjectID, &r.TaskID, &r.AttemptID, &r.LeaseID,
				&r.PrincipalID, &r.ResourceType, &r.ResourceKey, &r.Mode, &r.Source,
				&r.AlternativeGroup, &r.Active, &r.CreatedAt)
			if err != nil {
				return err
			}
			out.Reservations = append(out.Reservations, r)
		}

		// (9) Move the task to claimed and publish the new epoch.
		task, err := scanTask(tx.QueryRow(ctx, `
			UPDATE tasks
			   SET status = 'claimed', active_lease_id = $2::uuid, fencing_epoch = $3,
			       attempts_count = attempts_count + 1, updated_at = now()
			 WHERE id = $1::uuid
			RETURNING `+strings.ReplaceAll(taskColumns, "t.", "tasks."),
			p.TaskID, lease.ID, newEpoch).Scan)
		if err != nil {
			return err
		}

		if p.SessionID != "" {
			if _, err := tx.Exec(ctx,
				`UPDATE sessions SET active_task_id = $2::uuid, state = 'working'
				  WHERE id = $1::uuid`, p.SessionID, p.TaskID); err != nil {
				return err
			}
		}

		out.Task = task
		out.Attempt = attempt
		out.Lease = lease
		out.Fence = domain.Fence{
			TaskID: task.ID, AttemptID: attempt.ID, LeaseID: lease.ID, FencingEpoch: newEpoch,
		}

		// (10) Domain events, written in the same transaction as the state change.
		return appendEvents(ctx, tx, task.OrganizationID, task.ProjectID, p.HolderPrincipal,
			eventSpec{"task", task.ID, "task.claimed", domain.VisibilityTeamSummary, map[string]any{
				"task_ref": task.Ref, "status": string(task.Status),
				"fencing_epoch": newEpoch, "attempt_id": attempt.ID,
				"attempt_number": attemptNumber, "harness": p.Harness,
				"model_alias": p.ModelAlias, "role": string(p.Role),
			}},
			eventSpec{"attempt", attempt.ID, "attempt.created", domain.VisibilityTeamSummary, map[string]any{
				"task_ref": task.Ref, "attempt_number": attemptNumber,
				"state": string(attempt.State), "harness": p.Harness,
				"model_alias": p.ModelAlias, "resolved_model": p.ResolvedModel,
				"reasoning_effort": string(effortOrDefault(p.ReasoningEffort)),
			}},
		)
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

func effortOrDefault(e domain.Effort) domain.Effort {
	if e == "" {
		return domain.EffortMedium
	}
	return e
}

// ClaimNextParams asks for the highest-priority ready task the caller is eligible for.
type ClaimNextParams struct {
	ClaimParams
	// MaxCandidates bounds how many ready tasks are examined before giving up. Each
	// candidate costs a conflict evaluation, so this keeps a busy queue from turning one
	// dispatch into a full scan.
	MaxCandidates int
}

// ClaimNext selects and claims the next eligible ready task.
//
// `FOR UPDATE SKIP LOCKED` is what makes duplicate dispatch structurally impossible rather
// than merely unlikely: two schedulers racing the same ready queue cannot select the same
// row, so scheduler replicas need no leader election (DESIGN.md §7.7, §28.3).
func (s *Store) ClaimNext(ctx context.Context, p ClaimNextParams) (ClaimResult, error) {
	if p.MaxCandidates <= 0 {
		p.MaxCandidates = 10
	}

	candidates, err := s.readyCandidates(ctx, p.ProjectID, p.MaxCandidates)
	if err != nil {
		return ClaimResult{}, err
	}
	if len(candidates) == 0 {
		return ClaimResult{}, domain.ErrNotFound
	}

	for _, task := range candidates {
		params := p.ClaimParams
		params.TaskID = task.ID
		if params.ModelAlias == "" {
			params.ModelAlias = task.ModelAlias
		}
		// A task's own declared scopes are re-requested on claim so the conflict check runs
		// against current reality rather than what was true when it was filed.
		if len(params.Scopes) == 0 {
			declared, err := s.ReservationsForTask(ctx, task.ID)
			if err != nil {
				return ClaimResult{}, err
			}
			for _, r := range declared {
				params.Scopes = append(params.Scopes, domain.ScopeRequest{
					Resource: r.Resource(), Mode: r.Mode, AlternativeGroup: r.AlternativeGroup,
				})
			}
		}

		result, err := s.Claim(ctx, params)
		switch {
		case err == nil:
			return result, nil
		case errors.Is(err, domain.ErrAlreadyClaimed),
			errors.Is(err, domain.ErrConflict),
			errors.Is(err, domain.ErrNotPermitted),
			errors.Is(err, domain.ErrIllegalTransition):
			// Someone else took it, or it is blocked right now. Try the next candidate.
			continue
		default:
			return ClaimResult{}, err
		}
	}
	return ClaimResult{}, domain.ErrNotFound
}

// readyCandidates returns ready tasks in dispatch order, skipping rows another scheduler has
// locked.
func (s *Store) readyCandidates(ctx context.Context, projectID domain.ID, limit int) ([]domain.Task, error) {
	var out []domain.Task
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+taskColumns+`
			  FROM tasks t
			 WHERE t.project_id = $1::uuid
			   AND t.status = 'ready'
			   AND t.attempts_count < t.max_attempts
			   AND NOT EXISTS (
			         SELECT 1 FROM task_dependencies d
			           JOIN tasks dep ON dep.id = d.depends_on_id
			          WHERE d.task_id = t.id AND dep.status <> 'done')
			 ORDER BY t.priority DESC, t.created_at
			 FOR UPDATE OF t SKIP LOCKED
			 LIMIT $2`, projectID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			task, err := scanTask(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, task)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Fencing
// ---------------------------------------------------------------------------

// assertFence implements the fencing rule of DESIGN.md §10.3. Zero updated rows means the
// caller is stale and must stop.
//
// It is written as an UPDATE rather than a SELECT so the check and the claim of the row
// happen in one statement; a SELECT-then-act would reintroduce exactly the race the fence
// exists to close.
func assertFence(ctx context.Context, tx pgx.Tx, f domain.Fence) error {
	tag, err := tx.Exec(ctx, `
		UPDATE tasks SET updated_at = now()
		 WHERE id = $1::uuid
		   AND active_lease_id = $2::uuid
		   AND fencing_epoch = $3`, f.TaskID, f.LeaseID, f.FencingEpoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: lease %s epoch %d is no longer current",
			domain.ErrStaleFencing, f.LeaseID, f.FencingEpoch)
	}
	// The epoch matched, but the lease may still have expired without being reclaimed yet.
	var live bool
	if err := tx.QueryRow(ctx, `
		SELECT released_at IS NULL AND expires_at > now()
		  FROM leases WHERE id = $1::uuid`, f.LeaseID).Scan(&live); err != nil {
		return noRows(err)
	}
	if !live {
		return fmt.Errorf("%w: lease %s has expired", domain.ErrLeaseNotHeld, f.LeaseID)
	}
	return nil
}

// AssertFence exposes the fencing check to service code that needs to validate before doing
// non-database work.
func (s *Store) AssertFence(ctx context.Context, f domain.Fence) error {
	return s.Tx(ctx, func(tx pgx.Tx) error { return assertFence(ctx, tx, f) })
}

// HeartbeatLease renews a lease, rejecting stale holders.
func (s *Store) HeartbeatLease(ctx context.Context, f domain.Fence, ttl time.Duration) (domain.Lease, error) {
	if ttl <= 0 {
		ttl = 90 * time.Second
	}
	var lease domain.Lease
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := assertFence(ctx, tx, f); err != nil {
			return err
		}
		var err error
		lease, err = scanLease(tx.QueryRow(ctx, `
			UPDATE leases
			   SET heartbeat_at = now(), expires_at = now() + $2::interval
			 WHERE id = $1::uuid AND released_at IS NULL
			RETURNING `+leaseColumns, f.LeaseID, ttl.String()).Scan)
		if err != nil {
			return noRows(err)
		}
		_, err = tx.Exec(ctx,
			`UPDATE attempts SET last_event_at = now() WHERE id = $1::uuid`, f.AttemptID)
		return err
	})
	return lease, err
}

// ReleaseParams hands a task back.
type ReleaseParams struct {
	Fence            domain.Fence
	Reason           string
	NextTaskStatus   domain.TaskStatus
	AttemptState     domain.AttemptState
	FailureClass     string
	FailureSummary   string
	KeepReservations bool
}

// Release ends an attempt and frees the task and its territory.
//
// A clean release is much better than waiting for a lease to expire: the reservations drop
// immediately, so a teammate blocked on the same directory is unblocked in seconds rather
// than after the TTL.
func (s *Store) Release(ctx context.Context, p ReleaseParams) (domain.Task, error) {
	if p.AttemptState == "" {
		p.AttemptState = domain.AttemptCancelled
	}
	var out domain.Task
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := assertFence(ctx, tx, p.Fence); err != nil {
			return err
		}

		var fromAttempt domain.AttemptState
		if err := tx.QueryRow(ctx,
			`SELECT state FROM attempts WHERE id = $1::uuid FOR UPDATE`,
			p.Fence.AttemptID).Scan(&fromAttempt); err != nil {
			return noRows(err)
		}
		if err := domain.AssertAttemptTransition(fromAttempt, p.AttemptState); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE attempts
			   SET state = $2, ended_at = now(), last_event_at = now(),
			       failure_class = COALESCE(NULLIF($3, ''), failure_class),
			       failure_summary = COALESCE(NULLIF($4, ''), failure_summary)
			 WHERE id = $1::uuid`,
			p.Fence.AttemptID, p.AttemptState, p.FailureClass, p.FailureSummary); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE leases SET released_at = now(), release_reason = $2
			 WHERE id = $1::uuid AND released_at IS NULL`,
			p.Fence.LeaseID, nullableText(p.Reason)); err != nil {
			return err
		}

		if !p.KeepReservations {
			if err := releaseTaskReservationsTx(ctx, tx, p.Fence.TaskID); err != nil {
				return err
			}
		}

		var status domain.TaskStatus
		var attemptsCount, maxAttempts int
		if err := tx.QueryRow(ctx, `
			SELECT status, attempts_count, max_attempts
			  FROM tasks WHERE id = $1::uuid FOR UPDATE`, p.Fence.TaskID,
		).Scan(&status, &attemptsCount, &maxAttempts); err != nil {
			return noRows(err)
		}

		next := p.NextTaskStatus
		if next == "" {
			next = domain.TaskStatusAfterAttempt(p.AttemptState, attemptsCount, maxAttempts)
		}
		if err := domain.AssertTaskTransition(status, next); err != nil {
			return err
		}

		var err error
		out, err = scanTask(tx.QueryRow(ctx, `
			UPDATE tasks
			   SET status = $2, active_lease_id = NULL, updated_at = now(),
			       completed_at = CASE WHEN $2 IN ('done','cancelled','superseded')
			                           THEN now() ELSE completed_at END
			 WHERE id = $1::uuid
			RETURNING `+strings.ReplaceAll(taskColumns, "t.", "tasks."),
			p.Fence.TaskID, next).Scan)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE sessions SET active_task_id = NULL, state = 'online_idle'
			 WHERE active_task_id = $1::uuid`, p.Fence.TaskID); err != nil {
			return err
		}

		return appendEvents(ctx, tx, out.OrganizationID, out.ProjectID, "",
			eventSpec{"task", out.ID, "task.released", domain.VisibilityTeamSummary, map[string]any{
				"task_ref": out.Ref, "status": string(next),
				"reason": p.Reason, "attempt_id": p.Fence.AttemptID,
			}})
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

// ReclaimedLease describes one lease the reconciler took back.
type ReclaimedLease struct {
	TaskID     domain.ID         `json:"task_id"`
	TaskRef    string            `json:"task_ref"`
	AttemptID  domain.ID         `json:"attempt_id"`
	LeaseID    domain.ID         `json:"lease_id"`
	NextStatus domain.TaskStatus `json:"next_status"`
	Worktree   string            `json:"worktree,omitempty"`
	Branch     string            `json:"branch,omitempty"`
}

// ReconcileLeases reclaims expired leases (DESIGN.md §10.4).
//
// Order matters here. The epoch is bumped as part of the same transaction that marks the
// attempt stale, so the moment a dead worker's lease is taken away, its epoch is already
// behind — it cannot publish a result into the gap between reclamation and the next claim.
//
// Worktrees are deliberately left on disk. A crashed run's branch is evidence, and a
// replacement attempt may adopt it after policy checks (§27.2, §27.6).
func (s *Store) ReconcileLeases(ctx context.Context, projectID domain.ID) ([]ReclaimedLease, error) {
	var out []ReclaimedLease

	err := s.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT l.id::text, l.task_id::text, l.attempt_id::text,
			       t.ref, t.status, t.attempts_count, t.max_attempts,
			       t.organization_id::text,
			       COALESCE(a.worktree_path, ''), COALESCE(a.branch, ''), a.state
			  FROM leases l
			  JOIN tasks t ON t.id = l.task_id
			  JOIN attempts a ON a.id = l.attempt_id
			 WHERE l.project_id = $1::uuid
			   AND l.released_at IS NULL
			   AND l.expires_at <= now()
			 FOR UPDATE OF l SKIP LOCKED`, projectID)
		if err != nil {
			return err
		}

		type expired struct {
			leaseID, taskID, attemptID, taskRef, orgID string
			status                                     domain.TaskStatus
			attemptState                               domain.AttemptState
			attemptsCount, maxAttempts                 int
			worktree, branch                           string
		}
		var batch []expired
		for rows.Next() {
			var e expired
			if err := rows.Scan(&e.leaseID, &e.taskID, &e.attemptID, &e.taskRef,
				&e.status, &e.attemptsCount, &e.maxAttempts, &e.orgID,
				&e.worktree, &e.branch, &e.attemptState); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, e := range batch {
			if _, err := tx.Exec(ctx, `
				UPDATE leases SET released_at = now(), release_reason = 'expired'
				 WHERE id = $1::uuid`, e.leaseID); err != nil {
				return err
			}

			if domain.CanTransitionAttempt(e.attemptState, domain.AttemptStale) {
				if _, err := tx.Exec(ctx, `
					UPDATE attempts
					   SET state = 'stale', ended_at = now(), last_event_at = now(),
					       failure_class = COALESCE(failure_class, 'lease_expired')
					 WHERE id = $1::uuid`, e.attemptID); err != nil {
					return err
				}
			}

			if err := releaseTaskReservationsTx(ctx, tx, e.taskID); err != nil {
				return err
			}

			next := domain.TaskStatusAfterAttempt(
				domain.AttemptStale, e.attemptsCount, e.maxAttempts)
			if !domain.CanTransitionTask(e.status, next) {
				// A task that has already moved on (to verifying, say) keeps its status;
				// only the lease and reservations are reclaimed.
				next = e.status
			}
			if _, err := tx.Exec(ctx, `
				UPDATE tasks
				   SET status = $2, active_lease_id = NULL,
				       fencing_epoch = fencing_epoch + 1, updated_at = now()
				 WHERE id = $1::uuid`, e.taskID, next); err != nil {
				return err
			}

			if _, err := tx.Exec(ctx, `
				UPDATE sessions SET active_task_id = NULL
				 WHERE active_task_id = $1::uuid`, e.taskID); err != nil {
				return err
			}

			if err := appendEvents(ctx, tx, e.orgID, projectID, "",
				eventSpec{"task", e.taskID, "lease.expired", domain.VisibilityTeamSummary, map[string]any{
					"task_ref": e.taskRef, "attempt_id": e.attemptID,
					"lease_id": e.leaseID, "status": string(next),
					"failure_class": "lease_expired",
				}}); err != nil {
				return err
			}

			out = append(out, ReclaimedLease{
				TaskID: e.taskID, TaskRef: e.taskRef, AttemptID: e.attemptID,
				LeaseID: e.leaseID, NextStatus: next,
				Worktree: e.worktree, Branch: e.branch,
			})
		}
		return nil
	})
	return out, err
}

// GetLease loads a lease by id.
func (s *Store) GetLease(ctx context.Context, id domain.ID) (domain.Lease, error) {
	l, err := scanLease(s.pool.QueryRow(ctx,
		`SELECT `+leaseColumns+` FROM leases WHERE id = $1::uuid`, id).Scan)
	return l, noRows(err)
}

// ActiveLeaseForTask returns the task's live lease, if any.
func (s *Store) ActiveLeaseForTask(ctx context.Context, taskID domain.ID) (domain.Lease, error) {
	l, err := scanLease(s.pool.QueryRow(ctx,
		`SELECT `+leaseColumns+` FROM leases
		  WHERE task_id = $1::uuid AND released_at IS NULL`, taskID).Scan)
	return l, noRows(err)
}
