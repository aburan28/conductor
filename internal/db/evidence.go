package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
)

// ---------------------------------------------------------------------------
// Artifacts and validation evidence
// ---------------------------------------------------------------------------

func (s *Store) AddArtifact(ctx context.Context, a domain.Artifact) (domain.Artifact, error) {
	if a.Visibility == "" {
		a.Visibility = domain.VisibilityTeamArtifacts
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO artifacts (project_id, task_id, attempt_id, kind, uri,
		        content_sha256, size_bytes, visibility)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8)
		RETURNING id::text, created_at`,
		a.ProjectID, a.TaskID, nullable(a.AttemptID), a.Kind, a.URI,
		a.ContentSHA256, a.SizeBytes, a.Visibility,
	).Scan(&a.ID, &a.CreatedAt)
	return a, err
}

func (s *Store) ListArtifacts(ctx context.Context, taskID domain.ID) ([]domain.Artifact, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, project_id::text, task_id::text, COALESCE(attempt_id::text, ''),
		       kind, uri, content_sha256, size_bytes, visibility, created_at
		  FROM artifacts WHERE task_id = $1::uuid ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Artifact
	for rows.Next() {
		var a domain.Artifact
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.TaskID, &a.AttemptID,
			&a.Kind, &a.URI, &a.ContentSHA256, &a.SizeBytes, &a.Visibility,
			&a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SubmitEvidence records a worker result manifest (DESIGN.md §15.1).
//
// The fence is verified first. A worker whose lease expired can keep writing in its own
// worktree, but it cannot publish a result — that is the entire purpose of the epoch
// (§7.5), and it is checked here rather than trusted from the caller.
//
// Validation rows are attributed to the runner, not the model. "Tests passed" is a claim
// until a runner says it observed exit code 0 (§25.5).
func (s *Store) SubmitEvidence(ctx context.Context, m domain.EvidenceManifest, runnerID domain.ID) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		fence := domain.Fence{
			TaskID: m.TaskID, AttemptID: m.AttemptID,
			LeaseID: m.LeaseID, FencingEpoch: m.FencingEpoch,
		}
		if err := assertFence(ctx, tx, fence); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE attempts
			   SET commit_sha = COALESCE(NULLIF($2, ''), commit_sha),
			       base_commit_sha = COALESCE(NULLIF($3, ''), base_commit_sha),
			       changed_paths = CASE WHEN $4::text[] IS NULL OR cardinality($4::text[]) = 0
			                            THEN changed_paths ELSE $4::text[] END,
			       last_event_at = now()
			 WHERE id = $1::uuid`,
			m.AttemptID, m.CommitSHA, m.BaseSHA, m.ChangedPaths); err != nil {
			return err
		}

		for _, c := range m.Commands {
			if _, err := tx.Exec(ctx, `
				INSERT INTO validation_results (attempt_id, task_id, command_id, command,
				        exit_code, duration_ms, runner_id)
				VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7::uuid)`,
				m.AttemptID, m.TaskID, c.CommandID, c.Command, c.ExitCode,
				c.DurationMS, nullable(runnerID)); err != nil {
				return err
			}
		}

		if m.CommitSHA != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO artifacts (project_id, task_id, attempt_id, kind, uri, content_sha256)
				SELECT project_id, task_id, id, 'commit', $2, $3 FROM attempts WHERE id = $1::uuid`,
				m.AttemptID, m.CommitSHA, m.DiffSHA256); err != nil {
				return err
			}
		}

		if len(m.AcceptanceResults) > 0 {
			body, err := marshalJSON(m.AcceptanceResults)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE tasks SET acceptance_criteria = $2, updated_at = now()
				  WHERE id = $1::uuid`, m.TaskID, body); err != nil {
				return err
			}
		}

		var orgID, projectID, ref string
		if err := tx.QueryRow(ctx,
			`SELECT organization_id::text, project_id::text, ref FROM tasks WHERE id = $1::uuid`,
			m.TaskID).Scan(&orgID, &projectID, &ref); err != nil {
			return noRows(err)
		}
		return appendEvents(ctx, tx, orgID, projectID, "",
			eventSpec{"attempt", m.AttemptID, "attempt.evidence", domain.VisibilityTeamArtifacts,
				map[string]any{
					"task_ref": ref, "commit_sha": m.CommitSHA,
					"changed_paths": m.ChangedPaths, "count": len(m.Commands),
				}})
	})
}

func (s *Store) ListValidation(ctx context.Context, taskID domain.ID) ([]domain.ValidationResult, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, attempt_id::text, task_id::text, command_id, command,
		       exit_code, duration_ms, COALESCE(artifact_id::text, ''),
		       COALESCE(runner_id::text, ''), created_at
		  FROM validation_results WHERE task_id = $1::uuid ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ValidationResult
	for rows.Next() {
		var v domain.ValidationResult
		if err := rows.Scan(&v.ID, &v.AttemptID, &v.TaskID, &v.CommandID, &v.Command,
			&v.ExitCode, &v.DurationMS, &v.ArtifactID, &v.RunnerID, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RequiredChecksPassed reports whether every required check has a passing result on the
// attempt. Verification consults this before allowing done (DESIGN.md §15.2).
func (s *Store) RequiredChecksPassed(ctx context.Context, attemptID domain.ID, required []string) (bool, []string, error) {
	if len(required) == 0 {
		return true, nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT command, exit_code FROM validation_results WHERE attempt_id = $1::uuid`, attemptID)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()

	passed := map[string]bool{}
	for rows.Next() {
		var cmd string
		var code int
		if err := rows.Scan(&cmd, &code); err != nil {
			return false, nil, err
		}
		if code == 0 {
			passed[cmd] = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, nil, err
	}

	var missing []string
	for _, r := range required {
		if !passed[r] {
			missing = append(missing, r)
		}
	}
	return len(missing) == 0, missing, nil
}

// ---------------------------------------------------------------------------
// Review findings
// ---------------------------------------------------------------------------

func (s *Store) AddReviewFinding(ctx context.Context, f domain.ReviewFinding) (domain.ReviewFinding, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO review_findings (task_id, attempt_id, reviewer_principal, severity,
		        category, path, line, summary)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8)
		RETURNING id::text, created_at`,
		f.TaskID, nullable(f.AttemptID), nullable(f.ReviewerPrincipal), f.Severity,
		f.Category, f.Path, nullIfZero(f.Line), f.Summary,
	).Scan(&f.ID, &f.CreatedAt)
	return f, err
}

func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func (s *Store) ListReviewFindings(ctx context.Context, taskID domain.ID) ([]domain.ReviewFinding, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, task_id::text, COALESCE(attempt_id::text, ''),
		       COALESCE(reviewer_principal::text, ''), severity, category, path,
		       COALESCE(line, 0), summary, resolved, created_at
		  FROM review_findings WHERE task_id = $1::uuid ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ReviewFinding
	for rows.Next() {
		var f domain.ReviewFinding
		if err := rows.Scan(&f.ID, &f.TaskID, &f.AttemptID, &f.ReviewerPrincipal,
			&f.Severity, &f.Category, &f.Path, &f.Line, &f.Summary,
			&f.Resolved, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Handoffs
// ---------------------------------------------------------------------------

// CreateHandoff records a handoff bundle and releases the source attempt's lease.
//
// The bundle is structured continuation state, never a transcript (DESIGN.md §22). The next
// harness receives the task card, decisions, and open questions — enough to continue, and
// nothing that belonged to the previous session's conversation.
func (s *Store) CreateHandoff(ctx context.Context, h domain.Handoff, fence domain.Fence, releaseLease bool) (domain.Handoff, error) {
	body, err := marshalJSON(h.Bundle)
	if err != nil {
		return domain.Handoff{}, err
	}
	if h.Visibility == "" {
		h.Visibility = domain.VisibilityTeamSummary
	}

	err = s.Tx(ctx, func(tx pgx.Tx) error {
		if fence.LeaseID != "" {
			if err := assertFence(ctx, tx, fence); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO handoffs (task_id, from_attempt_id, to_harness, to_role,
			        visibility, bundle, created_by)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7::uuid)
			RETURNING id::text, created_at`,
			h.TaskID, nullable(h.FromAttemptID), h.ToHarness, h.ToRole,
			h.Visibility, body, h.CreatedBy,
		).Scan(&h.ID, &h.CreatedAt); err != nil {
			return err
		}

		if releaseLease && fence.LeaseID != "" {
			if _, err := tx.Exec(ctx, `
				UPDATE leases SET released_at = now(), release_reason = 'handoff'
				 WHERE id = $1::uuid AND released_at IS NULL`, fence.LeaseID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE attempts SET state = 'succeeded', ended_at = now(), last_event_at = now()
				 WHERE id = $1::uuid AND state IN ('queued','preparing_workspace',
				       'starting_harness','running','waiting_for_approval',
				       'waiting_for_input','paused_conflict')`, fence.AttemptID); err != nil {
				return err
			}
			if err := releaseTaskReservationsTx(ctx, tx, h.TaskID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET status = 'ready', active_lease_id = NULL, updated_at = now()
				 WHERE id = $1::uuid
				   AND status IN ('claimed','running','blocked_conflict',
				                  'blocked_dependency','blocked_input')`, h.TaskID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE sessions SET active_task_id = NULL, state = 'online_idle'
				 WHERE active_task_id = $1::uuid`, h.TaskID); err != nil {
				return err
			}
		}

		var orgID, projectID, ref string
		if err := tx.QueryRow(ctx,
			`SELECT organization_id::text, project_id::text, ref FROM tasks WHERE id = $1::uuid`,
			h.TaskID).Scan(&orgID, &projectID, &ref); err != nil {
			return noRows(err)
		}
		return appendEvents(ctx, tx, orgID, projectID, h.CreatedBy,
			eventSpec{"task", h.TaskID, "task.handoff", h.Visibility, map[string]any{
				"task_ref": ref, "harness": h.ToHarness, "role": h.ToRole,
			}})
	})
	return h, err
}

// LatestHandoff returns the most recent handoff bundle for a task, which a new attempt
// receives in place of any prior conversation.
func (s *Store) LatestHandoff(ctx context.Context, taskID domain.ID) (domain.Handoff, error) {
	var h domain.Handoff
	var body []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, task_id::text, COALESCE(from_attempt_id::text, ''),
		       COALESCE(to_attempt_id::text, ''), to_harness, to_role, visibility,
		       bundle, created_by::text, created_at
		  FROM handoffs WHERE task_id = $1::uuid ORDER BY created_at DESC LIMIT 1`, taskID,
	).Scan(&h.ID, &h.TaskID, &h.FromAttemptID, &h.ToAttemptID, &h.ToHarness, &h.ToRole,
		&h.Visibility, &body, &h.CreatedBy, &h.CreatedAt)
	if err != nil {
		return domain.Handoff{}, noRows(err)
	}
	if err := decodeJSON(body, &h.Bundle); err != nil {
		return domain.Handoff{}, err
	}
	return h, nil
}

// ---------------------------------------------------------------------------
// Policy decisions
// ---------------------------------------------------------------------------

// RecordDecision persists why the system routed, blocked, or escalated something. This is
// what makes the policy-audit dashboard view possible (DESIGN.md §26.4).
func (s *Store) RecordDecision(ctx context.Context, d domain.PolicyDecision) error {
	body, err := marshalJSON(d.Rationale)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO policy_decisions (project_id, task_id, attempt_id, kind, decision, rationale)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6)`,
		d.ProjectID, nullable(d.TaskID), nullable(d.AttemptID), d.Kind, d.Decision, body)
	return err
}

func (s *Store) ListDecisions(ctx context.Context, taskID domain.ID, limit int) ([]domain.PolicyDecision, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, project_id::text, COALESCE(task_id::text, ''),
		       COALESCE(attempt_id::text, ''), kind, decision, rationale, created_at
		  FROM policy_decisions WHERE task_id = $1::uuid
		 ORDER BY created_at DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PolicyDecision
	for rows.Next() {
		var d domain.PolicyDecision
		var body []byte
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.TaskID, &d.AttemptID,
			&d.Kind, &d.Decision, &body, &d.CreatedAt); err != nil {
			return nil, err
		}
		if err := decodeJSON(body, &d.Rationale); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

// RememberIdempotent stores a response against an idempotency key.
func (s *Store) RememberIdempotent(ctx context.Context, key string, principal domain.ID, requestHash string, status int, response []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (key, principal_id, request_hash, status_code, response)
		VALUES ($1, $2::uuid, $3, $4, $5)
		ON CONFLICT (key, principal_id) DO NOTHING`,
		key, principal, requestHash, status, response)
	return err
}

// RecallIdempotent returns a stored response for a repeated request.
//
// A mismatched body under the same key is a client bug, and returning the old response would
// hide it. Surfacing it as an error is the only honest option.
func (s *Store) RecallIdempotent(ctx context.Context, key string, principal domain.ID, requestHash string) (int, []byte, bool, error) {
	var status int
	var response []byte
	var storedHash string
	err := s.pool.QueryRow(ctx, `
		SELECT status_code, response, request_hash FROM idempotency_keys
		 WHERE key = $1 AND principal_id = $2::uuid`, key, principal,
	).Scan(&status, &response, &storedHash)
	if err != nil {
		if err := noRows(err); err == domain.ErrNotFound {
			return 0, nil, false, nil
		}
		return 0, nil, false, err
	}
	if storedHash != requestHash {
		return 0, nil, false, fmt.Errorf(
			"%w: idempotency key %q was used with a different request body", domain.ErrInvalidArgument, key)
	}
	return status, response, true, nil
}
