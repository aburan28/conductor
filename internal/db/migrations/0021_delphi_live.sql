-- Live delegations (docs/DELPHI.md, "OpenAI-compatible endpoints on the control plane"):
-- requests to an operator-configured OpenAI-compatible endpoint (OpenRouter, Fireworks,
-- DeepSeek, vLLM, ...) that answers once and keeps nothing. conductord's live worker makes
-- the call in the background so no API request is held open.
--
-- DESIGN.md §12.4 is unchanged: neither the prompt nor the answer is written here. The
-- prompt lives in the worker's memory until the call is made, and the answer in the memory
-- of the process that made it (holder) until fetched or expired. A restart loses both, and
-- the loop marks the row missing (error_type 'lost') once its holder stops heartbeating.
ALTER TABLE delphi_delegations
    DROP CONSTRAINT delphi_delegations_provider_check,
    ADD COLUMN live boolean NOT NULL DEFAULT false,
    ADD COLUMN holder text NOT NULL DEFAULT '',
    -- Batch rows still name a batch API; only live rows name an operator's endpoint.
    ADD CONSTRAINT delphi_delegations_provider_check CHECK (
        provider IN ('anthropic', 'openai') OR (live AND provider ~ '^[a-z0-9][a-z0-9_-]{0,62}$'));
CREATE INDEX delphi_delegations_live_open ON delphi_delegations (updated_at) WHERE live AND state IN ('submitting', 'running');
