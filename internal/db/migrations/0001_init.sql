-- Conductor initial schema. Implements DESIGN.md §23 (persistence model).
--
-- Invariants that live here rather than in Go, because they must hold even if application
-- code is wrong:
--   * at most one active lease per task              (one_active_lease_per_task)
--   * at most one active attempt per task            (one_active_write_attempt_per_task)
--   * fencing epochs are monotonic per task          (tasks.fencing_epoch + guarded updates)
--   * event sequence numbers are gapless per aggregate (domain_events unique constraint)
--   * no column anywhere holds a prompt or transcript (see docs/PRIVACY.md)

CREATE TABLE schema_migrations (
    version     text PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Tenancy and identity
-- ---------------------------------------------------------------------------

CREATE TABLE organizations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        text NOT NULL UNIQUE,
    name        text NOT NULL,
    -- Per-tenant HMAC key for intent fingerprints (DESIGN.md §12.2, §25.6). Never leaves
    -- the control plane; rotating it invalidates historical fingerprints by design.
    dedupe_key  bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE principals (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    kind            text NOT NULL CHECK (kind IN (
                        'human','interactive_session','agent_attempt',
                        'runner_service','control_plane_service','review_bot')),
    handle          text NOT NULL,
    display_name    text NOT NULL DEFAULT '',
    email           text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, handle)
);

CREATE TABLE projects (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id   uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    slug              text NOT NULL,
    display_name      text NOT NULL,
    canonical_remote  text NOT NULL DEFAULT '',
    default_branch    text NOT NULL DEFAULT 'main',
    worktree_root     text NOT NULL DEFAULT '.conductor/runtime/worktrees',
    repo_path         text NOT NULL DEFAULT '',
    config            jsonb NOT NULL DEFAULT '{}'::jsonb,
    config_sha        text NOT NULL DEFAULT '',
    workflow_sha      text NOT NULL DEFAULT '',
    task_seq          bigint NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, slug)
);

CREATE TABLE project_memberships (
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    role         text NOT NULL CHECK (role IN (
                     'org_admin','project_admin','maintainer','contributor',
                     'reviewer','observer','runner')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, principal_id)
);

CREATE TABLE api_tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id  uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    name          text NOT NULL DEFAULT '',
    -- SHA-256 of the bearer token. The plaintext is shown once, at creation, and never
    -- stored or logged (DESIGN.md §25.1).
    token_hash    bytea NOT NULL UNIQUE,
    expires_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    revoked_at    timestamptz
);

-- ---------------------------------------------------------------------------
-- Execution topology
-- ---------------------------------------------------------------------------

CREATE TABLE runners (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id      uuid REFERENCES projects(id) ON DELETE CASCADE,
    principal_id    uuid REFERENCES principals(id) ON DELETE SET NULL,
    name            text NOT NULL,
    capabilities    jsonb NOT NULL DEFAULT '{}'::jsonb,
    max_concurrency integer NOT NULL DEFAULT 1,
    in_flight       integer NOT NULL DEFAULT 0,
    state           text NOT NULL DEFAULT 'online'
                        CHECK (state IN ('online','draining','offline')),
    registered_at   timestamptz NOT NULL DEFAULT now(),
    heartbeat_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, name)
);

CREATE TABLE sessions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    principal_id    uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    runner_id       uuid REFERENCES runners(id) ON DELETE SET NULL,
    harness         text NOT NULL,
    harness_version text NOT NULL DEFAULT '',
    machine_id      text NOT NULL DEFAULT '',
    base_sha        text NOT NULL DEFAULT '',
    branch          text NOT NULL DEFAULT '',
    worktree_path   text NOT NULL DEFAULT '',
    visibility      text NOT NULL DEFAULT 'team_summary'
                        CHECK (visibility IN ('private','team_summary','team_artifacts','shared_debug')),
    active_task_id  uuid,
    state           text NOT NULL DEFAULT 'online_idle' CHECK (state IN (
                        'online_idle','planning','working','waiting_for_input',
                        'blocked','reviewing','offline_grace','stale','closed')),
    started_at      timestamptz NOT NULL DEFAULT now(),
    heartbeat_at    timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    closed_at       timestamptz
);

CREATE INDEX sessions_live ON sessions (project_id, heartbeat_at DESC) WHERE closed_at IS NULL;

CREATE TABLE model_profiles (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id       uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    alias                 text NOT NULL,
    harness               text NOT NULL,
    provider              text NOT NULL DEFAULT '',
    model                 text NOT NULL DEFAULT '',
    reasoning_effort      text NOT NULL DEFAULT 'medium'
                              CHECK (reasoning_effort IN ('none','low','medium','high','max')),
    capabilities          text[] NOT NULL DEFAULT '{}',
    tier                  text NOT NULL DEFAULT 'T2' CHECK (tier IN ('T0','T1','T2','T3','T4')),
    context_window        integer NOT NULL DEFAULT 0,
    input_cost_per_mtok   numeric(12,4),
    output_cost_per_mtok  numeric(12,4),
    billing               text NOT NULL DEFAULT 'tokens' CHECK (billing IN ('tokens','capacity')),
    enabled               boolean NOT NULL DEFAULT true,
    catalog_version       text NOT NULL DEFAULT '',
    UNIQUE (organization_id, alias, harness)
);

CREATE TABLE workflows (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    sha          text NOT NULL,
    content      text NOT NULL,
    frontmatter  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, sha)
);

-- ---------------------------------------------------------------------------
-- Task ledger
-- ---------------------------------------------------------------------------

CREATE TABLE tasks (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id     uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id          uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    ref                 text NOT NULL,                 -- human-facing, e.g. T-42
    parent_id           uuid REFERENCES tasks(id) ON DELETE SET NULL,
    external_ref        text,
    title               text NOT NULL DEFAULT '',
    objective           text NOT NULL DEFAULT '',
    acceptance_criteria jsonb NOT NULL DEFAULT '[]'::jsonb,
    status              text NOT NULL CHECK (status IN (
                            'proposed','ready','claimed','running',
                            'blocked_dependency','blocked_conflict','blocked_input',
                            'verifying','review_required','merging',
                            'done','failed','cancelled','superseded')),
    visibility          text NOT NULL DEFAULT 'team_summary'
                            CHECK (visibility IN ('private','team_summary','team_artifacts','shared_debug')),
    priority            integer NOT NULL DEFAULT 0,
    risk_level          text NOT NULL DEFAULT 'unknown'
                            CHECK (risk_level IN ('unknown','low','medium','high','critical')),
    base_sha            text NOT NULL DEFAULT '',
    workflow_sha        text NOT NULL DEFAULT '',
    active_lease_id     uuid,
    -- Monotonic per task. Bumped on every claim and every reclamation, so a worker holding
    -- a stale epoch can never mutate canonical state (DESIGN.md §10.3).
    fencing_epoch       bigint NOT NULL DEFAULT 0,
    -- Coordination metadata only. HMAC of the canonicalized intent, plus a MinHash
    -- signature over HMAC'd shingles for similarity without plaintext (DESIGN.md §12.2).
    intent_fingerprint  text,
    intent_minhash      bigint[],
    model_alias         text NOT NULL DEFAULT '',
    harness_pref        text NOT NULL DEFAULT '',
    budget              jsonb NOT NULL DEFAULT '{}'::jsonb,
    features            jsonb NOT NULL DEFAULT '{}'::jsonb,
    attempts_count      integer NOT NULL DEFAULT 0,
    max_attempts        integer NOT NULL DEFAULT 4,
    superseded_by       uuid REFERENCES tasks(id) ON DELETE SET NULL,
    created_by          uuid NOT NULL REFERENCES principals(id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    completed_at        timestamptz,
    UNIQUE (project_id, ref)
);

CREATE UNIQUE INDEX tasks_external_ref_unique
    ON tasks (project_id, external_ref)
    WHERE external_ref IS NOT NULL
      AND status NOT IN ('cancelled','superseded');

CREATE INDEX tasks_dispatch ON tasks (project_id, status, priority DESC, created_at);
CREATE INDEX tasks_fingerprint ON tasks (project_id, intent_fingerprint)
    WHERE intent_fingerprint IS NOT NULL;

-- Private detail for `private` visibility tasks. Encrypted at rest with the org key; the
-- owning principal is the only reader. Split into its own table so the hot task row can be
-- selected freely without risk of leaking the payload through a SELECT *.
CREATE TABLE task_private_fields (
    task_id           uuid PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    owner_principal   uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    detail_ciphertext bytea NOT NULL,
    nonce             bytea NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE task_dependencies (
    task_id       uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    depends_on_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, depends_on_id),
    CHECK (task_id <> depends_on_id)
);

CREATE INDEX task_dependencies_reverse ON task_dependencies (depends_on_id);

CREATE TABLE attempts (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id            uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    project_id         uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    attempt_number     integer NOT NULL,
    sponsor_principal  uuid NOT NULL REFERENCES principals(id),
    executor_principal uuid REFERENCES principals(id),
    runner_id          uuid REFERENCES runners(id) ON DELETE SET NULL,
    session_id         uuid REFERENCES sessions(id) ON DELETE SET NULL,
    role               text NOT NULL DEFAULT 'implementer' CHECK (role IN (
                           'planner','implementer','verifier','reviewer','researcher')),
    harness            text NOT NULL,
    model_profile_id   uuid REFERENCES model_profiles(id),
    model_alias        text NOT NULL DEFAULT '',
    resolved_model     text NOT NULL DEFAULT '',
    reasoning_effort   text NOT NULL DEFAULT 'medium'
                           CHECK (reasoning_effort IN ('none','low','medium','high','max')),
    state              text NOT NULL CHECK (state IN (
                           'queued','preparing_workspace','starting_harness','running',
                           'waiting_for_approval','waiting_for_input','paused_conflict',
                           'succeeded','failed_retryable','failed_terminal','cancelled','stale')),
    branch             text NOT NULL DEFAULT '',
    worktree_path      text NOT NULL DEFAULT '',
    base_commit_sha    text NOT NULL DEFAULT '',
    commit_sha         text NOT NULL DEFAULT '',
    -- Actual paths touched, harvested from git by the runner. Feeds the merge-risk graph
    -- (DESIGN.md §11.6) and scope-drift detection (§8.6).
    changed_paths      text[] NOT NULL DEFAULT '{}',
    -- Usage metadata only. No prompts, completions, tool arguments, or tool results.
    tokens_in          bigint NOT NULL DEFAULT 0,
    tokens_out         bigint NOT NULL DEFAULT 0,
    cost_usd           numeric(12,4) NOT NULL DEFAULT 0,
    turns              integer NOT NULL DEFAULT 0,
    workflow_sha       text NOT NULL DEFAULT '',
    project_config_sha text NOT NULL DEFAULT '',
    router_policy_ver  text NOT NULL DEFAULT '',
    model_catalog_ver  text NOT NULL DEFAULT '',
    failure_class      text,
    failure_summary    text,
    started_at         timestamptz,
    ended_at           timestamptz,
    last_event_at      timestamptz NOT NULL DEFAULT now(),
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_id, attempt_number)
);

-- At most one live write attempt per task. Read-only reviewer attempts are excluded, which
-- is what lets a reviewer run concurrently with an implementer (DESIGN.md §9.2).
CREATE UNIQUE INDEX one_active_write_attempt_per_task
    ON attempts (task_id)
    WHERE state IN ('queued','preparing_workspace','starting_harness','running',
                    'waiting_for_approval','waiting_for_input','paused_conflict')
      AND role <> 'reviewer';

CREATE INDEX attempts_liveness ON attempts (state, last_event_at);

CREATE TABLE leases (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id          uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempt_id       uuid NOT NULL REFERENCES attempts(id) ON DELETE CASCADE,
    project_id       uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    fencing_epoch    bigint NOT NULL,
    holder_principal uuid NOT NULL REFERENCES principals(id),
    session_id       uuid REFERENCES sessions(id) ON DELETE SET NULL,
    acquired_at      timestamptz NOT NULL DEFAULT now(),
    heartbeat_at     timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    released_at      timestamptz,
    release_reason   text
);

CREATE UNIQUE INDEX one_active_lease_per_task
    ON leases (task_id)
    WHERE released_at IS NULL;

CREATE INDEX leases_expiry ON leases (expires_at) WHERE released_at IS NULL;

ALTER TABLE tasks
    ADD CONSTRAINT tasks_active_lease_fk
    FOREIGN KEY (active_lease_id) REFERENCES leases(id) ON DELETE SET NULL;

ALTER TABLE sessions
    ADD CONSTRAINT sessions_active_task_fk
    FOREIGN KEY (active_task_id) REFERENCES tasks(id) ON DELETE SET NULL;

-- ---------------------------------------------------------------------------
-- Scope reservations (DESIGN.md §11)
-- ---------------------------------------------------------------------------

CREATE TABLE scope_reservations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id       uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempt_id    uuid REFERENCES attempts(id) ON DELETE SET NULL,
    lease_id      uuid REFERENCES leases(id) ON DELETE SET NULL,
    principal_id  uuid NOT NULL REFERENCES principals(id),
    resource_type text NOT NULL CHECK (resource_type IN (
                      'repo','path','dir','lockfile','migration','schema','api','symbol','test')),
    resource_key  text NOT NULL,
    mode          text NOT NULL CHECK (mode IN (
                      'read_shared','write_exclusive','review_shared',
                      'speculative_write','protected_exclusive')),
    source        text NOT NULL DEFAULT 'declared'
                      CHECK (source IN ('declared','planned','observed','policy')),
    alternative_group text,
    active        boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    released_at   timestamptz
);

CREATE INDEX active_scope_lookup
    ON scope_reservations (project_id, resource_type, resource_key)
    WHERE active = true;

CREATE INDEX active_scope_by_task
    ON scope_reservations (task_id) WHERE active = true;

-- ---------------------------------------------------------------------------
-- Evidence and artifacts (DESIGN.md §15)
-- ---------------------------------------------------------------------------

CREATE TABLE artifacts (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id        uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempt_id     uuid REFERENCES attempts(id) ON DELETE SET NULL,
    kind           text NOT NULL CHECK (kind IN (
                       'commit','patch','diff','test_log','report','pr','plan','review')),
    uri            text NOT NULL DEFAULT '',
    content_sha256 text NOT NULL DEFAULT '',
    size_bytes     bigint NOT NULL DEFAULT 0,
    visibility     text NOT NULL DEFAULT 'team_artifacts'
                       CHECK (visibility IN ('private','team_summary','team_artifacts','shared_debug')),
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE validation_results (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    attempt_id  uuid NOT NULL REFERENCES attempts(id) ON DELETE CASCADE,
    task_id     uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    command_id  text NOT NULL,
    command     text NOT NULL,
    exit_code   integer NOT NULL,
    duration_ms bigint NOT NULL DEFAULT 0,
    artifact_id uuid REFERENCES artifacts(id) ON DELETE SET NULL,
    -- Runner-attested, not model-claimed (DESIGN.md §25.5).
    runner_id   uuid REFERENCES runners(id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX validation_by_attempt ON validation_results (attempt_id);

CREATE TABLE review_findings (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id            uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempt_id         uuid REFERENCES attempts(id) ON DELETE SET NULL,
    reviewer_principal uuid REFERENCES principals(id),
    severity           text NOT NULL CHECK (severity IN ('info','low','medium','high','critical')),
    category           text NOT NULL DEFAULT '',
    path               text NOT NULL DEFAULT '',
    line               integer,
    summary            text NOT NULL,
    resolved           boolean NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE handoffs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id         uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    from_attempt_id uuid REFERENCES attempts(id) ON DELETE SET NULL,
    to_attempt_id   uuid REFERENCES attempts(id) ON DELETE SET NULL,
    to_harness      text NOT NULL DEFAULT '',
    to_role         text NOT NULL DEFAULT '',
    visibility      text NOT NULL DEFAULT 'team_summary'
                        CHECK (visibility IN ('private','team_summary','team_artifacts','shared_debug')),
    -- Structured continuation state. Explicitly NOT a transcript (DESIGN.md §22).
    bundle          jsonb NOT NULL,
    created_by      uuid NOT NULL REFERENCES principals(id),
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE policy_decisions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id    uuid REFERENCES tasks(id) ON DELETE CASCADE,
    attempt_id uuid REFERENCES attempts(id) ON DELETE SET NULL,
    kind       text NOT NULL,          -- route | conflict | escalation | budget | admission
    decision   text NOT NULL,
    rationale  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX policy_decisions_by_task ON policy_decisions (task_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- Coordination: intents and conflicts (DESIGN.md §11.6, §12)
-- ---------------------------------------------------------------------------

CREATE TABLE intents (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    session_id     uuid REFERENCES sessions(id) ON DELETE CASCADE,
    principal_id   uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    task_id        uuid REFERENCES tasks(id) ON DELETE SET NULL,
    external_ref   text,
    visibility     text NOT NULL DEFAULT 'team_summary'
                       CHECK (visibility IN ('private','team_summary','team_artifacts','shared_debug')),
    -- Sanitized, caller-supplied. Never derived from a prompt by the server.
    public_summary text NOT NULL DEFAULT '',
    verb           text NOT NULL DEFAULT '',
    resource       text NOT NULL DEFAULT '',
    fingerprint    text NOT NULL,
    minhash        bigint[] NOT NULL DEFAULT '{}',
    scopes         jsonb NOT NULL DEFAULT '[]'::jsonb,
    outcome        text NOT NULL DEFAULT 'allow',
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL
);

CREATE INDEX intents_live ON intents (project_id, expires_at DESC);
CREATE INDEX intents_fingerprint ON intents (project_id, fingerprint);

CREATE TABLE conflict_edges (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_a          uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    task_b          uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind            text NOT NULL CHECK (kind IN (
                        'scope_overlap','duplicate_intent','merge_risk',
                        'dependency_cycle','protected_resource')),
    severity        text NOT NULL CHECK (severity IN ('info','low','medium','high','critical')),
    weight          double precision NOT NULL DEFAULT 0,
    suggestion      text NOT NULL DEFAULT 'allow' CHECK (suggestion IN (
                        'allow','allow_with_warning','block_duplicate','block_conflict',
                        'suggest_join','suggest_split','suggest_wait','suggest_supersede')),
    detail          jsonb NOT NULL DEFAULT '{}'::jsonb,
    state           text NOT NULL DEFAULT 'open'
                        CHECK (state IN ('open','acknowledged','resolved','ignored')),
    resolution_note text,
    resolved_by     uuid REFERENCES principals(id),
    detected_at     timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    resolved_at     timestamptz,
    CHECK (task_a < task_b)          -- canonical ordering makes the pair index meaningful
);

CREATE UNIQUE INDEX conflict_edge_unique
    ON conflict_edges (project_id, task_a, task_b, kind)
    WHERE state IN ('open','acknowledged');

CREATE INDEX conflict_edges_open ON conflict_edges (project_id, state, severity);

-- ---------------------------------------------------------------------------
-- Events, outbox, idempotency, audit (DESIGN.md §23.3)
-- ---------------------------------------------------------------------------

CREATE TABLE domain_events (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    aggregate_type  text NOT NULL,
    aggregate_id    uuid NOT NULL,
    sequence_number bigint NOT NULL,
    event_type      text NOT NULL,
    actor_principal uuid REFERENCES principals(id),
    visibility      text NOT NULL DEFAULT 'team_summary'
                        CHECK (visibility IN ('private','team_summary','team_artifacts','shared_debug')),
    payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (aggregate_type, aggregate_id, sequence_number)
);

CREATE INDEX domain_events_stream ON domain_events (project_id, occurred_at DESC, id);

CREATE TABLE outbox_events (
    id           bigserial PRIMARY KEY,
    event_id     uuid NOT NULL REFERENCES domain_events(id) ON DELETE CASCADE,
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    delivered_at timestamptz,
    attempts     integer NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX outbox_pending ON outbox_events (id) WHERE delivered_at IS NULL;

CREATE TABLE idempotency_keys (
    key          text NOT NULL,
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    request_hash text NOT NULL,
    status_code  integer NOT NULL,
    response     jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (key, principal_id)
);

CREATE TABLE audit_log (
    id              bigserial PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id      uuid REFERENCES projects(id) ON DELETE CASCADE,
    actor_principal uuid REFERENCES principals(id) ON DELETE SET NULL,
    action          text NOT NULL,
    target_type     text NOT NULL DEFAULT '',
    target_id       text NOT NULL DEFAULT '',
    detail          jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_recent ON audit_log (organization_id, created_at DESC);
