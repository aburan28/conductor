-- Token usage ledger (DESIGN.md §26.1: token/cost by role, model, project, and sponsor).
--
-- One row is one harness session, one model, one hour. Counters are absolute for that hour,
-- not deltas: a collector that re-reads a whole transcript reproduces the same rows, and an
-- upsert replaces rather than adds, so nothing is ever counted twice. Rows come from three
-- places — a `conductor wrap` sidecar reading the harness's own usage log (session), a
-- `conductor usage sync` run over a machine's logs for bare sessions (sync), and runner
-- attempts (attempt). As everywhere else, this is metadata only: numbers, a model name, a
-- timestamp. No prompt, completion, or tool payload has a column to land in.

CREATE TABLE usage_buckets (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id          uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    principal_id        uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    session_id          uuid REFERENCES sessions(id) ON DELETE SET NULL,
    attempt_id          uuid REFERENCES attempts(id) ON DELETE SET NULL,
    source              text NOT NULL CHECK (source IN ('session','sync','attempt')),
    harness             text NOT NULL,
    model               text NOT NULL DEFAULT '',
    provider            text NOT NULL DEFAULT '',
    reasoning_effort    text NOT NULL DEFAULT '',
    external_session_id text NOT NULL,
    bucket_start        timestamptz NOT NULL,
    requests            bigint NOT NULL DEFAULT 0,
    input_tokens        bigint NOT NULL DEFAULT 0,
    cache_read_tokens   bigint NOT NULL DEFAULT 0,
    cache_write_tokens  bigint NOT NULL DEFAULT 0,
    output_tokens       bigint NOT NULL DEFAULT 0,
    reasoning_tokens    bigint NOT NULL DEFAULT 0,
    cost_usd            numeric(14,6) NOT NULL DEFAULT 0,
    -- 'reported' when the harness priced the call itself, 'catalog' when Conductor estimated
    -- it from the organization's model catalog, '' when neither could.
    cost_source         text NOT NULL DEFAULT '' CHECK (cost_source IN ('','reported','catalog')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, principal_id, harness, external_session_id, model, provider,
            reasoning_effort, bucket_start)
);

-- Every report is "this project, this window": index by recency within a project.
CREATE INDEX usage_buckets_window ON usage_buckets (project_id, bucket_start DESC);
