package db

import (
	"context"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

const sessionColumns = `
	id::text, project_id::text, principal_id::text, COALESCE(runner_id::text, ''),
	harness, harness_version, machine_id, base_sha, branch, worktree_path,
	visibility, COALESCE(active_task_id::text, ''), state, pending_control, capabilities,
	started_at, heartbeat_at, expires_at, closed_at`

func scanSession(scan func(...any) error) (domain.Session, error) {
	var s domain.Session
	var caps []byte
	if err := scan(&s.ID, &s.ProjectID, &s.PrincipalID, &s.RunnerID,
		&s.Harness, &s.HarnessVersion, &s.MachineID, &s.BaseSHA, &s.Branch, &s.WorktreePath,
		&s.Visibility, &s.ActiveTaskID, &s.State, &s.PendingControl, &caps,
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
	SessionID  domain.ID
	State      domain.SessionState
	Branch     string
	BaseSHA    string
	ControlAck domain.SessionControl
	TTL        time.Duration
}

// HeartbeatSession extends a session's life and updates its presence state.
//
// This is deliberately not an MCP tool (DESIGN.md §7.2): heartbeats are high frequency, and
// spending model tokens on them would be absurd. A local adapter or the CLI sidecar calls it
// directly over HTTP.
//
// ControlAck is the sidecar saying what it is currently doing ("pause" while frozen,
// "resume" while running). A pending control clears the moment the acknowledgement matches
// it — including when the session was already in that state, so a control that asks for
// reality needs no action and is simply confirmed.
func (s *Store) HeartbeatSession(ctx context.Context, p HeartbeatSessionParams) (domain.Session, error) {
	if p.TTL <= 0 {
		p.TTL = 90 * time.Second
	}
	sess, err := scanSession(s.pool.QueryRow(ctx, `
		UPDATE sessions
		   SET heartbeat_at = now(),
		       expires_at   = now() + $2::interval,
		       state        = COALESCE(NULLIF($3, '')::text, state),
		       branch       = COALESCE(NULLIF($4, ''), branch),
		       base_sha     = COALESCE(NULLIF($5, ''), base_sha),
		       pending_control = CASE WHEN $6 <> '' AND $6 = pending_control
		                              THEN '' ELSE pending_control END
		 WHERE id = $1::uuid AND closed_at IS NULL
		RETURNING `+sessionColumns,
		p.SessionID, p.TTL.String(), string(p.State), p.Branch, p.BaseSHA, string(p.ControlAck),
	).Scan)
	return sess, noRows(err)
}

// RequestSessionControl records a pause or resume for the session's sidecar to pick up on
// its next heartbeat. Idempotent: asking twice leaves one pending control, and asking for
// the state the session is already in clears on the next acknowledgement.
func (s *Store) RequestSessionControl(ctx context.Context, id domain.ID, control domain.SessionControl) (domain.Session, error) {
	sess, err := scanSession(s.pool.QueryRow(ctx, `
		UPDATE sessions SET pending_control = $2
		 WHERE id = $1::uuid AND closed_at IS NULL
		RETURNING `+sessionColumns, id, string(control)).Scan)
	return sess, noRows(err)
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
func (s *Store) RegisterRunner(ctx context.Context, r domain.Runner) (domain.Runner, error) {
	caps, err := marshalJSON(r.Capabilities)
	if err != nil {
		return domain.Runner{}, err
	}
	if r.MaxConcurrency <= 0 {
		r.MaxConcurrency = 1
	}
	return scanRunner(s.pool.QueryRow(ctx, `
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
		RETURNING `+runnerColumns,
		r.OrganizationID, nullable(r.ProjectID), nullable(r.PrincipalID), r.Name,
		caps, r.MaxConcurrency,
	).Scan)
}

func (s *Store) HeartbeatRunner(ctx context.Context, id domain.ID, inFlight int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE runners SET heartbeat_at = now(), in_flight = $2, state = 'online'
		 WHERE id = $1::uuid`, id, inFlight)
	return err
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
