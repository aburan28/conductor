package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aburan28/conductor/internal/domain"
)

// Delphi delegations (migration 0016): metadata for requests submitted to provider batch
// APIs. Nothing here holds a request body or an answer.

const delphiColumns = `
	id::text, organization_id::text, project_id::text, ref, COALESCE(principal_id::text, ''),
	COALESCE(task_id::text, ''), provider, model, body_sha256, label, batch_id, state, status,
	truncated, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	cost_usd::float8, error_type, error_detail, refusal_category, labels, submitted_at,
	finished_at, updated_at, profile, fallback_of`

func scanDelphi(scan func(...any) error) (domain.DelphiDelegation, error) {
	var d domain.DelphiDelegation
	var labels []byte
	err := scan(&d.ID, &d.OrganizationID, &d.ProjectID, &d.Ref, &d.PrincipalID, &d.TaskID,
		&d.Provider, &d.Model, &d.BodySHA256, &d.Label, &d.BatchID, &d.State, &d.Status,
		&d.Truncated, &d.StopReason, &d.InputTokens, &d.OutputTokens, &d.CacheReadTokens,
		&d.CacheWriteTokens, &d.CostUSD, &d.ErrorType, &d.ErrorDetail, &d.RefusalCategory,
		&labels, &d.SubmittedAt, &d.FinishedAt, &d.UpdatedAt, &d.Profile, &d.FallbackOf)
	if err == nil && len(labels) > 0 {
		err = json.Unmarshal(labels, &d.Labels)
	}
	return d, err
}

// CreateDelphiDelegation records a delegation in state submitting, before the provider is
// called, so a crash during the call leaves a row to reconcile rather than a batch nobody
// knows about. A ref already used in the project returns domain.ErrDuplicate together with
// the existing row.
func (s *Store) CreateDelphiDelegation(ctx context.Context, d domain.DelphiDelegation) (domain.DelphiDelegation, error) {
	labels, err := json.Marshal(d.Labels)
	if err != nil {
		return d, err
	}
	if d.Labels == nil {
		labels = []byte("{}")
	}
	out, err := scanDelphi(s.pool.QueryRow(ctx, `
		INSERT INTO delphi_delegations (organization_id, project_id, ref, principal_id, task_id,
		        provider, model, body_sha256, label, state, labels, profile, fallback_of)
		VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5::uuid, $6, $7, $8, $9, 'submitting', $10::jsonb, $11, $12)
		RETURNING `+delphiColumns,
		d.OrganizationID, d.ProjectID, d.Ref, nullable(d.PrincipalID), nullable(d.TaskID),
		d.Provider, d.Model, d.BodySHA256, d.Label, labels, d.Profile, d.FallbackOf).Scan)
	if isUniqueViolation(err, "delphi_delegations_project_id_ref_key") {
		existing, gerr := s.GetDelphiDelegation(ctx, d.ProjectID, d.Ref)
		if gerr != nil {
			return d, gerr
		}
		return existing, fmt.Errorf("%w: ref %q is already used in this project", domain.ErrDuplicate, d.Ref)
	}
	return out, err
}

// GetDelphiDelegation returns a project's delegation by ref.
func (s *Store) GetDelphiDelegation(ctx context.Context, projectID domain.ID, ref string) (domain.DelphiDelegation, error) {
	d, err := scanDelphi(s.pool.QueryRow(ctx,
		`SELECT `+delphiColumns+` FROM delphi_delegations WHERE project_id = $1::uuid AND ref = $2`,
		projectID, ref).Scan)
	return d, noRows(err)
}

// ListDelphiDelegations returns a project's delegations, newest first, optionally filtered
// by state.
func (s *Store) ListDelphiDelegations(ctx context.Context, projectID domain.ID, state string, limit int) ([]domain.DelphiDelegation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.queryDelphi(ctx, `
		SELECT `+delphiColumns+` FROM delphi_delegations
		WHERE project_id = $1::uuid AND ($2 = '' OR state = $2)
		ORDER BY submitted_at DESC, id LIMIT $3`, projectID, state, limit)
}

// ListOpenDelphiDelegations returns every delegation, in any project, that still waits on a
// provider: the background loop's work list.
func (s *Store) ListOpenDelphiDelegations(ctx context.Context) ([]domain.DelphiDelegation, error) {
	return s.queryDelphi(ctx, `
		SELECT `+delphiColumns+` FROM delphi_delegations
		WHERE state IN ('submitting', 'running')
		ORDER BY project_id, submitted_at`)
}

func (s *Store) queryDelphi(ctx context.Context, sql string, args ...any) ([]domain.DelphiDelegation, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.DelphiDelegation{}
	for rows.Next() {
		d, err := scanDelphi(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetDelphiRunning binds a submitting delegation to the batch the provider created.
func (s *Store) SetDelphiRunning(ctx context.Context, id domain.ID, batchID string) (domain.DelphiDelegation, error) {
	d, err := scanDelphi(s.pool.QueryRow(ctx, `
		UPDATE delphi_delegations SET batch_id = $2, state = 'running', updated_at = now()
		WHERE id = $1::uuid AND state = 'submitting'
		RETURNING `+delphiColumns, id, batchID).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, fmt.Errorf("%w: delegation is not waiting for its batch id", domain.ErrIllegalTransition)
	}
	return d, err
}

// DeleteUnsubmittedDelphi removes a delegation whose submit is known not to have created a
// batch, so its ref can be used again.
func (s *Store) DeleteUnsubmittedDelphi(ctx context.Context, id domain.ID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM delphi_delegations WHERE id = $1::uuid AND state = 'submitting'`, id)
	return err
}

// AbandonDelphi marks a submitting delegation as given up: an operator decided its batch
// was never created.
func (s *Store) AbandonDelphi(ctx context.Context, id domain.ID) (domain.DelphiDelegation, error) {
	d, err := scanDelphi(s.pool.QueryRow(ctx, `
		UPDATE delphi_delegations SET state = 'abandoned', updated_at = now(), finished_at = now()
		WHERE id = $1::uuid AND state = 'submitting'
		RETURNING `+delphiColumns, id).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, fmt.Errorf("%w: only a submitting delegation can be abandoned", domain.ErrIllegalTransition)
	}
	return d, err
}

// DelphiOutcome is what the loop learned when a delegation's batch ended.
type DelphiOutcome struct {
	Status           string
	Truncated        bool
	StopReason       string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
	ErrorType        string
	ErrorDetail      string
	RefusalCategory  string
	FinishedAt       time.Time
}

// FinishDelphi records a running delegation's outcome. It reports false when another
// replica finished it first, so side effects (usage, events) happen once.
func (s *Store) FinishDelphi(ctx context.Context, id domain.ID, o DelphiOutcome) (domain.DelphiDelegation, bool, error) {
	d, err := scanDelphi(s.pool.QueryRow(ctx, `
		UPDATE delphi_delegations SET state = 'finished', status = $2, truncated = $3,
		       stop_reason = $4, input_tokens = $5, output_tokens = $6, cache_read_tokens = $7,
		       cache_write_tokens = $8, cost_usd = $9, error_type = $10, error_detail = $11,
		       refusal_category = $12, finished_at = $13, updated_at = now()
		WHERE id = $1::uuid AND state = 'running'
		RETURNING `+delphiColumns,
		id, o.Status, o.Truncated, o.StopReason, o.InputTokens, o.OutputTokens, o.CacheReadTokens,
		o.CacheWriteTokens, o.CostUSD, o.ErrorType, o.ErrorDetail, o.RefusalCategory, o.FinishedAt).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	return d, err == nil, err
}
