-- Gemini's Batch Mode (internal/delphi/gemini) joins Anthropic and OpenAI as a batch API a
-- hosted delegation may name. Live rows keep naming an operator's endpoint (0021).
ALTER TABLE delphi_delegations
    DROP CONSTRAINT delphi_delegations_provider_check,
    ADD CONSTRAINT delphi_delegations_provider_check CHECK (
        provider IN ('anthropic', 'openai', 'gemini') OR (live AND provider ~ '^[a-z0-9][a-z0-9_-]{0,62}$'));
