-- A link sign-in (POST /v1/sso/{provider}/link) no longer links in the callback. The
-- callback runs in whatever browser followed the authorization URL, which need not be the
-- linking principal's, so it only records the account the provider verified; the link is
-- made when the ticket is redeemed with the PKCE verifier the principal started with.
ALTER TABLE sso_logins
    ADD COLUMN link_issuer  text NOT NULL DEFAULT '',
    ADD COLUMN link_subject text NOT NULL DEFAULT '',
    ADD COLUMN link_email   text NOT NULL DEFAULT '',
    ADD COLUMN link_orgs    text[] NOT NULL DEFAULT '{}';
