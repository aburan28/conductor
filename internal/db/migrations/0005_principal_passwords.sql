-- Username/password sign-in for the dashboard.
--
-- The hash column is nullable on purpose: a principal without a password is exactly what
-- every service identity should be, and existing humans keep working on tokens alone until
-- someone sets a password for them. The format is `pbkdf2-sha256$<iterations>$<salt>$<hash>`
-- (see internal/db/password.go), so the cost factor can move without a second column.

ALTER TABLE principals ADD COLUMN password_hash text;
