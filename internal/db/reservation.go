package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/resource"
)

// ScopeConflict describes one blocked or warned-about overlap, with enough context for a
// human or agent to choose join / wait / split.
//
// It names the holder and the contested resource, and nothing else. When the holding task is
// private, the API layer suppresses its title before this reaches the caller; the resource
// and owner are always disclosed because that is what prevents the collision.
type ScopeConflict struct {
	Requested     string                 `json:"requested"`
	ResourceKey   string                 `json:"resource"`
	Outcome       domain.Outcome         `json:"outcome"`
	Severity      domain.Severity        `json:"severity"`
	Kind          domain.ConflictKind    `json:"kind"`
	HolderTaskID  domain.ID              `json:"holder_task_id"`
	HolderTaskRef string                 `json:"holder_task_ref"`
	HolderTitle   string                 `json:"holder_task_title,omitempty"`
	HolderOwner   string                 `json:"holder_owner"`
	HolderMode    domain.ReservationMode `json:"holder_mode"`
	HeldSince     time.Time              `json:"held_since"`
	// HolderStatus is the holding task's status. A holder in verifying, review_required, or
	// merging has finished its work and is waiting for it to merge: the territory is held
	// because its new contents sit in an unmerged branch, not because someone is editing.
	HolderStatus domain.TaskStatus `json:"holder_status,omitempty"`
	// HolderPullRequest is the holder's pull request, when one is known, so a blocked caller
	// can go and look at (or review) the change they are waiting for.
	HolderPullRequest string `json:"holder_pull_request,omitempty"`
}

// PendingMerge reports whether the conflict is with finished work waiting to merge.
func (c ScopeConflict) PendingMerge() bool { return PendingMerge(c.HolderStatus) }

// Blocking reports whether any conflict in the set forbids proceeding.
func Blocking(conflicts []ScopeConflict) bool {
	for _, c := range conflicts {
		if c.Outcome.Blocks() {
			return true
		}
	}
	return false
}

// activeHolder is one row of the active reservation set, joined with the context needed to
// explain a conflict.
type activeHolder struct {
	res        domain.ScopeReservation
	parsed     resource.Resource
	taskRef    string
	taskTitle  string
	taskStatus domain.TaskStatus
	pullURL    string
	visibility domain.Visibility
	owner      string
	ownerID    domain.ID
}

// loadActiveHolders reads every active reservation in a project except those held by
// excludeTask.
//
// The comparison happens in Go rather than SQL because overlap is not an equality test:
// directories contain paths, globs cross separators, and migrations serialize against
// everything. Encoding that in SQL would be both unreadable and untestable, and the active
// set for one project is small.
func loadActiveHolders(ctx context.Context, tx pgx.Tx, projectID, excludeTask domain.ID) ([]activeHolder, error) {
	rows, err := tx.Query(ctx, `
		SELECT r.id::text, r.project_id::text, r.task_id::text,
		       COALESCE(r.attempt_id::text, ''), COALESCE(r.lease_id::text, ''),
		       r.principal_id::text, r.resource_type, r.resource_key, r.mode, r.source,
		       COALESCE(r.alternative_group, ''), r.created_at,
		       t.ref, t.title, t.visibility, t.created_by::text, p.handle,
		       t.status, t.pull_request_url
		  FROM scope_reservations r
		  JOIN tasks t ON t.id = r.task_id
		  JOIN principals p ON p.id = r.principal_id
		 WHERE r.project_id = $1::uuid
		   AND r.active = true
		   AND ($2::uuid IS NULL OR r.task_id <> $2::uuid)
		   AND t.status NOT IN ('done','cancelled','superseded','failed')`,
		projectID, nullable(excludeTask))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []activeHolder
	for rows.Next() {
		var h activeHolder
		if err := rows.Scan(&h.res.ID, &h.res.ProjectID, &h.res.TaskID,
			&h.res.AttemptID, &h.res.LeaseID,
			&h.res.PrincipalID, &h.res.ResourceType, &h.res.ResourceKey, &h.res.Mode,
			&h.res.Source, &h.res.AlternativeGroup, &h.res.CreatedAt,
			&h.taskRef, &h.taskTitle, &h.visibility, &h.ownerID, &h.owner,
			&h.taskStatus, &h.pullURL); err != nil {
			return nil, err
		}
		h.res.Active = true
		parsed, err := resource.New(h.res.ResourceType, h.res.ResourceKey)
		if err != nil {
			// A stored reservation that no longer parses must not silently stop
			// conflicting; treat it as repo-wide so it still blocks.
			parsed = resource.Resource{Type: domain.ResourceRepo, Key: ""}
		}
		h.parsed = parsed
		out = append(out, h)
	}
	return out, rows.Err()
}

// evaluateScopes applies the §11.3 matrix to every requested resource against every active
// holder, returning all conflicts found (not just the first, so a caller can see the whole
// picture before deciding).
func evaluateScopes(
	requests []domain.ScopeRequest,
	holders []activeHolder,
	policy resource.Policy,
) ([]ScopeConflict, []resource.Resource, error) {
	parsed := make([]resource.Resource, 0, len(requests))
	var conflicts []ScopeConflict

	for _, req := range requests {
		want, err := resource.Parse(req.Resource)
		if err != nil {
			return nil, nil, err
		}
		parsed = append(parsed, want)

		mode := req.Mode
		if mode == "" {
			mode = domain.ModeWriteExclusive
		}
		if err := domain.Validate(mode, domain.AllReservationModes, "mode"); err != nil {
			return nil, nil, err
		}

		for _, h := range holders {
			if !resource.Overlaps(want, h.parsed) {
				continue
			}
			outcome := resource.Decide(
				resource.Holder{Mode: h.res.Mode, AlternativeGroup: h.res.AlternativeGroup},
				resource.Holder{Mode: mode, AlternativeGroup: req.AlternativeGroup},
				policy,
			)
			if outcome == domain.OutcomeAllow {
				continue
			}
			conflicts = append(conflicts, ScopeConflict{
				Requested:         want.String(),
				ResourceKey:       h.parsed.String(),
				Outcome:           outcome,
				Severity:          resource.SeverityFor(outcome, h.res.Mode, mode),
				Kind:              resource.ConflictKindFor(h.res.Mode, mode),
				HolderTaskID:      h.res.TaskID,
				HolderTaskRef:     h.taskRef,
				HolderTitle:       h.taskTitle,
				HolderOwner:       h.owner,
				HolderMode:        h.res.Mode,
				HeldSince:         h.res.CreatedAt,
				HolderStatus:      h.taskStatus,
				HolderPullRequest: h.pullURL,
			})
		}
	}
	return conflicts, parsed, nil
}

// redactHolderTitles blanks the title of any conflict whose holding task is private and not
// owned by the viewer. Territory and owner stay visible; intent does not.
func redactHolderTitles(conflicts []ScopeConflict, holders []activeHolder, viewer domain.ID) {
	private := map[domain.ID]bool{}
	for _, h := range holders {
		if h.visibility == domain.VisibilityPrivate && h.ownerID != viewer {
			private[h.res.TaskID] = true
		}
	}
	for i := range conflicts {
		if private[conflicts[i].HolderTaskID] {
			// The pull request would name the work as surely as the title does.
			conflicts[i].HolderTitle = ""
			conflicts[i].HolderPullRequest = ""
		}
	}
}

// CheckScopesParams asks "what happens if I reserve these?" without reserving anything.
type CheckScopesParams struct {
	ProjectID   domain.ID
	ExcludeTask domain.ID
	Viewer      domain.ID
	Requests    []domain.ScopeRequest
	Policy      resource.Policy
}

// CheckScopes is the read-only conflict probe behind `conductor check` and
// `coord_expand_scope`'s dry run. It is the single most valuable call in the system: an
// agent that runs it before editing never causes a collision.
func (s *Store) CheckScopes(ctx context.Context, p CheckScopesParams) ([]ScopeConflict, error) {
	var conflicts []ScopeConflict
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		holders, err := loadActiveHolders(ctx, tx, p.ProjectID, p.ExcludeTask)
		if err != nil {
			return err
		}
		conflicts, _, err = evaluateScopes(p.Requests, holders, p.Policy)
		if err != nil {
			return err
		}
		redactHolderTitles(conflicts, holders, p.Viewer)
		return nil
	})
	return conflicts, err
}

// AcquireScopesParams reserves territory for a task.
type AcquireScopesParams struct {
	ProjectID   domain.ID
	TaskID      domain.ID
	AttemptID   domain.ID
	LeaseID     domain.ID
	PrincipalID domain.ID
	Requests    []domain.ScopeRequest
	Source      domain.ReservationSource
	Policy      resource.Policy
	// AllowWarnings proceeds when the only conflicts are advisory. The default is to
	// proceed on warnings and stop on blocks; set this false to require a clean result.
	AllowWarnings bool
}

// AcquireScopes reserves resources, refusing on a hard conflict.
//
// The project advisory lock is what makes this correct. Checking for overlap and then
// inserting is a classic check-then-act race: row locks cannot protect a row that does not
// exist yet, so without serializing on the project, two agents can each see a clear field
// and both plant a flag.
func (s *Store) AcquireScopes(ctx context.Context, p AcquireScopesParams) ([]domain.ScopeReservation, []ScopeConflict, error) {
	if p.Source == "" {
		p.Source = domain.SourceDeclared
	}
	var granted []domain.ScopeReservation
	var conflicts []ScopeConflict

	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockProject(ctx, tx, p.ProjectID); err != nil {
			return err
		}
		holders, err := loadActiveHolders(ctx, tx, p.ProjectID, p.TaskID)
		if err != nil {
			return err
		}
		var parsed []resource.Resource
		conflicts, parsed, err = evaluateScopes(p.Requests, holders, p.Policy)
		if err != nil {
			return err
		}
		redactHolderTitles(conflicts, holders, p.PrincipalID)

		if Blocking(conflicts) {
			return fmt.Errorf("%w: %d blocking overlap(s)", domain.ErrConflict, len(conflicts))
		}
		if !p.AllowWarnings && len(conflicts) > 0 {
			return fmt.Errorf("%w: %d advisory overlap(s)", domain.ErrConflict, len(conflicts))
		}

		for i, req := range p.Requests {
			mode := req.Mode
			if mode == "" {
				mode = domain.ModeWriteExclusive
			}
			var r domain.ScopeReservation
			err := tx.QueryRow(ctx, `
				INSERT INTO scope_reservations (project_id, task_id, attempt_id, lease_id,
				        principal_id, resource_type, resource_key, mode, source, alternative_group)
				VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::uuid, $6, $7, $8, $9, $10)
				RETURNING id::text, project_id::text, task_id::text,
				          COALESCE(attempt_id::text, ''), COALESCE(lease_id::text, ''),
				          principal_id::text, resource_type, resource_key, mode, source,
				          COALESCE(alternative_group, ''), active, created_at`,
				p.ProjectID, p.TaskID, nullable(p.AttemptID), nullable(p.LeaseID),
				p.PrincipalID, parsed[i].Type, parsed[i].Key, mode, p.Source,
				nullableText(req.AlternativeGroup),
			).Scan(&r.ID, &r.ProjectID, &r.TaskID, &r.AttemptID, &r.LeaseID,
				&r.PrincipalID, &r.ResourceType, &r.ResourceKey, &r.Mode, &r.Source,
				&r.AlternativeGroup, &r.Active, &r.CreatedAt)
			if err != nil {
				return err
			}
			granted = append(granted, r)
		}
		return nil
	})
	return granted, conflicts, err
}

// ListReservations returns the active reservations for a project, for the conflict radar.
func (s *Store) ListReservations(ctx context.Context, projectID domain.ID) ([]domain.ScopeReservation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id::text, r.project_id::text, r.task_id::text,
		       COALESCE(r.attempt_id::text, ''), COALESCE(r.lease_id::text, ''),
		       r.principal_id::text, r.resource_type, r.resource_key, r.mode, r.source,
		       COALESCE(r.alternative_group, ''), r.active, r.created_at
		  FROM scope_reservations r
		  JOIN tasks t ON t.id = r.task_id
		 WHERE r.project_id = $1::uuid AND r.active = true
		   AND t.status NOT IN ('done','cancelled','superseded','failed')
		 ORDER BY r.created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReservations(rows)
}

// ReservationsForTask lists a task's active reservations.
func (s *Store) ReservationsForTask(ctx context.Context, taskID domain.ID) ([]domain.ScopeReservation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, project_id::text, task_id::text,
		       COALESCE(attempt_id::text, ''), COALESCE(lease_id::text, ''),
		       principal_id::text, resource_type, resource_key, mode, source,
		       COALESCE(alternative_group, ''), active, created_at
		  FROM scope_reservations
		 WHERE task_id = $1::uuid AND active = true
		 ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReservations(rows)
}

func scanReservations(rows pgx.Rows) ([]domain.ScopeReservation, error) {
	var out []domain.ScopeReservation
	for rows.Next() {
		var r domain.ScopeReservation
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.TaskID, &r.AttemptID, &r.LeaseID,
			&r.PrincipalID, &r.ResourceType, &r.ResourceKey, &r.Mode, &r.Source,
			&r.AlternativeGroup, &r.Active, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReleaseReservation drops a single reservation.
func (s *Store) ReleaseReservation(ctx context.Context, id domain.ID) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		released, err := releaseReservationsTx(ctx, tx,
			`UPDATE scope_reservations SET active = false, released_at = now()
			  WHERE id = $1::uuid AND active = true
			 RETURNING task_id::text, resource_type, resource_key`, id)
		if err != nil {
			return err
		}
		if len(released) == 0 {
			return domain.ErrNotFound
		}
		return notifyWaitersTx(ctx, tx, released)
	})
}

// ReleaseTaskReservations drops every reservation a task holds. Called on release, handoff,
// completion, and lease reclamation.
func (s *Store) ReleaseTaskReservations(ctx context.Context, taskID domain.ID) (int64, error) {
	var n int64
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		released, err := releaseReservationsTx(ctx, tx, releaseTaskReservationsSQL, taskID)
		if err != nil {
			return err
		}
		n = int64(len(released))
		return notifyWaitersTx(ctx, tx, released)
	})
	return n, err
}

const releaseTaskReservationsSQL = `
	UPDATE scope_reservations SET active = false, released_at = now()
	 WHERE task_id = $1::uuid AND active = true
	RETURNING task_id::text, resource_type, resource_key`

// releaseTaskReservationsTx is the in-transaction form used by claim/release paths. Whoever
// was blocked on the freed territory is told in the same transaction (notifyWaitersTx).
func releaseTaskReservationsTx(ctx context.Context, tx pgx.Tx, taskID domain.ID) error {
	released, err := releaseReservationsTx(ctx, tx, releaseTaskReservationsSQL, taskID)
	if err != nil {
		return err
	}
	return notifyWaitersTx(ctx, tx, released)
}

// releasedScope is one reservation that was just dropped.
type releasedScope struct {
	taskID   domain.ID
	resource resource.Resource
}

func releaseReservationsTx(ctx context.Context, tx pgx.Tx, query string, arg any) ([]releasedScope, error) {
	rows, err := tx.Query(ctx, query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []releasedScope
	for rows.Next() {
		var r releasedScope
		var typ domain.ResourceType
		var key string
		if err := rows.Scan(&r.taskID, &typ, &key); err != nil {
			return nil, err
		}
		parsed, err := resource.New(typ, key)
		if err != nil {
			// Same rule as loadActiveHolders: an unparseable reservation was treated as
			// repo-wide while held, so its release frees the whole repository.
			parsed = resource.Resource{Type: domain.ResourceRepo, Key: ""}
		}
		r.resource = parsed
		out = append(out, r)
	}
	return out, rows.Err()
}

// waiterWindow bounds how far back a blocked request still counts as someone waiting. It
// matches how long a recorded intent lives (StartWork records them for 15 minutes).
const waiterWindow = "15 minutes"

// notifyWaitersTx tells whoever was blocked on freshly released territory that it is free.
//
// "Wait for it" was the advice every blocked caller got, with nothing to wait on: no signal
// ever said the holder had let go. A blocked check, start-work, or scope expansion leaves a
// short-lived intent with outcome block_conflict and the scopes it wanted; when a release
// overlaps those scopes, this appends one `scope.released` event per waiting party, naming
// them, so the dashboard (and any notifier reading the event stream) can tell them to try
// again. The event carries the freed resources and the holder's ref — territory and owner,
// which a conflict already disclosed — and nothing about either side's intent.
//
// It is a nudge, not a reservation: someone else may still claim the territory first, and the
// waiter's own re-check is what decides.
func notifyWaitersTx(ctx context.Context, tx pgx.Tx, released []releasedScope) error {
	if len(released) == 0 {
		return nil
	}
	byTask := map[domain.ID][]resource.Resource{}
	var order []domain.ID
	for _, r := range released {
		if _, ok := byTask[r.taskID]; !ok {
			order = append(order, r.taskID)
		}
		byTask[r.taskID] = append(byTask[r.taskID], r.resource)
	}
	for _, taskID := range order {
		if err := notifyWaitersForTaskTx(ctx, tx, taskID, byTask[taskID]); err != nil {
			return err
		}
	}
	return nil
}

func notifyWaitersForTaskTx(ctx context.Context, tx pgx.Tx, taskID domain.ID, freed []resource.Resource) error {
	var orgID, projectID domain.ID
	var holderRef string
	if err := tx.QueryRow(ctx, `
		SELECT organization_id::text, project_id::text, ref FROM tasks WHERE id = $1::uuid`,
		taskID).Scan(&orgID, &projectID, &holderRef); err != nil {
		return noRows(err)
	}
	rows, err := tx.Query(ctx, `
		SELECT i.principal_id::text, p.handle, COALESCE(i.session_id::text, ''),
		       COALESCE(i.task_id::text, ''), COALESCE(t.ref, ''), i.scopes
		  FROM intents i
		  JOIN principals p ON p.id = i.principal_id
		  LEFT JOIN tasks t ON t.id = i.task_id
		 WHERE i.project_id = $1::uuid
		   AND i.outcome = $2
		   AND i.expires_at > now()
		   AND i.created_at > now() - $4::interval
		   AND (i.task_id IS NULL OR i.task_id <> $3::uuid)
		 ORDER BY i.created_at`, projectID, string(domain.OutcomeBlockConflict), taskID, waiterWindow)
	if err != nil {
		return err
	}
	type waiter struct {
		principalID, handle, sessionID, taskRef string
		resources                               []string
	}
	var waiters []*waiter
	index := map[string]*waiter{}
	for rows.Next() {
		var principalID, handle, sessionID, waitingTask, waitingRef string
		var raw []byte
		if err := rows.Scan(&principalID, &handle, &sessionID, &waitingTask, &waitingRef, &raw); err != nil {
			rows.Close()
			return err
		}
		var wanted []domain.ScopeRequest
		if err := decodeJSON(raw, &wanted); err != nil {
			continue
		}
		var hits []string
		for _, w := range wanted {
			want, err := resource.Parse(w.Resource)
			if err != nil {
				continue
			}
			for _, f := range freed {
				if resource.Overlaps(want, f) {
					hits = append(hits, f.String())
				}
			}
		}
		if len(hits) == 0 {
			continue
		}
		// One notice per waiting party per release, however many times they were refused.
		key := principalID + "\x00" + waitingRef
		w := index[key]
		if w == nil {
			w = &waiter{principalID: principalID, handle: handle, sessionID: sessionID, taskRef: waitingRef}
			index[key] = w
			waiters = append(waiters, w)
		}
		if w.sessionID == "" {
			w.sessionID = sessionID
		}
		w.resources = appendUnique(w.resources, hits...)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, w := range waiters {
		payload := map[string]any{
			"task_ref": holderRef, "resources": w.resources,
			"principal": w.handle, "principal_id": w.principalID,
			"reason": "territory you were blocked on was released; check again",
		}
		if w.sessionID != "" {
			payload["session_id"] = w.sessionID
		}
		if w.taskRef != "" {
			payload["waiting_task_ref"] = w.taskRef
		}
		if err := appendEvents(ctx, tx, orgID, projectID, "",
			eventSpec{"task", taskID, "scope.released", domain.VisibilityTeamSummary, payload}); err != nil {
			return err
		}
	}
	return nil
}

func appendUnique(dst []string, values ...string) []string {
	for _, v := range values {
		found := false
		for _, d := range dst {
			if d == v {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, v)
		}
	}
	return dst
}

// ProtectedScopesInFlight counts active protected_exclusive reservations, which policy caps
// at one writer project-wide (DESIGN.md §13.5, migration serialization).
func (s *Store) ProtectedScopesInFlight(ctx context.Context, projectID domain.ID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM scope_reservations r
		  JOIN tasks t ON t.id = r.task_id
		 WHERE r.project_id = $1::uuid AND r.active = true
		   AND r.mode = 'protected_exclusive'
		   AND t.status NOT IN ('done','cancelled','superseded','failed')`, projectID).Scan(&n)
	return n, err
}
