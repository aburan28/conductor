-- Usage-limit tracking (docs/USAGE_LIMITS.md).
--
-- quota_snapshots holds the latest reading of each window of each subscription login a
-- principal reported: one row per (principal, machine, harness, account, window). The
-- account is a label for the login (its state directory's name, or an opaque hash), never an
-- email or a token, and nothing but numbers, labels, and times has a column here. Readings
-- belong to the principal, not to a project — a login is used across projects — and only
-- that principal ever reads the rows back; project members see counts.
--
-- quota_alerts is the persisted half of "once per window per level": a row per window per
-- level raised, carrying the reset time of the window it was raised for, so a later reading
-- in the same window does not raise it again and a reading in the next window does.

CREATE TABLE quota_snapshots (
    principal_id    uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    machine         text NOT NULL DEFAULT '',
    harness         text NOT NULL,
    account         text NOT NULL,
    window_name     text NOT NULL,
    window_minutes  integer NOT NULL DEFAULT 0,
    used_percent    double precision,
    used_value      double precision,
    limit_value     double precision,
    unit            text NOT NULL DEFAULT '',
    resets_at       timestamptz,
    limit_reached   boolean NOT NULL DEFAULT false,
    plan            text NOT NULL DEFAULT '',
    source          text NOT NULL,
    source_kind     text NOT NULL CHECK (source_kind IN ('documented','local_file','undocumented','manual')),
    observed_at     timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, machine, harness, account, window_name)
);

-- The team view counts recent readings across a project's members.
CREATE INDEX quota_snapshots_recent ON quota_snapshots (observed_at DESC);

CREATE TABLE quota_alerts (
    id               uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    principal_id     uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    machine          text NOT NULL DEFAULT '',
    harness          text NOT NULL,
    account          text NOT NULL,
    window_name      text NOT NULL,
    level            text NOT NULL CHECK (level IN ('warning','critical','exhausted')),
    window_resets_at timestamptz,
    alerted_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, machine, harness, account, window_name, level)
);
