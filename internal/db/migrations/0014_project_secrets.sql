-- Project secrets: small credentials sealed at rest under conductord's secretbox key
-- (internal/secretbox). Agents and local tools retrieve them via the API / MCP; values never
-- appear in task metadata, SSE, or list responses — only a short hint is kept in the clear.

CREATE TABLE project_secrets (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name            text NOT NULL,
    value_sealed    text NOT NULL,
    hint            text NOT NULL DEFAULT '',
    description     text NOT NULL DEFAULT '',
    created_by      uuid REFERENCES principals(id) ON DELETE SET NULL,
    updated_by      uuid REFERENCES principals(id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

CREATE INDEX project_secrets_project ON project_secrets (project_id);
