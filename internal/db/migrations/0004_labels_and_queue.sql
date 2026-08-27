-- Task labels and the admission queue.
--
-- Labels are free-form tags dispatch rules can match ("docs go to the local model") and
-- people filter by. They live with the title: visible under the same rules, never a place
-- for private context.
ALTER TABLE tasks ADD COLUMN labels text[] NOT NULL DEFAULT '{}';
CREATE INDEX tasks_labels ON tasks USING gin (labels);

-- Admission tickets. When a project caps active sessions or attempts, work past the cap
-- queues here in arrival order instead of failing. A granted ticket must be heartbeated —
-- an abandoned slot is handed on by the scheduler, never held forever. Rows carry identity,
-- a kind, and a model name; what the work is about is not recorded here.
CREATE TABLE admission_tickets (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    principal_id  uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    session_id    uuid REFERENCES sessions(id) ON DELETE SET NULL,
    task_id       uuid REFERENCES tasks(id) ON DELETE SET NULL,
    kind          text NOT NULL CHECK (kind IN ('session','attempt')),
    harness       text NOT NULL DEFAULT '',
    model         text NOT NULL DEFAULT '',
    priority      integer NOT NULL DEFAULT 0,
    state         text NOT NULL DEFAULT 'queued'
                      CHECK (state IN ('queued','granted','released','expired','cancelled')),
    note          text NOT NULL DEFAULT '',
    requested_at  timestamptz NOT NULL DEFAULT now(),
    granted_at    timestamptz,
    heartbeat_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    released_at   timestamptz
);

-- The queue is read in arrival order within a project; granting scans open tickets only.
CREATE INDEX admission_tickets_open
    ON admission_tickets (project_id, kind, priority DESC, requested_at)
    WHERE state IN ('queued','granted');
CREATE INDEX admission_tickets_expiry
    ON admission_tickets (expires_at) WHERE state IN ('queued','granted');
