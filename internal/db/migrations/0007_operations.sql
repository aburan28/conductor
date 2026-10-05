-- Operations and replica safety: indexes for the scheduler's hot queries and for retention,
-- shared liveness for scheduler replicas, persisted alert levels, and the GitHub App state
-- that used to live in one process's memory or on one host's disk.

-- ---------------------------------------------------------------------------
-- Indexes
-- ---------------------------------------------------------------------------

-- SpendSince filters attempts by project and window for every budgeted project on every
-- scheduler tick. Without this it is a sequential scan of the whole attempts table.
CREATE INDEX attempts_project_created ON attempts (project_id, created_at);

-- Retention deletes by age across all tenants, so each pruned table needs an index that
-- leads with its timestamp; the existing ones lead with a tenant column.
CREATE INDEX domain_events_occurred ON domain_events (occurred_at);
CREATE INDEX outbox_events_created ON outbox_events (created_at);
CREATE INDEX audit_log_created ON audit_log (created_at);
CREATE INDEX idempotency_keys_created ON idempotency_keys (created_at);
CREATE INDEX usage_buckets_start ON usage_buckets (bucket_start);

-- outbox_events.event_id cascades from domain_events. Without an index every deleted event
-- scans the outbox for rows to cascade to.
CREATE INDEX outbox_events_event ON outbox_events (event_id);

-- ---------------------------------------------------------------------------
-- Liveness of background components, shared by every replica
-- ---------------------------------------------------------------------------

-- One row per component ('scheduler', 'github_poller'). The scheduler uses its row to tell
-- an outage (no replica ticked for a while, so no worker could heartbeat) from a dead
-- worker; /v1/ready reports both rows.
CREATE TABLE service_heartbeats (
    component   text PRIMARY KEY,
    last_run_at timestamptz NOT NULL DEFAULT now(),
    holder      text NOT NULL DEFAULT '',
    last_error  text NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- Budget and stall alerts
-- ---------------------------------------------------------------------------

-- The last budget threshold the scheduler announced for a project. An event is written only
-- when this changes, so restarts and replicas do not announce the same crossing again.
CREATE TABLE budget_alert_levels (
    project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    level      text NOT NULL CHECK (level IN ('', 'downshift', 'exhausted')),
    changed_at timestamptz NOT NULL DEFAULT now()
);

-- Attempts the scheduler has announced as stalled. A row exists while the attempt stays
-- silent and is removed when it recovers or ends, so each stall is announced once — not
-- again after a restart, and not once per replica.
CREATE TABLE attempt_stall_alerts (
    attempt_id   uuid PRIMARY KEY REFERENCES attempts(id) ON DELETE CASCADE,
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    announced_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX attempt_stall_alerts_project ON attempt_stall_alerts (project_id);

-- ---------------------------------------------------------------------------
-- GitHub App
-- ---------------------------------------------------------------------------

-- The app's credentials, so every replica serves the same app. This holds the app's private
-- key and webhook secret: database backups now carry them too (docs/OPERATIONS.md).
CREATE TABLE github_app (
    singleton   boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    credentials jsonb NOT NULL,
    source      text NOT NULL DEFAULT 'setup' CHECK (source IN ('setup', 'imported')),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Pending manifest-flow setups. The state is a bearer secret in a URL, so only its hash is
-- kept; GitHub's callback may land on any replica.
CREATE TABLE github_setup_states (
    state_hash text PRIMARY KEY,
    org        text NOT NULL DEFAULT '',
    name       text NOT NULL,
    created_by uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The check run Conductor posted for each head commit, and a fingerprint of what it said.
-- A repeat check with the same result posts nothing; a changed result updates the same run
-- rather than stacking a new one, whichever replica or restart computes it.
CREATE TABLE github_check_runs (
    repository   text NOT NULL,
    head_sha     text NOT NULL,
    check_run_id bigint NOT NULL DEFAULT 0,
    fingerprint  text NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repository, head_sha)
);

CREATE INDEX github_check_runs_updated ON github_check_runs (updated_at);
