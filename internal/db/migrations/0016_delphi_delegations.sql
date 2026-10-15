-- Delphi delegations (internal/delphi, docs/DELPHI.md): requests conductord submitted to a
-- provider batch API (Anthropic Message Batches, OpenAI Batch) on a member's behalf.
--
-- Only metadata is kept. The request body goes straight to the provider when it is
-- submitted and is never written here; the model's answer is fetched from the provider when
-- someone asks for it and is never written here either (DESIGN.md §12.4). body_sha256 lets a
-- resubmission under the same ref be recognised as the same request without storing it.
-- Each row is one provider batch of one request: the batch discount is per request, so
-- holding requests back to group them would only mean holding their bodies.
CREATE TABLE delphi_delegations (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id    uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id         uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    ref                text NOT NULL,
    principal_id       uuid REFERENCES principals(id) ON DELETE SET NULL,
    task_id            uuid REFERENCES tasks(id) ON DELETE SET NULL,
    provider           text NOT NULL CHECK (provider IN ('anthropic', 'openai')),
    model              text NOT NULL,
    body_sha256        text NOT NULL,
    label              text NOT NULL,
    batch_id           text NOT NULL DEFAULT '',
    state              text NOT NULL CHECK (state IN ('submitting', 'running', 'finished', 'abandoned')),
    status             text NOT NULL DEFAULT ''
                       CHECK (status IN ('', 'succeeded', 'refused', 'errored', 'expired', 'canceled', 'missing')),
    truncated          boolean NOT NULL DEFAULT false,
    stop_reason        text NOT NULL DEFAULT '',
    input_tokens       bigint NOT NULL DEFAULT 0,
    output_tokens      bigint NOT NULL DEFAULT 0,
    cache_read_tokens  bigint NOT NULL DEFAULT 0,
    cache_write_tokens bigint NOT NULL DEFAULT 0,
    cost_usd           numeric(14,6) NOT NULL DEFAULT 0,
    error_type         text NOT NULL DEFAULT '',
    error_detail       text NOT NULL DEFAULT '',
    refusal_category   text NOT NULL DEFAULT '',
    labels             jsonb NOT NULL DEFAULT '{}',
    submitted_at       timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, ref)
);
CREATE INDEX delphi_delegations_project ON delphi_delegations (project_id, submitted_at DESC);
CREATE INDEX delphi_delegations_open ON delphi_delegations (state) WHERE state IN ('submitting', 'running');

-- Delegated spend joins the token ledger as its own source.
ALTER TABLE usage_buckets
    DROP CONSTRAINT usage_buckets_source_check,
    ADD CONSTRAINT usage_buckets_source_check CHECK (source IN ('session', 'sync', 'attempt', 'delegation'));
