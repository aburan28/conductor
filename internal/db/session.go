package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/aburan28/conductor/internal/domain"
)

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

const sessionColumns = `
	id::text, project_id::text, principal_id::text, COALESCE(runner_id::text, ''),
	harness, harness_version, machine_id, base_sha, branch, worktree_path,
	visibility, COALESCE(active_task_id::text, ''), state, capabilities,
	started_at, heartbeat_at, expires_at, closed_at`

func scanSession(scan func(...any) error) (domain.Session, error) {
	var s domain.Session
	var caps []byte
	if err := scan(&s.ID, &s.ProjectID, &s.PrincipalID, &s.RunnerID,
		&s.Harness, &s.HarnessVersion, &s.MachineID, &s.BaseSHA, &s.Branch, &s.WorktreePath,
		&s.Visibility, &s.ActiveTaskID, &s.State, &caps,
		&s.StartedAt, &s.HeartbeatAt, &s.ExpiresAt, &s.ClosedAt); err != nil {
		return domain.Session{}, err
	}
	if err := decodeJSON(caps, &s.Capabilities); err != nil {
		return domain.Session{}, err
	}
	return s, nil
}

type RegisterSessionParams struct {
	ProjectID      domain.ID
	PrincipalID    domain.ID
	RunnerID       domain.ID
	Harness        string
	HarnessVersion string
	MachineID      string
	BaseSHA        string
	Branch         string
	WorktreePath   string
	Visibility     domain.Visibility
	Capabilities   domain.SessionCapabilities
	TTL            time.Duration
}

// RegisterSession records a live working context (DESIGN.md §7.3).
//
// A session expires unless it heartbeats. That is what lets presence reflect reality when a
// laptop closes mid-task instead of showing someone as working forever.
func (s *Store) RegisterSession(ctx context.Context, p RegisterSessionParams) (domain.Session, error) {
	if p.Visibility == "" {
		p.Visibility = domain.VisibilityTeamSummary
	}
	if p.TTL <= 0 {
		p.TTL = 90 * time.Second
	}
	caps, err := marshalJSON(p.Capabilities)
	if err != nil {
		return domain.Session{}, err
	}
	sess, err := scanSession(s.pool.QueryRow(ctx, `
		INSERT INTO sessions (project_id, principal_id, runner_id, harness, harness_version,
		                      machine_id, base_sha, branch, worktree_path, visibility,
		                      capabilities, expires_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11,
		        now() + $12::interval)
		RETURNING `+sessionColumns,
		p.ProjectID, p.PrincipalID, nullable(p.RunnerID), p.Harness, p.HarnessVersion,
		p.MachineID, p.BaseSHA, p.Branch, p.WorktreePath, p.Visibility, caps, p.TTL.String(),
	).Scan)
	return sess, err
}

// SetSessionCapabilities replaces a session's advertised capabilities.
//
// Capabilities change mid-session — someone switches model or raises effort — and routing
// that trusts a stale advertisement will hand xhigh work to a session that dropped to medium
// an hour ago.
func (s *Store) SetSessionCapabilities(ctx context.Context, id domain.ID, caps domain.SessionCapabilities) (domain.Session, error) {
	raw, err := marshalJSON(caps)
	if err != nil {
		return domain.Session{}, err
	}
	sess, err := scanSession(s.pool.QueryRow(ctx, `
		UPDATE sessions SET capabilities = $2
		 WHERE id = $1::uuid AND closed_at IS NULL
		RETURNING `+sessionColumns, id, raw).Scan)
	return sess, noRows(err)
}

func (s *Store) GetSession(ctx context.Context, id domain.ID) (domain.Session, error) {
	sess, err := scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = $1::uuid`, id).Scan)
	return sess, noRows(err)
}

type HeartbeatSessionParams struct {
	SessionID domain.ID
	State     domain.SessionState
	Branch    string
	BaseSHA   string
	TTL       time.Duration
	// ChangedPaths are the repository-relative paths the session's working tree differs in
	// (`git diff --name-only` plus untracked files), when the caller observed them. Paths
	// only: nothing about what changed. nil leaves the recorded set alone.
	ChangedPaths []string
}

// maxObservedPaths bounds what one heartbeat may record. A generated tree or a vendored
// dependency can touch thousands of files; the merge-risk graph needs to know that the
// session is wide, not every name.
const maxObservedPaths = 500

// HeartbeatSession extends a session's life and updates its presence state.
//
// It also renews every live lease the session holds. A lease bound to a session lives exactly
// as long as the session does: the sidecar that proves the person is still there is the same
// signal that proves their claim is still being worked. Without this an interactive claim
// expired after one TTL (90 seconds by default) unless the model happened to call a
// progress tool, and the reconciler then gave the territory away under a live session.
//
// Only leases whose holder is the session's own principal are renewed, so binding a lease to
// someone else's session cannot borrow their heartbeat. A lease that has already expired is
// not revived: once it lapsed, the reconciler (or a competing claim) may already be taking it
// back, and resurrecting it would race that.
//
// This is deliberately not an MCP tool (DESIGN.md §7.2): heartbeats are high frequency, and
// spending model tokens on them would be absurd. A local adapter or the CLI sidecar calls it
// directly over HTTP.
func (s *Store) HeartbeatSession(ctx context.Context, p HeartbeatSessionParams) (domain.Session, error) {
	if p.TTL <= 0 {
		p.TTL = 90 * time.Second
	}
	var sess domain.Session
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		sess, err = scanSession(tx.QueryRow(ctx, `
			UPDATE sessions
			   SET heartbeat_at = now(),
			       expires_at   = now() + $2::interval,
			       state        = COALESCE(NULLIF($3, '')::text, state),
			       branch       = COALESCE(NULLIF($4, ''), branch),
			       base_sha     = COALESCE(NULLIF($5, ''), base_sha)
			 WHERE id = $1::uuid AND closed_at IS NULL
			RETURNING `+sessionColumns,
			p.SessionID, p.TTL.String(), string(p.State), p.Branch, p.BaseSHA,
		).Scan)
		if err != nil {
			return noRows(err)
		}
		rows, err := tx.Query(ctx, `
			UPDATE leases
			   SET heartbeat_at = now(), expires_at = now() + $2::interval
			 WHERE session_id = $1::uuid
			   AND holder_principal = $3::uuid
			   AND released_at IS NULL
			   AND expires_at > now()
			RETURNING attempt_id::text`, sess.ID, p.TTL.String(), sess.PrincipalID)
		if err != nil {
			return err
		}
		var attempts []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			attempts = append(attempts, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(attempts) == 0 {
			return nil
		}
		// The attempt is alive for the same reason the lease is. Observed paths, when sent,
		// replace the recorded set: a heartbeat reports the whole working tree, not a delta,
		// so a file that was reverted drops out again.
		paths := p.ChangedPaths
		if len(paths) > maxObservedPaths {
			paths = paths[:maxObservedPaths]
		}
		_, err = tx.Exec(ctx, `
			UPDATE attempts
			   SET last_event_at = now(),
			       changed_paths = CASE WHEN $2::boolean THEN $3::text[] ELSE changed_paths END
			 WHERE id = ANY($1::uuid[])`,
			attempts, p.ChangedPaths != nil, nonNilStrings(paths))
		return err
	})
	return sess, err
}

// nonNilStrings returns an empty slice for nil, because a text[] column rejects NULL.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// AdoptLeasesParams binds claims made outside any session to the session that will work them.
type AdoptLeasesParams struct {
	SessionID domain.ID
	// WorktreePath is the repository root the session runs in. Only claims recorded against
	// the same root are adopted, so a second terminal on another checkout does not take over
	// a claim it is not working.
	WorktreePath string
	TTL          time.Duration
}

// AdoptedLease is one claim a session took over.
type AdoptedLease struct {
	TaskID    domain.ID `json:"task_id"`
	TaskRef   string    `json:"task_ref"`
	LeaseID   domain.ID `json:"lease_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AdoptLeases makes the "claim, then wrap" order work: `conductor task claim` in a plain shell
// has no session to bind its lease to, so `conductor wrap` adopts it on start, and from then
// on the session's heartbeat keeps it alive.
//
// Adoption is narrow on purpose. Only the session owner's own live leases in the session's
// project qualify, only those recorded against the same worktree, and only those not already
// carried by another live session — a lease another terminal is heartbeating stays where it
// is. A lease that has already expired is not revived (see HeartbeatSession for why).
func (s *Store) AdoptLeases(ctx context.Context, p AdoptLeasesParams) ([]AdoptedLease, error) {
	if p.TTL <= 0 {
		p.TTL = 90 * time.Second
	}
	if p.WorktreePath == "" {
		return nil, nil
	}
	var out []AdoptedLease
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var principalID, projectID domain.ID
		var closed bool
		if err := tx.QueryRow(ctx, `
			SELECT principal_id::text, project_id::text, closed_at IS NOT NULL
			  FROM sessions WHERE id = $1::uuid FOR UPDATE`, p.SessionID,
		).Scan(&principalID, &projectID, &closed); err != nil {
			return noRows(err)
		}
		if closed {
			return fmt.Errorf("%w: session %s is closed", domain.ErrNotPermitted, p.SessionID)
		}
		rows, err := tx.Query(ctx, `
			UPDATE leases l
			   SET session_id = $1::uuid, heartbeat_at = now(), expires_at = now() + $5::interval
			  FROM attempts a, tasks t
			 WHERE a.id = l.attempt_id AND t.id = l.task_id
			   AND l.project_id = $2::uuid
			   AND l.holder_principal = $3::uuid
			   AND l.released_at IS NULL
			   AND l.expires_at > now()
			   AND a.worktree_path = $4
			   AND (l.session_id IS NULL OR NOT EXISTS (
			         SELECT 1 FROM sessions other
			          WHERE other.id = l.session_id
			            AND other.closed_at IS NULL AND other.expires_at > now()))
			RETURNING l.task_id::text, t.ref, l.id::text, l.expires_at`,
			p.SessionID, projectID, principalID, p.WorktreePath, p.TTL.String())
		if err != nil {
			return err
		}
		for rows.Next() {
			var a AdoptedLease
			if err := rows.Scan(&a.TaskID, &a.TaskRef, &a.LeaseID, &a.ExpiresAt); err != nil {
				rows.Close()
				return err
			}
			out = append(out, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, a := range out {
			if _, err := tx.Exec(ctx, `
				UPDATE attempts SET session_id = $2::uuid, last_event_at = now()
				 WHERE id = (SELECT attempt_id FROM leases WHERE id = $1::uuid)`,
				a.LeaseID, p.SessionID); err != nil {
				return err
			}
		}
		if len(out) > 0 {
			// The session now works the (most recently adopted) task, which is what presence
			// and the pre-edit hook read.
			if _, err := tx.Exec(ctx, `
				UPDATE sessions SET active_task_id = $2::uuid, state = 'working'
				 WHERE id = $1::uuid`, p.SessionID, out[len(out)-1].TaskID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// SessionLease returns the live lease a session holds for its active task, as the fence a
// mutation on that task must present. It is how a caller that knows only its session — the
// pre-edit hook in a wrapped terminal — acts on the claim the session carries, without ever
// handling the epoch itself.
func (s *Store) SessionLease(ctx context.Context, sessionID domain.ID) (domain.Lease, error) {
	l, err := scanLease(s.pool.QueryRow(ctx, `
		SELECT `+leaseColumnsQualified+`
		  FROM leases l
		  JOIN sessions s ON s.id = l.session_id
		 WHERE l.session_id = $1::uuid
		   AND l.holder_principal = s.principal_id
		   AND l.released_at IS NULL AND l.expires_at > now()
		 ORDER BY (l.task_id = s.active_task_id) DESC NULLS LAST, l.acquired_at DESC
		 LIMIT 1`, sessionID).Scan)
	return l, noRows(err)
}

func (s *Store) SetSessionTask(ctx context.Context, sessionID, taskID domain.ID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET active_task_id = $2::uuid WHERE id = $1::uuid`,
		sessionID, nullable(taskID))
	return err
}

func (s *Store) CloseSession(ctx context.Context, id domain.ID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions
		   SET closed_at = now(), state = 'closed', active_task_id = NULL
		 WHERE id = $1::uuid AND closed_at IS NULL`, id)
	return err
}

// ReapSessions advances presence state for sessions that stopped heartbeating: first to
// offline_grace, then to stale (DESIGN.md §7.3). It returns how many rows changed.
//
// Sessions are never deleted. A stale session is evidence for recovery — it names the
// worktree and branch a crashed run left behind.
func (s *Store) ReapSessions(ctx context.Context, grace time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions
		   SET state = CASE
		         WHEN heartbeat_at < now() - $1::interval THEN 'stale'
		         ELSE 'offline_grace'
		       END
		 WHERE closed_at IS NULL
		   AND expires_at < now()
		   AND state NOT IN ('stale','closed')`, grace.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// LiveSessions returns sessions that are still present, for the presence projection.
func (s *Store) LiveSessions(ctx context.Context, projectID domain.ID) ([]domain.Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+sessionColumns+`
		  FROM sessions
		 WHERE project_id = $1::uuid AND closed_at IS NULL AND state <> 'stale'
		 ORDER BY heartbeat_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Session
	for rows.Next() {
		sess, err := scanSession(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// ListSessions returns every session a project has ever had, newest first.
//
// Sessions are never deleted (see ReapSessions), so this is the complete record: live,
// stale, and closed alike. Presence answers "who is here now"; this answers "who has been
// here", which is what an export needs.
func (s *Store) ListSessions(ctx context.Context, projectID domain.ID) ([]domain.Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+sessionColumns+`
		  FROM sessions
		 WHERE project_id = $1::uuid
		 ORDER BY started_at DESC, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Session{}
	for rows.Next() {
		sess, err := scanSession(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Runners
// ---------------------------------------------------------------------------

const runnerColumns = `
	id::text, organization_id::text, COALESCE(project_id::text, ''),
	COALESCE(principal_id::text, ''), name, capabilities, max_concurrency,
	in_flight, state, registered_at, heartbeat_at`

func scanRunner(scan func(...any) error) (domain.Runner, error) {
	var r domain.Runner
	var raw []byte
	if err := scan(&r.ID, &r.OrganizationID, &r.ProjectID, &r.PrincipalID, &r.Name,
		&raw, &r.MaxConcurrency, &r.InFlight, &r.State,
		&r.RegisteredAt, &r.HeartbeatAt); err != nil {
		return domain.Runner{}, err
	}
	if err := decodeJSON(raw, &r.Capabilities); err != nil {
		return domain.Runner{}, err
	}
	return r, nil
}

// RegisterRunner advertises a machine's execution capabilities (DESIGN.md §7.10). Runners
// connect outbound, so a laptop never needs an inbound port.
//
// Runner names are unique per organization, and re-registering under a name refreshes that
// runner — but only for the principal that registered it. Before, any member of the
// organization could register a teammate's runner name and take the record over, steering
// the scheduler's view of that machine; now that is a conflict.
func (s *Store) RegisterRunner(ctx context.Context, r domain.Runner) (domain.Runner, error) {
	caps, err := marshalJSON(r.Capabilities)
	if err != nil {
		return domain.Runner{}, err
	}
	if r.MaxConcurrency <= 0 {
		r.MaxConcurrency = 1
	}
	runner, err := scanRunner(s.pool.QueryRow(ctx, `
		INSERT INTO runners (organization_id, project_id, principal_id, name,
		                     capabilities, max_concurrency, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, 'online')
		ON CONFLICT (organization_id, name) DO UPDATE SET
		    project_id = EXCLUDED.project_id,
		    principal_id = EXCLUDED.principal_id,
		    capabilities = EXCLUDED.capabilities,
		    max_concurrency = EXCLUDED.max_concurrency,
		    state = 'online',
		    heartbeat_at = now()
		 WHERE runners.principal_id IS NULL OR runners.principal_id = EXCLUDED.principal_id
		RETURNING `+runnerColumns,
		r.OrganizationID, nullable(r.ProjectID), nullable(r.PrincipalID), r.Name,
		caps, r.MaxConcurrency,
	).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		// The name exists and belongs to someone else: the conflict update matched nothing.
		return domain.Runner{}, fmt.Errorf(
			"%w: a runner named %q is registered by another principal; choose another name (--name)",
			domain.ErrDuplicate, r.Name)
	}
	return runner, err
}

// HeartbeatRunner refreshes a runner's liveness and load. Only the principal that registered
// the runner may do it: anyone else could otherwise hold a dead machine "online", or report a
// live one as full, and steer the scheduler around it. A runner that does not exist and one
// that belongs to someone else are the same ErrNotFound, so the endpoint cannot be used to
// probe for runner ids.
func (s *Store) HeartbeatRunner(ctx context.Context, id, principalID domain.ID, inFlight int) error {
	if inFlight < 0 {
		inFlight = 0
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE runners SET heartbeat_at = now(), in_flight = $3, state = 'online'
		 WHERE id = $1::uuid AND principal_id = $2::uuid`, id, principalID, inFlight)
	if err != nil {
		if isBadUUIDError(err) {
			return domain.ErrNotFound
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// GetRunner loads one runner by id.
func (s *Store) GetRunner(ctx context.Context, id domain.ID) (domain.Runner, error) {
	r, err := scanRunner(s.pool.QueryRow(ctx,
		`SELECT `+runnerColumns+` FROM runners WHERE id = $1::uuid`, id).Scan)
	if err != nil && isBadUUIDError(err) {
		return domain.Runner{}, domain.ErrNotFound
	}
	return r, noRows(err)
}

// isBadUUIDError reports a non-UUID string cast to uuid, which is a lookup miss.
func isBadUUIDError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// AvailableRunners lists runners with spare capacity that can execute a given harness.
func (s *Store) AvailableRunners(ctx context.Context, projectID domain.ID, harness string, staleAfter time.Duration) ([]domain.Runner, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+runnerColumns+`
		  FROM runners
		 WHERE (project_id = $1::uuid OR project_id IS NULL)
		   AND state = 'online'
		   AND heartbeat_at > now() - $2::interval
		   AND in_flight < max_concurrency
		   AND ($3 = '' OR capabilities->'harnesses' @> to_jsonb($3::text))
		 ORDER BY in_flight, heartbeat_at DESC`,
		projectID, staleAfter.String(), harness)
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
	return out, rows.Err()
}

// MarkStaleRunners takes runners offline once they stop heartbeating, so the scheduler stops
// dispatching to a machine that is gone.
func (s *Store) MarkStaleRunners(ctx context.Context, staleAfter time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runners SET state = 'offline'
		 WHERE state <> 'offline' AND heartbeat_at < now() - $1::interval`, staleAfter.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
