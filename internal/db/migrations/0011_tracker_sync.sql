-- Issue tracker sync (docs/DESIGN.md §17.5): issues on a team's tracker become Conductor tasks,
-- and a task's claim and completion are written back to its issue.
--
-- tracker_configs is the opt-in, one row per project and tracker. It also carries the
-- import's resume point: the newest issue update already read (cursor) and the ETag GitHub
-- answered the last listing from that point with, so an unchanged repository costs one
-- conditional request that does not count against the rate limit.
--
-- tracker_links maps an issue to the task imported from it. The primary key is what makes
-- an import idempotent whatever the task's status: a webhook and the poller seeing the same
-- issue, two replicas, or a redelivery all find the same row. (tasks_external_ref_unique
-- deliberately ignores cancelled tasks, so a person may file a fresh task for a key whose
-- old one was cancelled; a synced issue keeps its one task, which an issue reopened brings
-- back.) No issue text is stored here: the task carries the mapped title and objective
-- exactly as it would for a task filed by hand, and the link keeps only hashes of what was
-- last read from the issue, which is how a Conductor-side edit is told apart from an issue
-- edit (tracker.Plan).

CREATE TABLE tracker_configs (
    project_id        uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    tracker           text NOT NULL CHECK (tracker IN ('github')),
    enabled           boolean NOT NULL DEFAULT true,
    -- Only issues carrying this label are imported; '' imports every open issue.
    label             text NOT NULL DEFAULT 'conductor',
    -- Added to an issue while its task is being worked; '' writes no label.
    in_progress_label text NOT NULL DEFAULT 'in-progress',
    -- The visibility of a task imported from a public repository, whose issue anyone can
    -- read already. Issues of a private repository take the project's default visibility.
    public_visibility text NOT NULL DEFAULT 'team_artifacts'
                          CHECK (public_visibility IN ('private','team_summary','team_artifacts','shared_debug')),
    -- Imported tasks are created by whoever enabled the sync.
    enabled_by        uuid NOT NULL REFERENCES principals(id),
    installation_id   bigint NOT NULL DEFAULT 0,
    cursor            timestamptz,
    etag              text NOT NULL DEFAULT '',
    last_sync_at      timestamptz,
    last_error        text NOT NULL DEFAULT '',
    -- Why the write-back is failing (an installation without issues: write, say), apart from
    -- the import's last_error: the two fail and recover independently.
    writeback_error   text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, tracker)
);

CREATE TABLE tracker_links (
    project_id        uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    external_ref      text NOT NULL,            -- e.g. github:acme/widgets#12
    tracker           text NOT NULL,
    task_id           uuid NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE CASCADE,
    url               text NOT NULL DEFAULT '',
    remote_state      text NOT NULL DEFAULT 'open' CHECK (remote_state IN ('open','closed')),
    remote_public     boolean NOT NULL DEFAULT false,
    remote_updated_at timestamptz,
    synced_title_hash text NOT NULL DEFAULT '',
    synced_body_hash  text NOT NULL DEFAULT '',
    -- When the task's title, objective, or acceptance criteria last changed (the trigger
    -- below): the Conductor side of the last-writer-wins rule.
    task_edited_at    timestamptz,
    -- The task was cancelled because its issue closed, so reopening the issue may revive it.
    cancelled_by_sync boolean NOT NULL DEFAULT false,
    -- Write-back memory: what the issue has already been told.
    claim_noted_for   uuid REFERENCES principals(id) ON DELETE SET NULL,
    -- The in-progress label the write-back put on the issue ('' none), by name, so it is the
    -- one removed even if the project's label setting changed since.
    label_applied     text NOT NULL DEFAULT '',
    done_noted        boolean NOT NULL DEFAULT false,
    -- The task's updated_at the write-back last reconciled; a task changed since is due.
    written_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, external_ref)
);

CREATE INDEX tracker_links_by_tracker ON tracker_links (tracker, project_id);

-- A content edit made in Conductor, by any path (the API, MCP, the dashboard), is stamped
-- on the task's link. updated_at cannot serve: it moves on every claim and status change,
-- and an issue edit must not lose to a task that was merely claimed after it.
CREATE FUNCTION tracker_note_task_edit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE tracker_links SET task_edited_at = now() WHERE task_id = NEW.id;
    RETURN NULL;
END
$$;

CREATE TRIGGER tasks_tracker_edit
    AFTER UPDATE OF title, objective, acceptance_criteria ON tasks
    FOR EACH ROW
    WHEN (OLD.title IS DISTINCT FROM NEW.title
       OR OLD.objective IS DISTINCT FROM NEW.objective
       OR OLD.acceptance_criteria IS DISTINCT FROM NEW.acceptance_criteria)
    EXECUTE FUNCTION tracker_note_task_edit();
