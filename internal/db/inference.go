package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aburan28/conductor/internal/domain"
)

// ---------------------------------------------------------------------------
// Inference endpoints and discovered models
// ---------------------------------------------------------------------------

const endpointColumns = `e.id::text, e.organization_id::text, e.project_id::text, e.name, e.kind,
	e.base_url, e.api_key_secret, e.enabled, e.last_status, e.last_error, e.last_refresh_at,
	(SELECT count(*) FROM discovered_models m WHERE m.endpoint_id = e.id AND m.missing_since IS NULL),
	e.created_at, e.updated_at`

func scanEndpoint(row pgx.Row) (domain.InferenceEndpoint, error) {
	var e domain.InferenceEndpoint
	err := row.Scan(&e.ID, &e.OrganizationID, &e.ProjectID, &e.Name, &e.Kind, &e.BaseURL,
		&e.APIKeySecret, &e.Enabled, &e.LastStatus, &e.LastError, &e.LastRefreshAt,
		&e.ModelCount, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

// CreateInferenceEndpoint registers an endpoint. A second endpoint of the same name in the
// project is domain.ErrInvalidArgument, because names are what profiles select by.
func (s *Store) CreateInferenceEndpoint(ctx context.Context, e domain.InferenceEndpoint, by domain.ID) (domain.InferenceEndpoint, error) {
	row := s.pool.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO inference_endpoints (organization_id, project_id, name, kind, base_url,
			                                 api_key_secret, enabled, created_by)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8::uuid)
			RETURNING *)
		SELECT `+endpointColumns+` FROM ins e`,
		e.OrganizationID, e.ProjectID, e.Name, e.Kind, e.BaseURL, e.APIKeySecret, e.Enabled, nullable(by))
	out, err := scanEndpoint(row)
	if err != nil {
		if isUniqueViolation(err, "inference_endpoints_project_id_name_key") {
			return domain.InferenceEndpoint{}, domain.ErrInvalidArgument
		}
		return domain.InferenceEndpoint{}, err
	}
	return out, nil
}

// UpdateInferenceEndpoint changes the fields that may change after creation. The name and
// kind are fixed: profiles refer to the name, and a different kind is a different endpoint.
func (s *Store) UpdateInferenceEndpoint(ctx context.Context, projectID, id domain.ID, baseURL, keySecret string, enabled bool) (domain.InferenceEndpoint, error) {
	row := s.pool.QueryRow(ctx, `
		WITH upd AS (
			UPDATE inference_endpoints
			   SET base_url = $3, api_key_secret = $4, enabled = $5, updated_at = now()
			 WHERE project_id = $1::uuid AND id = $2::uuid
			RETURNING *)
		SELECT `+endpointColumns+` FROM upd e`, projectID, id, baseURL, keySecret, enabled)
	out, err := scanEndpoint(row)
	return out, noRows(err)
}

// GetInferenceEndpoint finds an endpoint by id or by name within a project.
func (s *Store) GetInferenceEndpoint(ctx context.Context, projectID domain.ID, idOrName string) (domain.InferenceEndpoint, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+endpointColumns+`
		  FROM inference_endpoints e
		 WHERE e.project_id = $1::uuid AND (e.id::text = $2 OR e.name = $2)`, projectID, idOrName)
	out, err := scanEndpoint(row)
	return out, noRows(err)
}

// ListInferenceEndpoints returns a project's endpoints.
func (s *Store) ListInferenceEndpoints(ctx context.Context, projectID domain.ID) ([]domain.InferenceEndpoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+endpointColumns+` FROM inference_endpoints e
		 WHERE e.project_id = $1::uuid ORDER BY e.name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEndpoints(rows)
}

// ListEnabledInferenceEndpoints returns every enabled endpoint on the server, for the
// background refresher.
func (s *Store) ListEnabledInferenceEndpoints(ctx context.Context) ([]domain.InferenceEndpoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+endpointColumns+` FROM inference_endpoints e
		 WHERE e.enabled ORDER BY e.last_refresh_at NULLS FIRST, e.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEndpoints(rows)
}

func collectEndpoints(rows pgx.Rows) ([]domain.InferenceEndpoint, error) {
	out := []domain.InferenceEndpoint{}
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteInferenceEndpoint removes an endpoint and the models it reported.
func (s *Store) DeleteInferenceEndpoint(ctx context.Context, projectID, id domain.ID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM inference_endpoints WHERE project_id = $1::uuid AND id = $2::uuid`, projectID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// DiscoveredInput is one model from a provider's list, ready to store.
type DiscoveredInput struct {
	Model         string
	DisplayName   string
	Created       *time.Time
	ContextWindow int
}

// RefreshDiff is what a successful refresh changed.
type RefreshDiff struct {
	// Added are models never seen on this endpoint before.
	Added []string
	// Returned are models that had gone missing and are served again.
	Returned []string
	// Retired are models the endpoint no longer lists.
	Retired []string
}

// RecordInferenceRefresh stores a successful list in one transaction and reports the diff.
// A model the provider stops listing is marked missing, not deleted.
func (s *Store) RecordInferenceRefresh(ctx context.Context, endpointID domain.ID, models []DiscoveredInput, now time.Time) (RefreshDiff, error) {
	var diff RefreshDiff
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return diff, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	known := map[string]bool{} // model -> currently missing
	rows, err := tx.Query(ctx,
		`SELECT model, missing_since IS NOT NULL FROM discovered_models WHERE endpoint_id = $1::uuid FOR UPDATE`, endpointID)
	if err != nil {
		return diff, err
	}
	for rows.Next() {
		var name string
		var missing bool
		if err := rows.Scan(&name, &missing); err != nil {
			rows.Close()
			return diff, err
		}
		known[name] = missing
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return diff, err
	}

	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if m.Model == "" || seen[m.Model] {
			continue
		}
		seen[m.Model] = true
		wasMissing, existed := known[m.Model]
		switch {
		case !existed:
			diff.Added = append(diff.Added, m.Model)
		case wasMissing:
			diff.Returned = append(diff.Returned, m.Model)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO discovered_models (endpoint_id, model, display_name, created_upstream,
			                               context_window, first_seen_at, last_seen_at)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $6)
			ON CONFLICT (endpoint_id, model) DO UPDATE SET
			    display_name = EXCLUDED.display_name,
			    created_upstream = COALESCE(EXCLUDED.created_upstream, discovered_models.created_upstream),
			    context_window = CASE WHEN EXCLUDED.context_window > 0
			                          THEN EXCLUDED.context_window ELSE discovered_models.context_window END,
			    last_seen_at = EXCLUDED.last_seen_at,
			    missing_since = NULL`,
			endpointID, m.Model, m.DisplayName, m.Created, m.ContextWindow, now); err != nil {
			return diff, err
		}
	}
	for name, missing := range known {
		if seen[name] || missing {
			continue
		}
		diff.Retired = append(diff.Retired, name)
		if _, err := tx.Exec(ctx,
			`UPDATE discovered_models SET missing_since = $3 WHERE endpoint_id = $1::uuid AND model = $2`,
			endpointID, name, now); err != nil {
			return diff, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE inference_endpoints
		   SET last_status = 'ok', last_error = '', last_refresh_at = $2, updated_at = now()
		 WHERE id = $1::uuid`, endpointID, now); err != nil {
		return diff, err
	}
	return diff, tx.Commit(ctx)
}

// RecordInferenceFailure notes that a refresh failed. The previously discovered models are
// left alone: an endpoint that is briefly down has not retired its models.
func (s *Store) RecordInferenceFailure(ctx context.Context, endpointID domain.ID, reason string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE inference_endpoints
		   SET last_status = 'error', last_error = $2, last_refresh_at = $3, updated_at = now()
		 WHERE id = $1::uuid`, endpointID, reason, now)
	return err
}

// ListDiscoveredModels returns what an endpoint serves, newest first. Models the endpoint no
// longer lists are included only when includeMissing is set.
func (s *Store) ListDiscoveredModels(ctx context.Context, endpointID domain.ID, includeMissing bool) ([]domain.DiscoveredModel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT endpoint_id::text, model, display_name, created_upstream, context_window,
		       first_seen_at, last_seen_at, missing_since
		  FROM discovered_models
		 WHERE endpoint_id = $1::uuid AND ($2 OR missing_since IS NULL)
		 ORDER BY created_upstream DESC NULLS LAST, first_seen_at DESC, model`, endpointID, includeMissing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.DiscoveredModel{}
	for rows.Next() {
		var m domain.DiscoveredModel
		if err := rows.Scan(&m.EndpointID, &m.Model, &m.DisplayName, &m.CreatedUpstream,
			&m.ContextWindow, &m.FirstSeenAt, &m.LastSeenAt, &m.MissingSince); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SelectorProfiles returns the catalog profiles whose model is chosen by discovery from this
// project's endpoints.
func (s *Store) SelectorProfiles(ctx context.Context, projectID domain.ID) ([]domain.ModelProfile, error) {
	rows, err := s.pool.Query(ctx, modelProfileSelect+`
		 WHERE select_project_id = $1::uuid AND select_endpoint <> ''
		 ORDER BY alias, harness`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanModelProfiles(rows)
}

// SetProfileModel records the model discovery chose for a profile and when. It reports
// whether the model changed, so a confirmation that changes nothing is not announced. A
// pinned profile is left alone.
func (s *Store) SetProfileModel(ctx context.Context, profileID domain.ID, model string, at time.Time) (changed bool, previous string, err error) {
	var pinned bool
	err = s.pool.QueryRow(ctx, `
		WITH old AS (SELECT model, pinned FROM model_profiles WHERE id = $1::uuid),
		     upd AS (UPDATE model_profiles SET model = $2, resolved_at = $3
		              WHERE id = $1::uuid AND NOT pinned RETURNING 1)
		SELECT model, pinned FROM old`, profileID, model, at).Scan(&previous, &pinned)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", domain.ErrNotFound
	}
	return !pinned && previous != model, previous, err
}

// PinProfileModel records a person's choice of model for a catalog profile. Discovery and
// bootstrap leave a pinned profile alone until UnpinProfile.
func (s *Store) PinProfileModel(ctx context.Context, orgID domain.ID, alias, harness, model string) (domain.ModelProfile, error) {
	rows, err := s.pool.Query(ctx, `
		WITH upd AS (
			UPDATE model_profiles SET model = $4, pinned = true, resolved_at = now()
			 WHERE organization_id = $1::uuid AND alias = $2 AND harness = $3
			RETURNING id)
		`+modelProfileSelect+` WHERE id IN (SELECT id FROM upd)`, orgID, alias, harness, model)
	if err != nil {
		return domain.ModelProfile{}, err
	}
	defer rows.Close()
	return firstProfile(rows)
}

// UnpinProfile hands a profile back to discovery (or to the models file at the next
// bootstrap, for a profile that names its model outright).
func (s *Store) UnpinProfile(ctx context.Context, orgID domain.ID, alias, harness string) (domain.ModelProfile, error) {
	rows, err := s.pool.Query(ctx, `
		WITH upd AS (
			UPDATE model_profiles SET pinned = false
			 WHERE organization_id = $1::uuid AND alias = $2 AND harness = $3
			RETURNING id)
		`+modelProfileSelect+` WHERE id IN (SELECT id FROM upd)`, orgID, alias, harness)
	if err != nil {
		return domain.ModelProfile{}, err
	}
	defer rows.Close()
	return firstProfile(rows)
}

func firstProfile(rows pgx.Rows) (domain.ModelProfile, error) {
	profiles, err := scanModelProfiles(rows)
	if err != nil {
		return domain.ModelProfile{}, err
	}
	if len(profiles) == 0 {
		return domain.ModelProfile{}, domain.ErrNotFound
	}
	return profiles[0], nil
}
