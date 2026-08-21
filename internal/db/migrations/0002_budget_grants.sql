-- Token budget sharing (DESIGN.md §13.8).
--
-- A grant moves part of one member's per-window token allowance to a teammate. The table
-- is an append-only ledger: rows are never updated or deleted, so any member's balance is
-- fully explained by policy allowance + grants in - grants out - recorded spend. Note the
-- absence of any free-text beyond a clamped note — grants carry amounts and identities,
-- never work content (docs/PRIVACY.md).

CREATE TABLE budget_grants (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    from_principal uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    to_principal   uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    tokens         bigint NOT NULL CHECK (tokens > 0),
    note           text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (from_principal <> to_principal)
);

-- Balance math always scopes to one project and a trailing window, so index by recency.
CREATE INDEX budget_grants_window ON budget_grants (project_id, created_at DESC);
