-- A current, unverified local email hint per owner's machine, harness, and profile slot.
-- Snapshots and alerts keep only opaque account keys. A new account in the same slot
-- replaces the hint, so historical readings cannot inherit the new account's email.
CREATE TABLE quota_identity_hints (
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    machine text NOT NULL,
    harness text NOT NULL CHECK (harness IN ('claude', 'codex')),
    slot text NOT NULL,
    account text NOT NULL,
    email text NOT NULL DEFAULT '',
    observed_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, machine, harness, slot)
);
