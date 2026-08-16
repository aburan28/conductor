package db

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
)

// ---------------------------------------------------------------------------
// Intents
// ---------------------------------------------------------------------------

const intentColumns = `
	id::text, project_id::text, COALESCE(session_id::text, ''), principal_id::text,
	COALESCE(task_id::text, ''), COALESCE(external_ref, ''), visibility,
	public_summary, verb, resource, fingerprint, minhash, scopes, outcome,
	created_at, expires_at`

func scanIntent(scan func(...any) error) (domain.Intent, error) {
	var i domain.Intent
	var scopes []byte
	if err := scan(&i.ID, &i.ProjectID, &i.SessionID, &i.PrincipalID,
		&i.TaskID, &i.ExternalRef, &i.Visibility,
		&i.PublicSummary, &i.Verb, &i.Resource, &i.Fingerprint, &i.MinHash, &scopes,
		&i.Outcome, &i.CreatedAt, &i.ExpiresAt); err != nil {
		return domain.Intent{}, err
	}
	if err := decodeJSON(scopes, &i.Scopes); err != nil {
		return domain.Intent{}, err
	}
	return i, nil
}

// RecordIntent stores a declared intent (DESIGN.md §12.1).
//
// The public summary is clamped rather than trusted: it is the one caller-supplied free-text
// field that becomes visible to the team, and an agent that pastes its context window into
// it should be truncated, not obeyed.
func (s *Store) RecordIntent(ctx context.Context, i domain.Intent, ttl time.Duration) (domain.Intent, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if i.Visibility == "" {
		i.Visibility = domain.VisibilityTeamSummary
	}
	if i.Outcome == "" {
		i.Outcome = domain.OutcomeAllow
	}
	summary := privacy.ClampSummary(i.PublicSummary)
	if i.Visibility == domain.VisibilityPrivate {
		// A private intent contributes its fingerprint to duplicate detection and nothing
		// else. There is no summary to store.
		summary = ""
	}
	scopes, err := marshalJSON(i.Scopes)
	if err != nil {
		return domain.Intent{}, err
	}
	return scanIntent(s.pool.QueryRow(ctx, `
		INSERT INTO intents (project_id, session_id, principal_id, task_id, external_ref,
		        visibility, public_summary, verb, resource, fingerprint, minhash, scopes,
		        outcome, expires_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12,
		        $13, now() + $14::interval)
		RETURNING `+intentColumns,
		i.ProjectID, nullable(i.SessionID), i.PrincipalID, nullable(i.TaskID),
		nullableText(i.ExternalRef), i.Visibility, summary, i.Verb, i.Resource,
		i.Fingerprint, i.MinHash, scopes, i.Outcome, ttl.String(),
	).Scan)
}

// LiveIntents lists unexpired intents, used for the "who is about to do what" view and for
// duplicate detection against work that has not become a task yet.
func (s *Store) LiveIntents(ctx context.Context, projectID domain.ID) ([]domain.Intent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+intentColumns+` FROM intents
		  WHERE project_id = $1::uuid AND expires_at > now()
		  ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Intent
	for rows.Next() {
		i, err := scanIntent(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// PruneIntents deletes expired intents. They are ephemeral coordination state, not history.
func (s *Store) PruneIntents(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM intents WHERE expires_at < now() - interval '1 hour'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// Duplicate detection
// ---------------------------------------------------------------------------

// DuplicateCandidate is an existing piece of work that resembles a proposed one.
type DuplicateCandidate struct {
	TaskID      domain.ID         `json:"task_id"`
	TaskRef     string            `json:"task_ref"`
	Title       string            `json:"title,omitempty"`
	Status      domain.TaskStatus `json:"status"`
	Visibility  domain.Visibility `json:"-"`
	OwnerID     domain.ID         `json:"-"`
	Owner       string            `json:"owner"`
	Branch      string            `json:"branch,omitempty"`
	ExternalRef string            `json:"external_ref,omitempty"`
	// Exact means the fingerprints matched, which is a much stronger signal than a
	// similarity score and is what triggers block_duplicate.
	Exact      bool    `json:"exact"`
	Similarity float64 `json:"-"`
}

// FindDuplicates compares a fingerprint and signature against open tasks in the project.
//
// This is the privacy-preserving half of the design working end to end: the comparison
// happens entirely over HMAC'd values, so the server detects that two people are building
// the same thing without ever holding either description in a form it can read back.
func (s *Store) FindDuplicates(
	ctx context.Context,
	projectID domain.ID,
	fingerprint string,
	signature []int64,
	externalRef string,
	threshold float64,
	excludeTask domain.ID,
) ([]DuplicateCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text, t.ref, t.title, t.status, t.visibility, t.created_by::text,
		       p.handle, COALESCE(t.external_ref, ''),
		       COALESCE(t.intent_fingerprint, ''), COALESCE(t.intent_minhash, '{}'),
		       COALESCE((SELECT a.branch FROM attempts a
		                  WHERE a.task_id = t.id ORDER BY a.attempt_number DESC LIMIT 1), '')
		  FROM tasks t
		  JOIN principals p ON p.id = t.created_by
		 WHERE t.project_id = $1::uuid
		   AND t.status NOT IN ('done','cancelled','superseded','failed')
		   AND ($2::uuid IS NULL OR t.id <> $2::uuid)`,
		projectID, nullable(excludeTask))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DuplicateCandidate
	for rows.Next() {
		var c DuplicateCandidate
		var otherFingerprint string
		var otherSignature []int64
		if err := rows.Scan(&c.TaskID, &c.TaskRef, &c.Title, &c.Status, &c.Visibility,
			&c.OwnerID, &c.Owner, &c.ExternalRef,
			&otherFingerprint, &otherSignature, &c.Branch); err != nil {
			return nil, err
		}

		switch {
		case externalRef != "" && c.ExternalRef == externalRef:
			// Same tracker issue is definitionally the same work (DESIGN.md §6.1).
			c.Exact, c.Similarity = true, 1
		case fingerprint != "" && otherFingerprint == fingerprint:
			c.Exact, c.Similarity = true, 1
		default:
			c.Similarity = privacy.Similarity(signature, otherSignature)
			if c.Similarity < threshold {
				continue
			}
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Exact matches first, then by descending similarity, so a caller taking the head of
	// the list gets the strongest signal.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Exact != out[j].Exact {
			return out[i].Exact
		}
		return out[i].Similarity > out[j].Similarity
	})
	return out, nil
}

// ---------------------------------------------------------------------------
// Conflict edges
// ---------------------------------------------------------------------------

const conflictColumns = `
	id::text, project_id::text, task_a::text, task_b::text, kind, severity, weight,
	suggestion, detail, state, COALESCE(resolution_note, ''),
	COALESCE(resolved_by::text, ''), detected_at, updated_at, resolved_at`

func scanConflict(scan func(...any) error) (domain.ConflictEdge, error) {
	var e domain.ConflictEdge
	var detail []byte
	if err := scan(&e.ID, &e.ProjectID, &e.TaskA, &e.TaskB, &e.Kind, &e.Severity, &e.Weight,
		&e.Suggestion, &detail, &e.State, &e.ResolutionNote, &e.ResolvedBy,
		&e.DetectedAt, &e.UpdatedAt, &e.ResolvedAt); err != nil {
		return domain.ConflictEdge{}, err
	}
	if err := decodeJSON(detail, &e.Detail); err != nil {
		return domain.ConflictEdge{}, err
	}
	return e, nil
}

// UpsertConflict records or refreshes an edge in the merge-risk graph.
//
// Task ids are ordered canonically before writing, because "A conflicts with B" and "B
// conflicts with A" are one edge; without the ordering the graph would accumulate duplicate
// pairs and the conflict radar would double-count.
func (s *Store) UpsertConflict(ctx context.Context, e domain.ConflictEdge) (domain.ConflictEdge, error) {
	a, b := e.TaskA, e.TaskB
	detail := e.Detail
	if a > b {
		a, b = b, a
		detail.TaskARef, detail.TaskBRef = detail.TaskBRef, detail.TaskARef
		detail.TaskAOwner, detail.TaskBOwner = detail.TaskBOwner, detail.TaskAOwner
	}
	body, err := marshalJSON(detail)
	if err != nil {
		return domain.ConflictEdge{}, err
	}
	if e.State == "" {
		e.State = "open"
	}
	return scanConflict(s.pool.QueryRow(ctx, `
		INSERT INTO conflict_edges (project_id, task_a, task_b, kind, severity, weight,
		        suggestion, detail, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (project_id, task_a, task_b, kind) WHERE state IN ('open','acknowledged')
		DO UPDATE SET severity = EXCLUDED.severity,
		              weight = EXCLUDED.weight,
		              suggestion = EXCLUDED.suggestion,
		              detail = EXCLUDED.detail,
		              updated_at = now()
		RETURNING `+conflictColumns,
		e.ProjectID, a, b, e.Kind, e.Severity, e.Weight, e.Suggestion, body, e.State,
	).Scan)
}

// ListConflicts returns conflict edges for a project.
func (s *Store) ListConflicts(ctx context.Context, projectID domain.ID, openOnly bool) ([]domain.ConflictEdge, error) {
	q := `SELECT ` + conflictColumns + ` FROM conflict_edges WHERE project_id = $1::uuid`
	if openOnly {
		q += ` AND state IN ('open','acknowledged')`
	}
	q += ` ORDER BY CASE severity
	                  WHEN 'critical' THEN 4 WHEN 'high' THEN 3
	                  WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END DESC,
	                detected_at DESC`

	rows, err := s.pool.Query(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ConflictEdge
	for rows.Next() {
		e, err := scanConflict(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ConflictsForTask returns open edges touching a task, which is what blocks a claim when
// policy says conflicts are blocking.
func (s *Store) ConflictsForTask(ctx context.Context, taskID domain.ID) ([]domain.ConflictEdge, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+conflictColumns+` FROM conflict_edges
		  WHERE (task_a = $1::uuid OR task_b = $1::uuid)
		    AND state IN ('open','acknowledged')`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ConflictEdge
	for rows.Next() {
		e, err := scanConflict(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ResolveConflict records a human decision. `ignored` requires a note, because an unexplained
// dismissal is indistinguishable from a mistake when someone reads it back a week later.
func (s *Store) ResolveConflict(ctx context.Context, id domain.ID, state string, note string, by domain.ID) (domain.ConflictEdge, error) {
	switch state {
	case "acknowledged", "resolved", "ignored":
	default:
		return domain.ConflictEdge{}, domain.ErrInvalidEnum
	}
	if state == "ignored" && note == "" {
		return domain.ConflictEdge{}, domain.ErrInvalidArgument
	}
	e, err := scanConflict(s.pool.QueryRow(ctx, `
		UPDATE conflict_edges
		   SET state = $2, resolution_note = $3, resolved_by = $4::uuid,
		       updated_at = now(),
		       resolved_at = CASE WHEN $2 IN ('resolved','ignored') THEN now() ELSE NULL END
		 WHERE id = $1::uuid
		RETURNING `+conflictColumns,
		id, state, nullableText(note), nullable(by)).Scan)
	return e, noRows(err)
}

// CloseConflictsForTask resolves every open edge touching a finished task, so the radar does
// not keep showing collisions with work that has landed.
func (s *Store) CloseConflictsForTask(ctx context.Context, taskID domain.ID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE conflict_edges
		   SET state = 'resolved', resolved_at = now(), updated_at = now(),
		       resolution_note = COALESCE(resolution_note, 'task closed')
		 WHERE (task_a = $1::uuid OR task_b = $1::uuid)
		   AND state IN ('open','acknowledged')`, taskID)
	return err
}

// ---------------------------------------------------------------------------
// Merge-risk inputs
// ---------------------------------------------------------------------------

// TaskFootprint is the observed and declared territory of one open task, the raw material
// for the merge-risk graph (DESIGN.md §11.6).
type TaskFootprint struct {
	TaskID       domain.ID
	TaskRef      string
	Title        string
	Status       domain.TaskStatus
	Visibility   domain.Visibility
	OwnerID      domain.ID
	Owner        string
	Branch       string
	ChangedPaths []string
	Declared     []domain.ScopeReservation
	Fingerprint  string
	MinHash      []int64
}

// OpenFootprints loads every open task with its declared reservations and the paths its
// latest attempt actually touched.
//
// Declared scope tells you what someone said they would do; changed paths tell you what they
// are doing. The second is what actually predicts a merge conflict.
func (s *Store) OpenFootprints(ctx context.Context, projectID domain.ID) ([]TaskFootprint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text, t.ref, t.title, t.status, t.visibility, t.created_by::text,
		       pr.handle,
		       COALESCE(a.branch, ''), COALESCE(a.changed_paths, '{}'),
		       COALESCE(t.intent_fingerprint, ''), COALESCE(t.intent_minhash, '{}')
		  FROM tasks t
		  JOIN principals pr ON pr.id = t.created_by
		  LEFT JOIN LATERAL (
		       SELECT branch, changed_paths FROM attempts
		        WHERE task_id = t.id ORDER BY attempt_number DESC LIMIT 1
		  ) a ON true
		 WHERE t.project_id = $1::uuid
		   AND t.status NOT IN ('done','cancelled','superseded','failed')`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TaskFootprint
	byID := map[domain.ID]int{}
	for rows.Next() {
		var f TaskFootprint
		if err := rows.Scan(&f.TaskID, &f.TaskRef, &f.Title, &f.Status, &f.Visibility,
			&f.OwnerID, &f.Owner, &f.Branch, &f.ChangedPaths,
			&f.Fingerprint, &f.MinHash); err != nil {
			return nil, err
		}
		byID[f.TaskID] = len(out)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	resRows, err := s.pool.Query(ctx, `
		SELECT r.id::text, r.project_id::text, r.task_id::text,
		       COALESCE(r.attempt_id::text, ''), COALESCE(r.lease_id::text, ''),
		       r.principal_id::text, r.resource_type, r.resource_key, r.mode, r.source,
		       COALESCE(r.alternative_group, ''), r.active, r.created_at
		  FROM scope_reservations r
		  JOIN tasks t ON t.id = r.task_id
		 WHERE r.project_id = $1::uuid AND r.active = true
		   AND t.status NOT IN ('done','cancelled','superseded','failed')`, projectID)
	if err != nil {
		return nil, err
	}
	defer resRows.Close()
	reservations, err := scanReservations(resRows)
	if err != nil {
		return nil, err
	}
	for _, r := range reservations {
		if idx, ok := byID[r.TaskID]; ok {
			out[idx].Declared = append(out[idx].Declared, r)
		}
	}
	return out, nil
}

// ReplaceConflictsOfKind swaps the open edges of one kind for a freshly computed set,
// preserving any that a human has already acknowledged or ignored.
//
// Recomputing merge risk from scratch each pass is simpler and less error-prone than
// incrementally patching the graph, but it must not discard human decisions — an edge
// someone deliberately ignored should stay ignored rather than reappearing every tick.
func (s *Store) ReplaceConflictsOfKind(ctx context.Context, projectID domain.ID, kind domain.ConflictKind, edges []domain.ConflictEdge) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE conflict_edges SET state = 'resolved', resolved_at = now(), updated_at = now()
			 WHERE project_id = $1::uuid AND kind = $2 AND state = 'open'`,
			projectID, kind); err != nil {
			return err
		}
		for _, e := range edges {
			a, b := e.TaskA, e.TaskB
			detail := e.Detail
			if a > b {
				a, b = b, a
				detail.TaskARef, detail.TaskBRef = detail.TaskBRef, detail.TaskARef
				detail.TaskAOwner, detail.TaskBOwner = detail.TaskBOwner, detail.TaskAOwner
			}
			body, err := marshalJSON(detail)
			if err != nil {
				return err
			}
			// Skip pairs a human already dispositioned.
			var handled bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM conflict_edges
				                WHERE project_id = $1::uuid AND task_a = $2::uuid
				                  AND task_b = $3::uuid AND kind = $4
				                  AND state IN ('acknowledged','ignored'))`,
				projectID, a, b, kind).Scan(&handled); err != nil {
				return err
			}
			if handled {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO conflict_edges (project_id, task_a, task_b, kind, severity,
				        weight, suggestion, detail, state)
				VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, 'open')
				ON CONFLICT (project_id, task_a, task_b, kind) WHERE state IN ('open','acknowledged')
				DO UPDATE SET severity = EXCLUDED.severity, weight = EXCLUDED.weight,
				              suggestion = EXCLUDED.suggestion, detail = EXCLUDED.detail,
				              updated_at = now()`,
				projectID, a, b, kind, e.Severity, e.Weight, e.Suggestion, body); err != nil {
				return err
			}
		}
		return nil
	})
}
