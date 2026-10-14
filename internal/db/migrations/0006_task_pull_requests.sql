-- The pull request a task's work travels in.
--
-- A task's last stretch happens on GitHub, not in Conductor: the work sits in a pull request
-- until it merges. Recording the link is what lets a merge complete the task (and release its
-- territory) and lets `task show` and the dashboard point at the review. The URL is the
-- repository's own address for the pull request, which everyone who can read the repository
-- can already see; no title, body, or diff is stored.
--
-- pull_request_state follows GitHub: 'open' while it can still merge, 'merged' once it has,
-- 'closed' when it was closed without merging. Empty means no pull request is known.
ALTER TABLE tasks
    ADD COLUMN pull_request_url   text NOT NULL DEFAULT '',
    ADD COLUMN pull_request_state text NOT NULL DEFAULT ''
        CHECK (pull_request_state IN ('', 'open', 'merged', 'closed'));

-- The poller asks "which of this project's tasks have a pull request still open?" on every
-- pass; without an index that is a scan of every task the project ever had.
CREATE INDEX tasks_open_pull_requests ON tasks (project_id)
    WHERE pull_request_state = 'open';
