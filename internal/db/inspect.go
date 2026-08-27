package db

import (
	"context"

	"github.com/adamburan/conductor/internal/domain"
)

// ListRunners returns every runner registered for a project (or org-wide), most recently
// heard from first. Offline runners are included: a fleet view that hides the machine that
// just went quiet hides exactly the one an operator needs to see.
func (s *Store) ListRunners(ctx context.Context, projectID domain.ID) ([]domain.Runner, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+runnerColumns+`
		  FROM runners
		 WHERE project_id = $1::uuid OR project_id IS NULL
		 ORDER BY heartbeat_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Runner
	for rows.Next() {
		r, err := scanRunner(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if out == nil {
		out = []domain.Runner{}
	}
	return out, rows.Err()
}
