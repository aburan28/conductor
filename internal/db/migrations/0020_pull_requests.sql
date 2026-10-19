-- Pull requests as first-class objects (docs/COLLABORATION.md §4.5, phase A).
--
-- Until now a task carried exactly one pull request (0006: pull_request_url/_state), linked
-- only by its head branch, and nothing else about the pull request was kept. This table is
-- the source of truth for every pull request Conductor has seen on a linked repository:
-- which task it belongs to and how that was decided, where it stands (open, merged, closed),
-- and the rollup a reviewer wants at a glance — checks, review decision, unresolved threads,
-- mergeability, size.
--
-- Deliberately not stored: the body, review or comment text, commit messages, CI logs, the
-- diff. Those are data from strangers and the usual prompt-injection carriers; agents read
-- them on their own machine through gh/git. Conductor keeps counts and states, and the
-- title, which is what the repository already shows to everyone who can read it.
--
-- tasks.pull_request_url/_state stay as a mirror of the task's primary pull request, so the
-- CLI, the card, issue sync, and conflict messages keep working unchanged.
CREATE TABLE pull_requests (
    project_id          uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repository          text NOT NULL,              -- owner/repo, as GitHub spells it
    number              integer NOT NULL,
    url                 text NOT NULL,
    task_id             uuid REFERENCES tasks(id) ON DELETE SET NULL,
    -- How the task link was decided, in rising precedence: branch < body_ref < trailer <
    -- request < manual. A stronger source replaces a weaker one; a weaker never replaces a
    -- stronger. Empty when unlinked.
    link_source         text NOT NULL DEFAULT ''
                        CHECK (link_source IN ('', 'branch', 'body_ref', 'trailer', 'request', 'manual')),
    state               text NOT NULL CHECK (state IN ('open', 'merged', 'closed')),
    draft               boolean NOT NULL DEFAULT false,
    title               text NOT NULL DEFAULT '',   -- bounded by the writer (privacy.MaxPullRequestTitle)
    author_login        text NOT NULL DEFAULT '',
    author_id           bigint NOT NULL DEFAULT 0,  -- GitHub's numeric user id; external_identities.subject for the GitHub provider
    author_principal    uuid REFERENCES principals(id) ON DELETE SET NULL,
    head_ref            text NOT NULL DEFAULT '',
    head_sha            text NOT NULL DEFAULT '',
    head_repo           text NOT NULL DEFAULT '',
    base_ref            text NOT NULL DEFAULT '',
    from_fork           boolean NOT NULL DEFAULT false,
    checks_total        integer NOT NULL DEFAULT 0,
    checks_passed       integer NOT NULL DEFAULT 0,
    checks_failed       integer NOT NULL DEFAULT 0,
    checks_pending      integer NOT NULL DEFAULT 0,
    review_decision     text NOT NULL DEFAULT ''    -- '' | approved | changes_requested | review_required
                        CHECK (review_decision IN ('', 'approved', 'changes_requested', 'review_required')),
    unresolved_threads  integer NOT NULL DEFAULT 0,
    mergeable           text NOT NULL DEFAULT ''    -- '' | clean | dirty | blocked | behind | unknown
                        CHECK (mergeable IN ('', 'clean', 'dirty', 'blocked', 'behind', 'unknown')),
    additions           integer NOT NULL DEFAULT 0,
    deletions           integer NOT NULL DEFAULT 0,
    changed_files       integer NOT NULL DEFAULT 0,
    remote_updated_at   timestamptz,                -- GitHub's updated_at, for the poller's diff
    rollup_at           timestamptz,                -- when the checks/review rollup was last read
    last_seen_at        timestamptz NOT NULL DEFAULT now(),
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, repository, number)
);
CREATE INDEX pull_requests_open ON pull_requests (project_id) WHERE state = 'open';
CREATE INDEX pull_requests_task ON pull_requests (task_id);
CREATE INDEX pull_requests_head ON pull_requests (project_id, repository, head_sha);

-- The pull request a task's mirror columns follow: "owner/repo#N", or empty. The first
-- pull request linked to a task becomes primary, as does one linked by hand; when the
-- primary closes unmerged and another is still open, that one takes over.
ALTER TABLE tasks ADD COLUMN primary_pull_request text NOT NULL DEFAULT '';

-- Pull requests already linked under migration 0006 become rows, so the table is complete
-- from the first poll and a merge still completes the task it was waiting on. The URL is
-- the only thing the old columns kept; the next ingestion fills in the rest.
INSERT INTO pull_requests (project_id, repository, number, url, task_id, link_source, state)
SELECT t.project_id,
       substring(t.pull_request_url from '([^/]+/[^/]+)/pull/[0-9]+/?$'),
       substring(t.pull_request_url from '/pull/([0-9]+)/?$')::integer,
       t.pull_request_url, t.id, 'branch', t.pull_request_state
  FROM tasks t
 WHERE t.pull_request_state IN ('open', 'merged', 'closed')
   AND t.pull_request_url ~ '[^/]+/[^/]+/pull/[0-9]+/?$'
ON CONFLICT DO NOTHING;

UPDATE tasks t
   SET primary_pull_request = p.repository || '#' || p.number::text
  FROM pull_requests p
 WHERE p.task_id = t.id AND t.primary_pull_request = '';
