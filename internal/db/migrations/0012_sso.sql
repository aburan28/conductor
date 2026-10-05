-- Single sign-on (DESIGN.md §25.7): accounts at an external identity provider linked to
-- principals, and the short-lived state of sign-ins in flight.

-- An external account, named by its issuer and subject — never by email, which can be
-- reassigned or, at some providers, changed by the user at will. The first unique key makes
-- one external account belong to at most one principal; the second gives a principal at most
-- one account per issuer, so a second account at the same provider can never be attached to
-- a principal that already signs in with one.
CREATE TABLE external_identities (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id  uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    -- The configured provider name the identity was linked through (its tokens are named
    -- sso:<provider>). Informational: lookups go by issuer and subject.
    provider      text NOT NULL,
    issuer        text NOT NULL,
    subject       text NOT NULL,
    -- The verified address the provider last reported, for display.
    email         text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    CONSTRAINT external_identities_account UNIQUE (issuer, subject),
    CONSTRAINT external_identities_one_per_issuer UNIQUE (principal_id, issuer)
);

-- A first sign-in links to the principal an administrator registered the verified address
-- on, compared case-insensitively.
CREATE INDEX principals_email_lower ON principals (lower(email)) WHERE email IS NOT NULL;

-- A sign-in in flight. Every replica must see it — the provider may send the browser back to
-- a different one than it left from — so it lives here and not in memory. Two phases share
-- the row: the authorization request (state, nonce, PKCE verifier), consumed once by the
-- callback; then the one-time ticket the callback hands the dashboard or CLI, consumed once
-- by redemption. Secrets that arrive in URLs (state, ticket) and the dashboard's browser
-- binder are stored only as SHA-256 hashes.
CREATE TABLE sso_logins (
    state_hash        text PRIMARY KEY,
    provider          text NOT NULL,
    client            text NOT NULL CHECK (client IN ('dashboard', 'cli')),
    nonce             text NOT NULL,
    verifier          text NOT NULL,
    -- dashboard: hash of the cookie that binds the sign-in to the browser that started it.
    binder_hash       text NOT NULL DEFAULT '',
    -- cli: the PKCE challenge the CLI's ticket redemption must answer.
    client_challenge  text NOT NULL DEFAULT '',
    -- dashboard: the same-origin path to return to; cli: its loopback redirect.
    return_to         text NOT NULL DEFAULT '',
    -- Set when a signed-in principal is adding this provider to their account.
    link_principal    uuid REFERENCES principals(id) ON DELETE CASCADE,
    created_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    used_at           timestamptz,
    ticket_hash       text UNIQUE,
    principal_id      uuid REFERENCES principals(id) ON DELETE CASCADE,
    ticket_expires_at timestamptz,
    ticket_used_at    timestamptz
);

CREATE INDEX sso_logins_expires ON sso_logins (expires_at);
