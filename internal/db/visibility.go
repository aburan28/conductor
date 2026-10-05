package db

import (
	"context"

	"github.com/aburan28/conductor/internal/domain"
)

// TaskAccess is what a read path needs to decide what one principal may learn about another's
// task: whose it is and how widely it is shared. Read paths that do not carry the task itself
// — the event feed above all — look it up here rather than trusting the visibility a record
// was written with, because a task's visibility is the owner's current decision and a record
// written before it (or written carelessly) must not outrank it.
type TaskAccess struct {
	TaskID     domain.ID
	Ref        string
	CreatedBy  domain.ID
	Visibility domain.Visibility
}

// TaskAccessFor loads TaskAccess for the given task ids and project-scoped refs in one round
// trip, keyed both by id and by ref. Ids that are not UUIDs are ignored rather than failing the
// whole lookup, since they come from event payloads.
func (s *Store) TaskAccessFor(ctx context.Context, projectID domain.ID, ids []domain.ID, refs []string) (map[string]TaskAccess, error) {
	out := map[string]TaskAccess{}
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if looksLikeUUID(id) {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 && len(refs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, ref, created_by::text, visibility
		  FROM tasks
		 WHERE project_id = $1::uuid
		   AND (id = ANY($2::uuid[]) OR ref = ANY($3::text[]))`,
		projectID, valid, refs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a TaskAccess
		if err := rows.Scan(&a.TaskID, &a.Ref, &a.CreatedBy, &a.Visibility); err != nil {
			return nil, err
		}
		out[a.TaskID] = a
		out["ref:"+a.Ref] = a
	}
	return out, rows.Err()
}

// looksLikeUUID is a cheap shape check (8-4-4-4-12 hex), enough to keep a stray payload value
// from turning a batch lookup into a cast error.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}
