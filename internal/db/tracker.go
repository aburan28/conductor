package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
)

// Issue tracker sync storage (migration 0011). The rules — what an issue maps to, who wins an
// edit, when a close cancels a task — live in internal/tracker; this file holds the rows and
// applies a decision inside the transaction it was made in, so two deliveries of the same
// issue can never both create a task or both act on a stale read.

// Tracker names, as stored.
const TrackerGitHub = "github"

// Remote states of a synced issue.
const (
	TrackerRemoteOpen   = "open"
	TrackerRemoteClosed = "closed"
)

// TrackerConfig is one project's opt-in to syncing a tracker's issues.
type TrackerConfig struct {
	ProjectID domain.ID `json:"project_id"`
	Tracker   string    `json:"tracker"`
	Enabled   bool      `json:"enabled"`
	// Label limits the import to issues carrying it; empty imports every open issue.
	Label string `json:"label"`
	// InProgressLabel is put on an issue while its task is worked; empty writes none.
	InProgressLabel  string            `json:"in_progress_label"`
	PublicVisibility domain.Visibility `json:"public_visibility"`
	EnabledBy        domain.ID         `json:"-"`
	InstallationID   int64             `json:"-"`
	Cursor           *time.Time        `json:"cursor,omitempty"`
	ETag             string            `json:"-"`
	LastSyncAt       *time.Time        `json:"last_sync_at,omitempty"`
	// LastError is why the last import failed; WriteBackError why the write-back is failing.
	LastError      string `json:"last_error,omitempty"`
	WriteBackError string `json:"writeback_error,omitempty"`
}

const trackerConfigColumns = `
	project_id::text, tracker, enabled, label, in_progress_label, public_visibility,
	enabled_by::text, installation_id, cursor, etag, last_sync_at, last_error, writeback_error`

func scanTrackerConfig(scan func(...any) error) (TrackerConfig, error) {
	var c TrackerConfig
	err := scan(&c.ProjectID, &c.Tracker, &c.Enabled, &c.Label, &c.InProgressLabel, &c.PublicVisibility,
		&c.EnabledBy, &c.InstallationID, &c.Cursor, &c.ETag, &c.LastSyncAt, &c.LastError, &c.WriteBackError)
	return c, err
}

// TrackerConfigFor returns a project's configuration for a tracker; found is false when the
// project never enabled it.
func (s *Store) TrackerConfigFor(ctx context.Context, projectID domain.ID, tracker string) (TrackerConfig, bool, error) {
	c, err := scanTrackerConfig(s.pool.QueryRow(ctx, `SELECT `+trackerConfigColumns+`
		  FROM tracker_configs WHERE project_id = $1::uuid AND tracker = $2`, projectID, tracker).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return TrackerConfig{}, false, nil
	}
	return c, err == nil, err
}

// SaveTrackerConfig creates or replaces a project's settings for a tracker. Changing which
// label qualifies starts the import from the beginning: an issue that newly qualifies may not
// have changed since the old resume point, and would never be listed again. (Re-reading is
// harmless, because importing an issue twice changes nothing.) Enabling the sync again keeps
// the resume point, so issues that closed or changed while it was off are still seen.
func (s *Store) SaveTrackerConfig(ctx context.Context, c TrackerConfig) (TrackerConfig, error) {
	if err := domain.Validate(c.PublicVisibility, domain.AllVisibilities, "public_visibility"); err != nil {
		return TrackerConfig{}, err
	}
	return scanTrackerConfig(s.pool.QueryRow(ctx, `
		INSERT INTO tracker_configs (project_id, tracker, enabled, label, in_progress_label,
		        public_visibility, enabled_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::uuid)
		ON CONFLICT (project_id, tracker) DO UPDATE SET
		    enabled = EXCLUDED.enabled, label = EXCLUDED.label,
		    in_progress_label = EXCLUDED.in_progress_label,
		    public_visibility = EXCLUDED.public_visibility,
		    enabled_by = CASE WHEN EXCLUDED.enabled AND NOT tracker_configs.enabled
		                      THEN EXCLUDED.enabled_by ELSE tracker_configs.enabled_by END,
		    cursor = CASE WHEN EXCLUDED.label <> tracker_configs.label
		                  THEN NULL ELSE tracker_configs.cursor END,
		    etag = CASE WHEN EXCLUDED.label <> tracker_configs.label
		                THEN '' ELSE tracker_configs.etag END,
		    last_error = CASE WHEN EXCLUDED.enabled THEN tracker_configs.last_error ELSE '' END,
		    writeback_error = CASE WHEN EXCLUDED.enabled THEN tracker_configs.writeback_error ELSE '' END,
		    updated_at = now()
		RETURNING `+trackerConfigColumns,
		c.ProjectID, c.Tracker, c.Enabled, c.Label, c.InProgressLabel, c.PublicVisibility, c.EnabledBy,
	).Scan)
}

// TrackerPoll is what one import pass over a project's tracker learned.
type TrackerPoll struct {
	InstallationID int64
	// Cursor, when set, is the newest issue update the pass read; the next pass resumes there.
	Cursor *time.Time
	ETag   string
	// Err is why the pass failed; empty records a success.
	Err string
}

// RecordTrackerPoll stores an import pass's resume point and outcome.
func (s *Store) RecordTrackerPoll(ctx context.Context, projectID domain.ID, tracker string, p TrackerPoll) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tracker_configs
		   SET installation_id = CASE WHEN $3 > 0 THEN $3 ELSE installation_id END,
		       cursor = CASE WHEN $6 = '' THEN COALESCE($4, cursor) ELSE cursor END,
		       etag = CASE WHEN $6 = '' THEN $5 ELSE etag END,
		       last_sync_at = CASE WHEN $6 = '' THEN now() ELSE last_sync_at END,
		       last_error = $6
		 WHERE project_id = $1::uuid AND tracker = $2`,
		projectID, tracker, p.InstallationID, p.Cursor, p.ETag, p.Err)
	return err
}

// RecordTrackerWriteBackError notes why a project's write-back is failing; an empty message
// clears it. An unchanged message writes nothing.
func (s *Store) RecordTrackerWriteBackError(ctx context.Context, projectID domain.ID, tracker, msg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tracker_configs SET writeback_error = $3
		 WHERE project_id = $1::uuid AND tracker = $2 AND writeback_error <> $3`, projectID, tracker, msg)
	return err
}

// TrackerCounts summarises a project's synced issues: how many tasks were imported and how
// many of those are still open.
func (s *Store) TrackerCounts(ctx context.Context, projectID domain.ID, tracker string) (total, open int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE t.status NOT IN ('done','cancelled','superseded','failed'))
		  FROM tracker_links l JOIN tasks t ON t.id = l.task_id
		 WHERE l.project_id = $1::uuid AND l.tracker = $2`, projectID, tracker).Scan(&total, &open)
	return total, open, err
}

// TrackerLink is the row tying an issue to its task.
type TrackerLink struct {
	ProjectID       domain.ID
	ExternalRef     string
	Tracker         string
	TaskID          domain.ID
	URL             string
	RemoteState     string
	RemotePublic    bool
	RemoteUpdatedAt *time.Time
	SyncedTitleHash string
	SyncedBodyHash  string
	TaskEditedAt    *time.Time
	CancelledBySync bool
	ClaimNotedFor   domain.ID
	LabelApplied    string
	DoneNoted       bool
	WrittenAt       *time.Time
}

const trackerLinkColumns = `
	l.project_id::text, l.external_ref, l.tracker, l.task_id::text, l.url, l.remote_state,
	l.remote_public, l.remote_updated_at, l.synced_title_hash, l.synced_body_hash,
	l.task_edited_at, l.cancelled_by_sync, COALESCE(l.claim_noted_for::text, ''),
	l.label_applied, l.done_noted, l.written_at`

func trackerLinkDest(l *TrackerLink) []any {
	return []any{&l.ProjectID, &l.ExternalRef, &l.Tracker, &l.TaskID, &l.URL, &l.RemoteState,
		&l.RemotePublic, &l.RemoteUpdatedAt, &l.SyncedTitleHash, &l.SyncedBodyHash,
		&l.TaskEditedAt, &l.CancelledBySync, &l.ClaimNotedFor,
		&l.LabelApplied, &l.DoneNoted, &l.WrittenAt}
}

// TrackerItem is everything a sync decision reads, as it stands inside the transaction that
// will apply the decision.
type TrackerItem struct {
	// Linked is true when the issue was imported before; Link and Task are then its row and
	// its task.
	Linked bool
	Link   TrackerLink
	Task   domain.Task
	// Existing is an open task that already names the issue in its external_ref without
	// being linked to it (filed by hand before the sync was enabled), when there is one.
	Existing *domain.Task
}

// TrackerStatusChange is a status move a sync decision asks for.
type TrackerStatusChange string

const (
	TrackerKeepStatus TrackerStatusChange = ""
	// TrackerCancel cancels the task because its issue closed, and remembers that the sync
	// did it.
	TrackerCancel TrackerStatusChange = "cancel"
	// TrackerRevive returns a task the sync cancelled to ready, because its issue reopened.
	TrackerRevive TrackerStatusChange = "revive"
)

// TrackerChange is a sync decision. The zero value changes nothing.
type TrackerChange struct {
	// Create imports the issue as a new task; Adopt links Existing instead. Both apply only
	// when the issue is not linked yet.
	Create *CreateTaskParams
	Adopt  bool
	// Title, Objective, and Criteria replace the task's fields.
	Title     *string
	Objective *string
	Criteria  *[]domain.AcceptanceCriterion
	Status    TrackerStatusChange
	Reason    string
	// Remote is what the issue looked like; nil leaves the link's view of it as it was.
	Remote *TrackerRemote
}

// TrackerRemote is the link's memory of the issue: where it is, whether it is open and
// public, when it last changed, and hashes of the title and body last read.
type TrackerRemote struct {
	URL       string
	State     string
	Public    bool
	UpdatedAt time.Time
	TitleHash string
	BodyHash  string
}

// TrackerResult reports what applying a decision did.
type TrackerResult struct {
	TaskID  domain.ID         `json:"task_id,omitempty"`
	TaskRef string            `json:"task_ref,omitempty"`
	Created bool              `json:"created,omitempty"`
	Adopted bool              `json:"adopted,omitempty"`
	Updated []string          `json:"updated,omitempty"`
	From    domain.TaskStatus `json:"from,omitempty"`
	Status  domain.TaskStatus `json:"status,omitempty"`
}

// ApplyTrackerItem reads an issue's link and task under lock, asks decide what to do, and
// does it, all in one transaction. A transaction-scoped advisory lock on the issue serializes
// the first import too, when there is no row yet to lock: a webhook and a poll racing on a
// new issue create one task, not two.
func (s *Store) ApplyTrackerItem(ctx context.Context, projectID domain.ID, tracker, externalRef string,
	decide func(TrackerItem) (TrackerChange, error)) (TrackerResult, error) {
	var res TrackerResult
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		res = TrackerResult{}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"conductor/tracker:"+projectID+":"+externalRef); err != nil {
			return err
		}
		var item TrackerItem
		err := tx.QueryRow(ctx, `SELECT `+trackerLinkColumns+` FROM tracker_links l
			 WHERE l.project_id = $1::uuid AND l.external_ref = $2 FOR UPDATE`, projectID, externalRef,
		).Scan(trackerLinkDest(&item.Link)...)
		switch {
		case err == nil:
			item.Linked = true
			if item.Task, err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks t
				 WHERE t.id = $1::uuid FOR UPDATE`, item.Link.TaskID).Scan); err != nil {
				return noRows(err)
			}
		case errors.Is(err, pgx.ErrNoRows):
			existing, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks t
				 WHERE t.project_id = $1::uuid AND t.external_ref = $2
				   AND t.status NOT IN ('cancelled','superseded')
				   AND NOT EXISTS (SELECT 1 FROM tracker_links x WHERE x.task_id = t.id)
				 ORDER BY t.created_at LIMIT 1 FOR UPDATE`, projectID, externalRef).Scan)
			if err == nil {
				item.Existing = &existing
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		default:
			return err
		}

		ch, err := decide(item)
		if err != nil {
			return err
		}
		return applyTrackerChangeTx(ctx, tx, projectID, tracker, externalRef, item, ch, &res)
	})
	return res, err
}

func applyTrackerChangeTx(ctx context.Context, tx pgx.Tx, projectID domain.ID, tracker, externalRef string,
	item TrackerItem, ch TrackerChange, res *TrackerResult) error {
	task := item.Task
	switch {
	case item.Linked:
	case ch.Create != nil:
		if ch.Remote == nil {
			return errors.New("tracker import without the issue's state")
		}
		p := *ch.Create
		p.ProjectID, p.ExternalRef = projectID, externalRef
		created, err := createTaskTx(ctx, tx, p)
		if err != nil {
			return err
		}
		task, res.Created = created, true
		if err := insertTrackerLinkTx(ctx, tx, projectID, tracker, externalRef, task.ID, *ch.Remote, false); err != nil {
			return err
		}
	case ch.Adopt && item.Existing != nil:
		if ch.Remote == nil {
			return errors.New("tracker adoption without the issue's state")
		}
		task, res.Adopted = *item.Existing, true
		// The person who filed the task by hand wrote its text; it counts as a Conductor edit,
		// so the issue's current text does not overwrite it, but a later issue edit does.
		if err := insertTrackerLinkTx(ctx, tx, projectID, tracker, externalRef, task.ID, *ch.Remote, true); err != nil {
			return err
		}
	default:
		return nil // not linked, and nothing to import
	}
	res.TaskID, res.TaskRef, res.From, res.Status = task.ID, task.Ref, task.Status, task.Status

	// Field updates.
	sets := []string{}
	args := []any{task.ID}
	add := func(field, expr string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf(expr, len(args)))
		res.Updated = append(res.Updated, field)
	}
	if item.Linked || res.Adopted {
		if ch.Title != nil && *ch.Title != task.Title {
			add("title", "title = $%d", *ch.Title)
		}
		if ch.Objective != nil && *ch.Objective != task.Objective {
			add("objective", "objective = $%d", *ch.Objective)
		}
		if ch.Criteria != nil && !sameCriteria(*ch.Criteria, task.AcceptanceCriteria) {
			body, err := marshalJSON(*ch.Criteria)
			if err != nil {
				return err
			}
			add("acceptance_criteria", "acceptance_criteria = $%d", body)
		}
	}
	if len(sets) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET `+strings.Join(sets, ", ")+`, updated_at = now()
			 WHERE id = $1::uuid`, args...); err != nil {
			return err
		}
	}

	// The link's view of the issue. A state change makes the write-back look at the task
	// again (written_at = NULL): a close it deferred may now apply.
	if ch.Remote != nil && (item.Linked || res.Adopted) {
		if _, err := tx.Exec(ctx, `
			UPDATE tracker_links
			   SET url = $3, remote_public = $5,
			       written_at = CASE WHEN remote_state <> $4 THEN NULL ELSE written_at END,
			       remote_state = $4,
			       remote_updated_at = GREATEST(remote_updated_at, $6),
			       synced_title_hash = $7, synced_body_hash = $8
			 WHERE project_id = $1::uuid AND external_ref = $2`,
			projectID, externalRef, ch.Remote.URL, ch.Remote.State, ch.Remote.Public,
			ch.Remote.UpdatedAt, ch.Remote.TitleHash, ch.Remote.BodyHash); err != nil {
			return err
		}
	}

	// Status.
	switch ch.Status {
	case TrackerCancel:
		updated, err := updateTaskStatusTx(ctx, tx, task.ID, domain.TaskCancelled, ch.Reason)
		if err != nil {
			return err
		}
		res.Status = updated.Status
		if _, err := tx.Exec(ctx, `UPDATE tracker_links SET cancelled_by_sync = true
			 WHERE project_id = $1::uuid AND external_ref = $2`, projectID, externalRef); err != nil {
			return err
		}
	case TrackerRevive:
		if err := reviveTaskTx(ctx, tx, task, ch.Reason); err != nil {
			return err
		}
		res.Status = domain.TaskReady
		if _, err := tx.Exec(ctx, `UPDATE tracker_links SET cancelled_by_sync = false
			 WHERE project_id = $1::uuid AND external_ref = $2`, projectID, externalRef); err != nil {
			return err
		}
	}

	if res.Created || res.Adopted || len(res.Updated) > 0 {
		// The issue is named under "reason", which follows the task's visibility in the event
		// stream (a private task's external_ref is its owner's), never under a territory key.
		event, reason := "tracker.updated", externalRef+" edited: "+strings.Join(res.Updated, ", ")
		switch {
		case res.Created:
			event, reason = "tracker.imported", "imported from "+externalRef
		case res.Adopted:
			event, reason = "tracker.linked", "linked to "+externalRef
		}
		payload := map[string]any{"task_ref": task.Ref, "provider": tracker, "reason": reason}
		if err := appendEvents(ctx, tx, task.OrganizationID, task.ProjectID, "",
			eventSpec{"task", task.ID, event, domain.VisibilityTeamSummary, payload}); err != nil {
			return err
		}
	}
	return nil
}

func insertTrackerLinkTx(ctx context.Context, tx pgx.Tx, projectID domain.ID, tracker, externalRef string,
	taskID domain.ID, r TrackerRemote, edited bool) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO tracker_links (project_id, external_ref, tracker, task_id, url, remote_state,
		        remote_public, remote_updated_at, synced_title_hash, synced_body_hash, task_edited_at)
		VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6, $7, $8, $9, $10,
		        CASE WHEN $11::boolean THEN now() END)`,
		projectID, externalRef, tracker, taskID, r.URL, r.State, r.Public, r.UpdatedAt,
		r.TitleHash, r.BodyHash, edited)
	return err
}

// reviveTaskTx returns a cancelled task to ready. It is the one edge out of cancelled, and
// it exists only for issue sync, deliberately outside the state machine: the sync cancelled
// the task as a mirror of its issue closing, and a reopened issue undoes that. Nothing about
// the task is in flight — cancelling ended its lease, bumped its fencing epoch, and released
// its reservations — so ready is exactly what a fresh import of the issue would produce,
// without losing the task's history. Only tasks the sync itself cancelled are revived
// (tracker_links.cancelled_by_sync, checked by the caller); a task a person cancelled stays
// cancelled.
func reviveTaskTx(ctx context.Context, tx pgx.Tx, task domain.Task, reason string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE tasks SET status = 'ready', completed_at = NULL, updated_at = now()
		 WHERE id = $1::uuid AND status = 'cancelled'`, task.ID)
	if err != nil {
		if isUniqueViolation(err, "tasks_external_ref_unique") {
			return fmt.Errorf("%w: another open task already names %s; %s stays cancelled",
				domain.ErrConflict, task.ExternalRef, task.Ref)
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return appendEvents(ctx, tx, task.OrganizationID, task.ProjectID, "",
		eventSpec{"task", task.ID, "task.status_changed", domain.VisibilityTeamSummary, map[string]any{
			"task_ref": task.Ref, "from": string(domain.TaskCancelled), "to": string(domain.TaskReady),
			"status": string(domain.TaskReady), "reason": reason,
		}})
}

func sameCriteria(a, b []domain.AcceptanceCriterion) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Text != b[i].Text {
			return false
		}
	}
	return true
}

// TrackedTask is a synced task as the write-back sees it: the link, the task's state, who
// holds it, and the project's settings.
type TrackedTask struct {
	Link             TrackerLink
	TaskRef          string
	Status           domain.TaskStatus
	Visibility       domain.Visibility
	PullRequestURL   string
	PullRequestState string
	UpdatedAt        time.Time
	// HolderID and HolderHandle name whoever holds the task's live lease, if anyone does.
	HolderID        domain.ID
	HolderHandle    string
	InProgressLabel string
	InstallationID  int64
}

// TrackerWriteBackDue lists synced tasks that changed since the write-back last looked at
// them, oldest change first, in an organization's projects whose sync is enabled. Projects
// whose write-back is failing are left out unless includeFailing is set, so they do not take
// the places of projects that can be written to.
func (s *Store) TrackerWriteBackDue(ctx context.Context, orgID domain.ID, tracker string, limit int, includeFailing bool) ([]TrackedTask, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+trackerLinkColumns+`, t.ref, t.status, t.visibility, t.pull_request_url,
		       t.pull_request_state, t.updated_at, COALESCE(le.holder_principal::text, ''),
		       COALESCE(p.handle, ''), c.in_progress_label, c.installation_id
		  FROM tracker_links l
		  JOIN tasks t ON t.id = l.task_id
		  JOIN tracker_configs c ON c.project_id = l.project_id AND c.tracker = l.tracker AND c.enabled
		  JOIN projects pr ON pr.id = l.project_id AND pr.organization_id = $3::uuid
		  LEFT JOIN leases le ON le.id = t.active_lease_id AND le.released_at IS NULL
		  LEFT JOIN principals p ON p.id = le.holder_principal
		 WHERE l.tracker = $1 AND (l.written_at IS NULL OR t.updated_at > l.written_at)
		   AND ($4 OR c.writeback_error = '')
		 ORDER BY t.updated_at
		 LIMIT $2`, tracker, limit, orgID, includeFailing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrackedTask
	for rows.Next() {
		var t TrackedTask
		dest := append(trackerLinkDest(&t.Link), &t.TaskRef, &t.Status, &t.Visibility, &t.PullRequestURL,
			&t.PullRequestState, &t.UpdatedAt, &t.HolderID, &t.HolderHandle, &t.InProgressLabel, &t.InstallationID)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TrackerNoted is what an issue has been told, after a write-back step.
type TrackerNoted struct {
	ClaimNotedFor domain.ID
	LabelApplied  string
	DoneNoted     bool
	RemoteState   string
}

// RecordTrackerWriteBack stores what an issue has been told. Each step is recorded as soon as
// GitHub accepted it, so a pass cut short does not repeat it. reconciled, when set, is the
// task's updated_at the whole pass was computed from: the task is not due again until it
// changes after that.
func (s *Store) RecordTrackerWriteBack(ctx context.Context, projectID domain.ID, externalRef string, n TrackerNoted, reconciled *time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tracker_links
		   SET claim_noted_for = NULLIF($3, '')::uuid, label_applied = $4, done_noted = $5,
		       remote_state = COALESCE(NULLIF($6, ''), remote_state),
		       written_at = CASE WHEN $7::timestamptz IS NULL THEN written_at
		                         ELSE GREATEST(written_at, $7::timestamptz) END
		 WHERE project_id = $1::uuid AND external_ref = $2`,
		projectID, externalRef, n.ClaimNotedFor, n.LabelApplied, n.DoneNoted, n.RemoteState, reconciled)
	return err
}

// TaskTrackerLink returns the link of a task imported from a tracker; found is false for a
// task that was not.
func (s *Store) TaskTrackerLink(ctx context.Context, taskID domain.ID) (TrackerLink, bool, error) {
	var l TrackerLink
	err := s.pool.QueryRow(ctx, `SELECT `+trackerLinkColumns+` FROM tracker_links l WHERE l.task_id = $1::uuid`,
		taskID).Scan(trackerLinkDest(&l)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, false, nil
	}
	return l, err == nil, err
}
