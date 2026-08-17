-- Session capabilities and task assignment (DESIGN.md §7.3, §7.7, §13).
--
-- Presence used to answer "who is here"; it could not answer "who here can think hard enough
-- for this". A session now advertises the model it is driving and the reasoning effort it can
-- be asked for, and work can be offered to a specific session that clears a capability floor.

ALTER TABLE sessions
    ADD COLUMN capabilities jsonb NOT NULL DEFAULT '{}'::jsonb;

-- Finding the sessions that can serve a tier floor is the hot path for assignment.
CREATE INDEX sessions_capabilities ON sessions USING gin (capabilities)
    WHERE closed_at IS NULL;

-- `xhigh` is a distinct setting on the harnesses that expose it, not a synonym for high or
-- max, so the catalog has to be able to name it.
ALTER TABLE model_profiles
    DROP CONSTRAINT model_profiles_reasoning_effort_check;
ALTER TABLE model_profiles
    ADD CONSTRAINT model_profiles_reasoning_effort_check
    CHECK (reasoning_effort IN ('none','low','medium','high','xhigh','max'));

ALTER TABLE attempts
    DROP CONSTRAINT attempts_reasoning_effort_check;
ALTER TABLE attempts
    ADD CONSTRAINT attempts_reasoning_effort_check
    CHECK (reasoning_effort IN ('none','low','medium','high','xhigh','max'));

CREATE TABLE task_assignments (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id       uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    session_id    uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    handoff_id    uuid REFERENCES handoffs(id) ON DELETE SET NULL,
    requirement   jsonb NOT NULL DEFAULT '{}'::jsonb,
    state         text NOT NULL DEFAULT 'offered' CHECK (state IN (
                      'offered','accepted','declined','expired','cancelled','fulfilled')),
    rationale     text NOT NULL DEFAULT '',
    response      text NOT NULL DEFAULT '',
    created_by    uuid REFERENCES principals(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    responded_at  timestamptz
);

-- One open offer per task. Offering the same task to two sessions at once would produce
-- exactly the duplicated work this system exists to prevent.
CREATE UNIQUE INDEX task_assignments_one_open ON task_assignments (task_id)
    WHERE state IN ('offered','accepted');

CREATE INDEX task_assignments_inbox ON task_assignments (session_id, state, created_at DESC);
CREATE INDEX task_assignments_expiry ON task_assignments (expires_at) WHERE state = 'offered';
