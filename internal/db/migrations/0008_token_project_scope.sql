-- A token can be confined to one project. Until now every bearer token worked across every
-- project its principal belonged to, so the credential a runner hands an agent for one
-- attempt was as powerful as its operator's own login. A scoped token is refused by every
-- project other than the one it names (coord.Authorize), and cannot mint further tokens.
ALTER TABLE api_tokens
    ADD COLUMN IF NOT EXISTS project_id uuid REFERENCES projects(id) ON DELETE CASCADE;
