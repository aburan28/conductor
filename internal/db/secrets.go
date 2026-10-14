package db

import (
	"context"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

// ProjectSecret is one named credential stored for a project. The sealed ciphertext never
// leaves the store layer except to the vault that opened it; JSON tags keep it off any
// accidental response serialization.
type ProjectSecret struct {
	ID             domain.ID `json:"id"`
	OrganizationID domain.ID `json:"-"`
	ProjectID      domain.ID `json:"project_id"`
	Name           string    `json:"name"`
	Hint           string    `json:"hint"`
	Description    string    `json:"description"`
	CreatedBy      domain.ID `json:"created_by,omitempty"`
	UpdatedBy      domain.ID `json:"updated_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	ValueSealed string `json:"-"`
}

const projectSecretColumns = `
	id::text, organization_id::text, project_id::text, name, hint, description,
	COALESCE(created_by::text, ''), COALESCE(updated_by::text, ''),
	created_at, updated_at, value_sealed`

func scanProjectSecret(scan func(...any) error) (ProjectSecret, error) {
	var s ProjectSecret
	err := scan(&s.ID, &s.OrganizationID, &s.ProjectID, &s.Name, &s.Hint, &s.Description,
		&s.CreatedBy, &s.UpdatedBy, &s.CreatedAt, &s.UpdatedAt, &s.ValueSealed)
	return s, err
}

// UpsertProjectSecret inserts or replaces a sealed secret by (project, name).
func (s *Store) UpsertProjectSecret(ctx context.Context, sec ProjectSecret) (ProjectSecret, error) {
	return scanProjectSecret(s.pool.QueryRow(ctx, `
		INSERT INTO project_secrets (organization_id, project_id, name, value_sealed, hint,
		        description, created_by, updated_by)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7::uuid, $7::uuid)
		ON CONFLICT (project_id, name) DO UPDATE SET
		        value_sealed = EXCLUDED.value_sealed,
		        hint = EXCLUDED.hint,
		        description = EXCLUDED.description,
		        updated_by = EXCLUDED.updated_by,
		        updated_at = now()
		RETURNING `+projectSecretColumns,
		sec.OrganizationID, sec.ProjectID, sec.Name, sec.ValueSealed, sec.Hint,
		sec.Description, nullable(sec.UpdatedBy)).Scan)
}

// ListProjectSecrets returns metadata for a project's secrets (including sealed blobs for
// the vault). Callers that serialize for clients must drop ValueSealed.
func (s *Store) ListProjectSecrets(ctx context.Context, projectID domain.ID) ([]ProjectSecret, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+projectSecretColumns+` FROM project_secrets
		 WHERE project_id = $1::uuid ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectSecret{}
	for rows.Next() {
		sec, err := scanProjectSecret(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, rows.Err()
}

// GetProjectSecret finds one secret within a project.
func (s *Store) GetProjectSecret(ctx context.Context, projectID domain.ID, name string) (ProjectSecret, error) {
	sec, err := scanProjectSecret(s.pool.QueryRow(ctx, `
		SELECT `+projectSecretColumns+` FROM project_secrets
		 WHERE project_id = $1::uuid AND name = $2`, projectID, name).Scan)
	return sec, noRows(err)
}

// DeleteProjectSecret removes a secret by name within a project.
func (s *Store) DeleteProjectSecret(ctx context.Context, projectID domain.ID, name string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM project_secrets WHERE project_id = $1::uuid AND name = $2`, projectID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// CountProjectSecrets returns how many secrets a project holds.
func (s *Store) CountProjectSecrets(ctx context.Context, projectID domain.ID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM project_secrets WHERE project_id = $1::uuid`, projectID).Scan(&n)
	return n, err
}
