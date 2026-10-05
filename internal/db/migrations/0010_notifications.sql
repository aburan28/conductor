-- Notifications: per-project channels (outbound webhooks, Slack, Discord) and the relay that
-- delivers outbox_events to them (internal/notify, docs/DESIGN.md §23.4).
--
-- A channel's URL and signing secret are credentials: a Slack or Discord webhook URL is the
-- whole credential, and the signing secret is what a receiver trusts. Both are sealed under
-- conductord's secret key (internal/secretbox), which is kept outside the database, so a
-- dump of this table cannot post to anyone's Slack. Only a short hint is kept in the clear.

CREATE TABLE notification_channels (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind            text NOT NULL CHECK (kind IN ('webhook', 'slack', 'discord')),
    name            text NOT NULL DEFAULT '',
    url_sealed      text NOT NULL,
    url_hint        text NOT NULL,
    secret_sealed   text NOT NULL DEFAULT '',
    -- Event types this channel receives; an entry may be "type:status" to match one target
    -- status of task.status_changed, or "*" for everything.
    events          text[] NOT NULL,
    created_by      uuid REFERENCES principals(id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Delivery health, for the API and the dashboard. retry_after is the channel's backoff:
    -- while it is in the future, the relay sends nothing to it.
    last_success_at timestamptz,
    last_error      text NOT NULL DEFAULT '',
    last_error_at   timestamptz,
    failures        integer NOT NULL DEFAULT 0,
    retry_after     timestamptz
);

CREATE INDEX notification_channels_project ON notification_channels (project_id);

-- One row per (outbox row, channel) the relay has tried, so a retry goes only to the channels
-- that have not had the event yet. Rows go with their outbox row.
CREATE TABLE notification_deliveries (
    outbox_id    bigint NOT NULL REFERENCES outbox_events(id) ON DELETE CASCADE,
    channel_id   uuid NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    state        text NOT NULL CHECK (state IN ('pending', 'delivered', 'failed')),
    attempts     integer NOT NULL DEFAULT 0,
    last_error   text NOT NULL DEFAULT '',
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (outbox_id, channel_id)
);

CREATE INDEX notification_deliveries_channel ON notification_deliveries (channel_id);

-- The relay's claim. A claimed row's next_attempt_at is pushed past the claim's lifetime, so
-- another replica skips it without a transaction held open across HTTP calls; a failed
-- delivery pushes it to the channel's backoff. NULL means due now.
ALTER TABLE outbox_events ADD COLUMN next_attempt_at timestamptz;

-- The relay reads the pending rows of projects that have a channel, oldest first. Projects
-- without one are never scanned; retention bounds their undelivered rows as before.
CREATE INDEX outbox_events_project_pending ON outbox_events (project_id, id) WHERE delivered_at IS NULL;

-- Conflict announcements (conflict.blocked, conflict.suggest_join, conflict.detected): the
-- persisted half of "at most once per window", like budget_alert_levels and
-- attempt_stall_alerts. An agent polling a blocked check, a restart, or a second replica
-- finds the row and writes no second event.
--
--   blocked / suggest_join: subject is the requesting principal and task_id the task in the
--     way; a row older than the window no longer counts and is replaced.
--   detected: subject is the edge's first task, task_id its second, kind the edge kind; the
--     row lives while the conflict stays open, so each conflict is announced once.
CREATE TABLE conflict_alerts (
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    outcome      text NOT NULL CHECK (outcome IN ('blocked', 'suggest_join', 'detected')),
    subject      uuid NOT NULL,
    task_id      uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind         text NOT NULL DEFAULT '',
    announced_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, outcome, subject, task_id, kind)
);
