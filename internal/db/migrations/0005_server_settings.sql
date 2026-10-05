-- Machine-level settings for this control plane, one row.
--
-- local_owner_id is the principal "Sign in on this computer" acts as: the person who set the
-- machine up (first `conductord bootstrap`). A dashboard opened on the same machine, the CLI,
-- or the macOS app can obtain a token for them without anyone pasting one.
--
-- security_mode decides whether that local sign-in exists at all. 'local' allows it from
-- loopback; 'enhanced' turns it off so every client needs a token minted the ordinary way.
-- NULL means "not chosen": conductord picks local for a loopback-only daemon and enhanced
-- for one reachable from the network.
CREATE TABLE server_settings (
    singleton       boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    local_owner_id  uuid REFERENCES principals(id) ON DELETE SET NULL,
    security_mode   text CHECK (security_mode IS NULL OR security_mode IN ('local', 'enhanced')),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
