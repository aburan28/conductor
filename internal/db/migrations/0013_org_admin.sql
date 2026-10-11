-- Enterprise administration (DESIGN.md §25.8): organization-level policy and branding, SCIM
-- provisioning, and deactivated accounts.

-- One policy document per organization. The document is validated in Go
-- (internal/admin.Policy); keeping it as jsonb lets a setting be added without a migration,
-- and a missing key always means "the default". Settings a server's config file sets are
-- not stored here at all: they are applied over this document when it is read.
CREATE TABLE org_policies (
    organization_id uuid PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    policy          jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      uuid REFERENCES principals(id) ON DELETE SET NULL
);

-- Organizations that existed before this migration keep the dashboard they had: every
-- advanced area stays on. A new organization starts with the simple set (no row, so the
-- defaults), and an administrator turns areas on as the team needs them.
INSERT INTO org_policies (organization_id, policy)
SELECT id, '{"features": {"mesh": true, "swarm": true, "budget_sharing": true, "queue": true,
                          "local_models": true, "checkpoints_by_account": true}}'::jsonb
  FROM organizations;

-- The organization's logo: small, an image type the server sniffed itself, served with the
-- type recorded here and never as markup.
CREATE TABLE org_logos (
    organization_id uuid PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    content_type    text NOT NULL CHECK (content_type IN ('image/png', 'image/jpeg', 'image/gif')),
    data            bytea NOT NULL CHECK (octet_length(data) <= 65536),
    sha256          text NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- A deactivated principal authenticates nowhere: its tokens are revoked when it is
-- deactivated, and token authentication refuses it regardless. Its rows (memberships,
-- tasks, audit history) stay, so reactivating restores access and the record stays whole.
ALTER TABLE principals
    ADD COLUMN deactivated_at   timestamptz,
    -- SCIM (RFC 7643): the identity provider's userName and externalId for the account, set
    -- once the provider manages it. scim_deleted_at hides a principal the provider deleted
    -- from SCIM, without deleting the row the audit log refers to.
    ADD COLUMN scim_user_name   text,
    ADD COLUMN scim_external_id text,
    ADD COLUMN scim_deleted_at  timestamptz;

CREATE UNIQUE INDEX principals_scim_user_name
    ON principals (organization_id, lower(scim_user_name))
    WHERE scim_user_name IS NOT NULL AND scim_deleted_at IS NULL;

-- Bearer tokens for an identity provider's SCIM client. Organization-scoped, minted by an
-- org_admin, stored only as a hash, and usable on /scim/v2 and nowhere else.
CREATE TABLE scim_tokens (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            text NOT NULL,
    token_hash      bytea NOT NULL UNIQUE,
    created_by      uuid REFERENCES principals(id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_used_at    timestamptz,
    revoked_at      timestamptz
);

-- SCIM groups. Their only effect is the group → role mapping applied at sign-in: a
-- principal's groups are those the provider pushed here plus those its ID token carries.
CREATE TABLE scim_groups (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    display_name    text NOT NULL,
    external_id     text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX scim_groups_name ON scim_groups (organization_id, lower(display_name));

CREATE TABLE scim_group_members (
    group_id     uuid NOT NULL REFERENCES scim_groups(id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, principal_id)
);

-- The audit viewer filters by action and actor within an organization.
CREATE INDEX audit_log_action ON audit_log (organization_id, action, created_at DESC);
CREATE INDEX audit_log_actor ON audit_log (organization_id, actor_principal, created_at DESC);
