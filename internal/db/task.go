package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
)

const taskColumns = `
	t.id::text, t.organization_id::text, t.project_id::text, t.ref,
	COALESCE(t.parent_id::text, ''), COALESCE(t.external_ref, ''),
	t.title, t.objective, t.acceptance_criteria, t.status, t.visibility,
	t.priority, t.risk_level, t.base_sha, t.workflow_sha,
	COALESCE(t.active_lease_id::text, ''), t.fencing_epoch,
	COALESCE(t.intent_fingerprint, ''), COALESCE(t.intent_minhash, '{}'),
	t.model_alias, t.harness_pref, t.budget, t.features,
	t.attempts_count, t.max_attempts, COALESCE(t.superseded_by::text, ''),
	t.created_by::text, t.created_at, t.updated_at, t.completed_at, t.labels,
	t.pull_request_url, t.pull_request_state`

func scanTask(scan func(...any) error) (domain.Task, error) {
	var t domain.Task
	var criteria, budget, features []byte
	if err := scan(&t.ID, &t.OrganizationID, &t.ProjectID, &t.Ref,
		&t.ParentID, &t.ExternalRef,
		&t.Title, &t.Objective, &criteria, &t.Status, &t.Visibility,
		&t.Priority, &t.RiskLevel, &t.BaseSHA, &t.WorkflowSHA,
		&t.ActiveLeaseID, &t.FencingEpoch,
		&t.Fingerprint, &t.MinHash,
		&t.ModelAlias, &t.HarnessPref, &budget, &features,
		&t.AttemptsCount, &t.MaxAttempts, &t.SupersededBy,
		&t.CreatedBy, &t.CreatedAt, &t.UpdatedAt, &t.CompletedAt, &t.Labels,
		&t.PullRequestURL, &t.PullRequestState); err != nil {
		return domain.Task{}, err
	}
	if err := decodeJSON(criteria, &t.AcceptanceCriteria); err != nil {
		return domain.Task{}, err
	}
	if err := decodeJSON(budget, &t.Budget); err != nil {
		return domain.Task{}, err
	}
	if err := decodeJSON(features, &t.Features); err != nil {
		return domain.Task{}, err
	}
	return t, nil
}

type CreateTaskParams struct {
	ProjectID          domain.ID
	CreatedBy          domain.ID
	ParentID           domain.ID
	ExternalRef        string
	Title              string
	Objective          string
	AcceptanceCriteria []domain.AcceptanceCriterion
	Status             domain.TaskStatus
	Visibility         domain.Visibility
	Priority           int
	RiskLevel          domain.RiskLevel
	ModelAlias         string
	HarnessPref        string
	Labels             []string
	MaxAttempts        int
	Budget             domain.Budget
	Features           domain.TaskFeatures
	DependsOn          []domain.ID
	Fingerprint        string
	MinHash            []int64
	BaseSHA            string
	WorkflowSHA        string
}

// CreateTask inserts a task and its dependency edges atomically.
//
// The human-facing ref (T-42) comes from a per-project counter bumped inside the same
// transaction, so refs are dense and monotonic even under concurrent creation.
func (s *Store) CreateTask(ctx context.Context, p CreateTaskParams) (domain.Task, error) {
	if p.Status == "" {
		p.Status = domain.TaskProposed
	}
	if p.Visibility == "" {
		p.Visibility = domain.VisibilityTeamSummary
	}
	if p.RiskLevel == "" {
		p.RiskLevel = domain.RiskUnknown
	}
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 4
	}
	if err := domain.Validate(p.Status, domain.AllTaskStatuses, "status"); err != nil {
		return domain.Task{}, err
	}
	if err := domain.Validate(p.Visibility, domain.AllVisibilities, "visibility"); err != nil {
		return domain.Task{}, err
	}

	criteria, err := marshalJSON(p.AcceptanceCriteria)
	if err != nil {
		return domain.Task{}, err
	}
	budget, err := marshalJSON(p.Budget)
	if err != nil {
		return domain.Task{}, err
	}
	features, err := marshalJSON(p.Features)
	if err != nil {
		return domain.Task{}, err
	}

	var out domain.Task
	err = s.Tx(ctx, func(tx pgx.Tx) error {
		var seq int64
		var orgID domain.ID
		if err := tx.QueryRow(ctx, `
			UPDATE projects SET task_seq = task_seq + 1
			 WHERE id = $1::uuid
			RETURNING task_seq, organization_id::text`, p.ProjectID).Scan(&seq, &orgID); err != nil {
			return noRows(err)
		}
		ref := "T-" + strconv.FormatInt(seq, 10)

		out, err = scanTask(tx.QueryRow(ctx, `
			INSERT INTO tasks (organization_id, project_id, ref, parent_id, external_ref,
			        title, objective, acceptance_criteria, status, visibility, priority,
			        risk_level, base_sha, workflow_sha, intent_fingerprint, intent_minhash,
			        model_alias, harness_pref, budget, features, max_attempts, created_by, labels)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5, $6, $7, $8, $9, $10, $11,
			        $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22::uuid, $23)
			RETURNING `+strings.ReplaceAll(taskColumns, "t.", "tasks."),
			orgID, p.ProjectID, ref, nullable(p.ParentID), nullableText(p.ExternalRef),
			p.Title, p.Objective, criteria, p.Status, p.Visibility, p.Priority,
			p.RiskLevel, p.BaseSHA, p.WorkflowSHA, nullableText(p.Fingerprint), p.MinHash,
			p.ModelAlias, p.HarnessPref, budget, features, p.MaxAttempts, p.CreatedBy,
			normalizeLabels(p.Labels),
		).Scan)
		if err != nil {
			return err
		}

		for _, dep := range p.DependsOn {
			if _, err := tx.Exec(ctx, `
				INSERT INTO task_dependencies (task_id, depends_on_id)
				VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`, out.ID, dep); err != nil {
				return err
			}
		}
		if len(p.DependsOn) > 0 {
			if err := assertNoCycle(ctx, tx, out.ID); err != nil {
				return err
			}
			out.DependsOn = p.DependsOn
		}
		return nil
	})
	return out, err
}

// assertNoCycle walks the dependency graph from taskID and fails if it reaches itself.
// DESIGN.md §14.3 requires plan validation to reject cycles; enforcing it at insert time
// means no code path can create one.
func assertNoCycle(ctx context.Context, tx pgx.Tx, taskID domain.ID) error {
	var cyclic bool
	err := tx.QueryRow(ctx, `
		WITH RECURSIVE reach(id) AS (
		    SELECT depends_on_id FROM task_dependencies WHERE task_id = $1::uuid
		  UNION
		    SELECT d.depends_on_id
		      FROM task_dependencies d JOIN reach r ON d.task_id = r.id
		)
		SELECT EXISTS (SELECT 1 FROM reach WHERE id = $1::uuid)`, taskID).Scan(&cyclic)
	if err != nil {
		return err
	}
	if cyclic {
		return fmt.Errorf("%w: task %s depends on itself", domain.ErrDependencyCycle, taskID)
	}
	return nil
}

func (s *Store) GetTask(ctx context.Context, id domain.ID) (domain.Task, error) {
	t, err := scanTask(s.pool.QueryRow(ctx,
		`SELECT `+taskColumns+` FROM tasks t WHERE t.id = $1::uuid`, id).Scan)
	if err != nil {
		return domain.Task{}, noRows(err)
	}
	t.DependsOn, err = s.DependenciesOf(ctx, t.ID)
	return t, err
}

func (s *Store) GetTaskByRef(ctx context.Context, projectID domain.ID, ref string) (domain.Task, error) {
	t, err := scanTask(s.pool.QueryRow(ctx,
		`SELECT `+taskColumns+` FROM tasks t
		  WHERE t.project_id = $1::uuid AND t.ref = $2`, projectID, ref).Scan)
	if err != nil {
		return domain.Task{}, noRows(err)
	}
	t.DependsOn, err = s.DependenciesOf(ctx, t.ID)
	return t, err
}

func (s *Store) DependenciesOf(ctx context.Context, taskID domain.ID) ([]domain.ID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT depends_on_id::text FROM task_dependencies WHERE task_id = $1::uuid`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) AddDependency(ctx context.Context, taskID, dependsOn domain.ID) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO task_dependencies (task_id, depends_on_id)
			VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`, taskID, dependsOn); err != nil {
			return err
		}
		return assertNoCycle(ctx, tx, taskID)
	})
}

// ListTasksFilter narrows a task listing. Zero values mean "no filter".
type ListTasksFilter struct {
	Statuses    []domain.TaskStatus
	OpenOnly    bool
	CreatedBy   domain.ID
	ExternalRef string
	// Labels keeps tasks carrying any of the given labels.
	Labels []string
	Limit  int
}

func (s *Store) ListTasks(ctx context.Context, projectID domain.ID, f ListTasksFilter) ([]domain.Task, error) {
	q := strings.Builder{}
	q.WriteString(`SELECT ` + taskColumns + ` FROM tasks t WHERE t.project_id = $1::uuid`)
	args := []any{projectID}

	if len(f.Statuses) > 0 {
		statuses := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			statuses[i] = string(st)
		}
		args = append(args, statuses)
		fmt.Fprintf(&q, " AND t.status = ANY($%d)", len(args))
	}
	if f.OpenOnly {
		q.WriteString(` AND t.status NOT IN ('done','cancelled','superseded','failed')`)
	}
	if f.CreatedBy != "" {
		args = append(args, f.CreatedBy)
		fmt.Fprintf(&q, " AND t.created_by = $%d::uuid", len(args))
	}
	if f.ExternalRef != "" {
		args = append(args, f.ExternalRef)
		fmt.Fprintf(&q, " AND t.external_ref = $%d", len(args))
	}
	if len(f.Labels) > 0 {
		args = append(args, normalizeLabels(f.Labels))
		fmt.Fprintf(&q, " AND t.labels && $%d", len(args))
	}
	q.WriteString(` ORDER BY t.priority DESC, t.created_at`)
	if f.Limit > 0 {
		args = append(args, f.Limit)
		fmt.Fprintf(&q, " LIMIT $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTaskStatus transitions a task, validating the edge against the state machine.
//
// The current status is read under a row lock so the guard and the write cannot straddle a
// concurrent transition — otherwise two callers could each validate against the old status
// and both apply.
//
// A transition into a terminal status also ends whatever the task still holds, in the same
// transaction: its live lease (and the attempt under it) and its scope reservations. That is
// what makes `conductor task done`, a dashboard cancel, and a merged pull request hand the
// territory back — a task in verifying keeps its reservations as a pending-merge hold (see
// Release), and something has to drop that hold when the work lands.
func (s *Store) UpdateTaskStatus(ctx context.Context, id domain.ID, to domain.TaskStatus) (domain.Task, error) {
	var out domain.Task
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = updateTaskStatusTx(ctx, tx, id, to, "")
		return err
	})
	return out, err
}

// updateTaskStatusTx is UpdateTaskStatus inside the caller's transaction. reason, when set,
// is recorded on the status event.
//
// via names intermediate statuses the task passes through on the way to `to`; every edge of
// the walk must be legal. Only the final status is written — the intermediate ones are how
// the state machine is honoured for a jump it has no single edge for (a merge completing a
// task that was still running), not states anyone needs to observe.
func updateTaskStatusTx(ctx context.Context, tx pgx.Tx, id domain.ID, to domain.TaskStatus, reason string, via ...domain.TaskStatus) (domain.Task, error) {
	var from domain.TaskStatus
	if err := tx.QueryRow(ctx,
		`SELECT status FROM tasks WHERE id = $1::uuid FOR UPDATE`, id).Scan(&from); err != nil {
		return domain.Task{}, noRows(err)
	}
	prev := from
	for _, next := range append(via, to) {
		if err := domain.AssertTaskTransition(prev, next); err != nil {
			return domain.Task{}, err
		}
		prev = next
	}
	terminal := to.IsTerminal() || to == domain.TaskFailed
	if terminal && from != to {
		if err := endLiveLeaseTx(ctx, tx, id, string(to)); err != nil {
			return domain.Task{}, err
		}
	}
	out, err := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks
		   SET status = $2,
		       updated_at = now(),
		       active_lease_id = CASE WHEN $3::boolean THEN NULL ELSE active_lease_id END,
		       completed_at = CASE WHEN $2 IN ('done','cancelled','superseded')
		                           THEN now() ELSE completed_at END
		 WHERE id = $1::uuid
		RETURNING `+strings.ReplaceAll(taskColumns, "t.", "tasks."), id, to, terminal).Scan)
	if err != nil {
		return domain.Task{}, err
	}
	if from == to {
		return out, nil
	}
	if terminal {
		if err := releaseTaskReservationsTx(ctx, tx, id); err != nil {
			return domain.Task{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE conflict_edges
			   SET state = 'resolved', resolved_at = now(), updated_at = now(),
			       resolution_note = COALESCE(resolution_note, 'task closed')
			 WHERE (task_a = $1::uuid OR task_b = $1::uuid)
			   AND state IN ('open','acknowledged')`, id); err != nil {
			return domain.Task{}, err
		}
	}
	return out, appendEvents(ctx, tx, out.OrganizationID, out.ProjectID, "",
		eventSpec{"task", out.ID, "task.status_changed", domain.VisibilityTeamSummary, map[string]any{
			"task_ref": out.Ref, "from": string(from), "to": string(to), "status": string(to),
			"reason": reason,
		}})
}

// endLiveLeaseTx releases a task's live lease, if it has one, and closes the attempt under
// it. A task forced into a terminal status (cancelled from the dashboard, completed by a
// merge) must not leave a lease behind that a worker could keep publishing under.
func endLiveLeaseTx(ctx context.Context, tx pgx.Tx, taskID domain.ID, reason string) error {
	var leaseID, attemptID domain.ID
	err := tx.QueryRow(ctx, `
		UPDATE leases SET released_at = now(), release_reason = $2
		 WHERE task_id = $1::uuid AND released_at IS NULL
		RETURNING id::text, attempt_id::text`, taskID, reason).Scan(&leaseID, &attemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var state domain.AttemptState
	if err := tx.QueryRow(ctx,
		`SELECT state FROM attempts WHERE id = $1::uuid FOR UPDATE`, attemptID).Scan(&state); err != nil {
		return noRows(err)
	}
	// A merge proves the work happened, so the attempt succeeded if it got far enough to be
	// able to; anything else just stops it.
	next := domain.AttemptCancelled
	if reason == string(domain.TaskDone) && domain.CanTransitionAttempt(state, domain.AttemptSucceeded) {
		next = domain.AttemptSucceeded
	}
	if domain.CanTransitionAttempt(state, next) && state != next {
		if _, err := tx.Exec(ctx, `
			UPDATE attempts SET state = $2, ended_at = now(), last_event_at = now()
			 WHERE id = $1::uuid`, attemptID, next); err != nil {
			return err
		}
	}
	// The epoch moves on, so a worker still holding the old fence is refused rather than
	// publishing into a task that has already closed.
	if _, err := tx.Exec(ctx, `
		UPDATE tasks SET fencing_epoch = fencing_epoch + 1 WHERE id = $1::uuid`, taskID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE sessions SET active_task_id = NULL, state = 'online_idle'
		 WHERE active_task_id = $1::uuid`, taskID)
	return err
}

// PatchTask applies caller-supplied field updates. Only fields explicitly present are
// touched; status changes go through UpdateTaskStatus so the state machine is never bypassed.
type TaskPatch struct {
	Title              *string
	Objective          *string
	Priority           *int
	RiskLevel          *domain.RiskLevel
	Visibility         *domain.Visibility
	ModelAlias         *string
	HarnessPref        *string
	MaxAttempts        *int
	AcceptanceCriteria *[]domain.AcceptanceCriterion
	Features           *domain.TaskFeatures
	BaseSHA            *string
	Labels             *[]string
}

func (s *Store) PatchTask(ctx context.Context, id domain.ID, p TaskPatch) (domain.Task, error) {
	sets := []string{"updated_at = now()"}
	args := []any{id}
	add := func(expr string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf(expr, len(args)))
	}

	if p.Title != nil {
		add("title = $%d", *p.Title)
	}
	if p.Objective != nil {
		add("objective = $%d", *p.Objective)
	}
	if p.Priority != nil {
		add("priority = $%d", *p.Priority)
	}
	if p.RiskLevel != nil {
		add("risk_level = $%d", *p.RiskLevel)
	}
	if p.Visibility != nil {
		if err := domain.Validate(*p.Visibility, domain.AllVisibilities, "visibility"); err != nil {
			return domain.Task{}, err
		}
		add("visibility = $%d", *p.Visibility)
	}
	if p.ModelAlias != nil {
		add("model_alias = $%d", *p.ModelAlias)
	}
	if p.HarnessPref != nil {
		add("harness_pref = $%d", *p.HarnessPref)
	}
	if p.MaxAttempts != nil {
		add("max_attempts = $%d", *p.MaxAttempts)
	}
	if p.Labels != nil {
		add("labels = $%d", normalizeLabels(*p.Labels))
	}
	if p.BaseSHA != nil {
		add("base_sha = $%d", *p.BaseSHA)
	}
	if p.AcceptanceCriteria != nil {
		body, err := marshalJSON(*p.AcceptanceCriteria)
		if err != nil {
			return domain.Task{}, err
		}
		add("acceptance_criteria = $%d", body)
	}
	if p.Features != nil {
		body, err := marshalJSON(*p.Features)
		if err != nil {
			return domain.Task{}, err
		}
		add("features = $%d", body)
	}

	t, err := scanTask(s.pool.QueryRow(ctx,
		`UPDATE tasks SET `+strings.Join(sets, ", ")+
			` WHERE id = $1::uuid RETURNING `+strings.ReplaceAll(taskColumns, "t.", "tasks."),
		args...).Scan)
	return t, noRows(err)
}

// SetTaskFingerprint stores the coordination metadata used for duplicate detection.
func (s *Store) SetTaskFingerprint(ctx context.Context, id domain.ID, fingerprint string, minhash []int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET intent_fingerprint = $2, intent_minhash = $3 WHERE id = $1::uuid`,
		id, nullableText(fingerprint), minhash)
	return err
}

// SupersedeTask marks a task as replaced by another, used when a duplicate is resolved by
// consolidating onto one task.
func (s *Store) SupersedeTask(ctx context.Context, id, by domain.ID) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		var from domain.TaskStatus
		if err := tx.QueryRow(ctx,
			`SELECT status FROM tasks WHERE id = $1::uuid FOR UPDATE`, id).Scan(&from); err != nil {
			return noRows(err)
		}
		if err := domain.AssertTaskTransition(from, domain.TaskSuperseded); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE tasks
			   SET status = 'superseded', superseded_by = $2::uuid,
			       completed_at = now(), updated_at = now()
			 WHERE id = $1::uuid`, id, by)
		return err
	})
}

// ---------------------------------------------------------------------------
// Private task detail
// ---------------------------------------------------------------------------

// PutPrivateDetail stores encrypted detail for a private task. The ciphertext is opaque to
// the control plane; only the owning principal's client can decrypt it.
func (s *Store) PutPrivateDetail(ctx context.Context, taskID, owner domain.ID, ciphertext, nonce []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO task_private_fields (task_id, owner_principal, detail_ciphertext, nonce)
		VALUES ($1::uuid, $2::uuid, $3, $4)
		ON CONFLICT (task_id) DO UPDATE SET
		    detail_ciphertext = EXCLUDED.detail_ciphertext, nonce = EXCLUDED.nonce`,
		taskID, owner, ciphertext, nonce)
	return err
}

// GetPrivateDetail returns the ciphertext only to its owner. A non-owner gets ErrNotFound,
// which is indistinguishable from "no private detail exists".
func (s *Store) GetPrivateDetail(ctx context.Context, taskID, requester domain.ID) (ciphertext, nonce []byte, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT detail_ciphertext, nonce FROM task_private_fields
		 WHERE task_id = $1::uuid AND owner_principal = $2::uuid`,
		taskID, requester).Scan(&ciphertext, &nonce)
	return ciphertext, nonce, noRows(err)
}

// ---------------------------------------------------------------------------
// Dependency readiness
// ---------------------------------------------------------------------------

// UnblockReadyTasks promotes blocked_dependency tasks whose dependencies are all done.
// Returns the ids that moved, so the caller can emit events for them.
func (s *Store) UnblockReadyTasks(ctx context.Context, projectID domain.ID) ([]domain.ID, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE tasks t
		   SET status = 'ready', updated_at = now()
		 WHERE t.project_id = $1::uuid
		   AND t.status = 'blocked_dependency'
		   AND NOT EXISTS (
		         SELECT 1 FROM task_dependencies d
		           JOIN tasks dep ON dep.id = d.depends_on_id
		          WHERE d.task_id = t.id AND dep.status <> 'done')
		RETURNING t.id::text`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// BlockTasksWithUnmetDependencies is the inverse: a ready task whose dependency regressed
// must not be dispatched.
func (s *Store) BlockTasksWithUnmetDependencies(ctx context.Context, projectID domain.ID) ([]domain.ID, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE tasks t
		   SET status = 'blocked_dependency', updated_at = now()
		 WHERE t.project_id = $1::uuid
		   AND t.status = 'ready'
		   AND EXISTS (
		         SELECT 1 FROM task_dependencies d
		           JOIN tasks dep ON dep.id = d.depends_on_id
		          WHERE d.task_id = t.id AND dep.status <> 'done')
		RETURNING t.id::text`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// normalizeLabels lower-cases, trims, de-duplicates, and sorts labels so "Docs" and "docs"
// are one label and a filter can compare them exactly. The result is never nil, because a
// text[] column does not accept NULL.
func normalizeLabels(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, l := range in {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" || seen[l] || len(l) > 64 {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}
