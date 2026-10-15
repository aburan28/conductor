-- Inference endpoints and the models they report serving.
--
-- An endpoint is an API a project routes to (Anthropic, OpenAI, a local Ollama or vLLM, a
-- gateway). Its key is not stored here: api_key_secret names a project secret, which is sealed
-- by the vault. discovered_models is what the endpoint last said it serves; rows are marked
-- missing rather than deleted so appearances and retirements stay on record.
--
-- Model profiles may carry a selector instead of a fixed model id. Discovery resolves the
-- selector against the project's discovered models and writes the winner into model_profiles.model,
-- so everything that reads the catalog (the router, the selector, usage) is unchanged.

CREATE TABLE inference_endpoints (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id       uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name             text NOT NULL,
    kind             text NOT NULL CHECK (kind IN ('anthropic','openai','openai_compatible','ollama')),
    base_url         text NOT NULL,
    api_key_secret   text NOT NULL DEFAULT '',
    enabled          boolean NOT NULL DEFAULT true,
    last_status      text NOT NULL DEFAULT 'never' CHECK (last_status IN ('never','ok','error')),
    last_error       text NOT NULL DEFAULT '',
    last_refresh_at  timestamptz,
    created_by       uuid REFERENCES principals(id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

CREATE INDEX inference_endpoints_project ON inference_endpoints (project_id);

CREATE TABLE discovered_models (
    endpoint_id       uuid NOT NULL REFERENCES inference_endpoints(id) ON DELETE CASCADE,
    model             text NOT NULL,
    display_name      text NOT NULL DEFAULT '',
    created_upstream  timestamptz,
    context_window    integer NOT NULL DEFAULT 0,
    first_seen_at     timestamptz NOT NULL DEFAULT now(),
    last_seen_at      timestamptz NOT NULL DEFAULT now(),
    missing_since     timestamptz,
    PRIMARY KEY (endpoint_id, model)
);

ALTER TABLE model_profiles
    ADD COLUMN select_project_id uuid REFERENCES projects(id) ON DELETE SET NULL,
    ADD COLUMN select_endpoint   text   NOT NULL DEFAULT '',
    ADD COLUMN select_match      text[] NOT NULL DEFAULT '{}',
    ADD COLUMN select_exclude    text[] NOT NULL DEFAULT '{}',
    ADD COLUMN select_pick       text   NOT NULL DEFAULT '',
    ADD COLUMN select_prefix     text   NOT NULL DEFAULT '',
    ADD COLUMN resolved_at       timestamptz,
    -- A person chose this model (dashboard or CLI). Discovery and bootstrap leave it alone
    -- until it is unpinned.
    ADD COLUMN pinned            boolean NOT NULL DEFAULT false;
