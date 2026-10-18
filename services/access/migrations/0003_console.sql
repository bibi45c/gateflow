-- Console users, and a name and display prefix for gateway keys (issue #137). Only
-- adds a table and defaulted columns, so the previous access image still starts on
-- this schema.

-- A user signs in to the console. An admin sees every tenant; a user sees only the
-- tenant it belongs to. password_hash is "pbkdf2-sha256$<iterations>$<salt>$<hash>"
-- with base64url parts; the password itself is never stored.
CREATE TABLE console_users (
  id            text PRIMARY KEY,
  username      text NOT NULL UNIQUE CHECK (username ~ '^[a-z0-9_.-]{1,64}$'),
  password_hash text NOT NULL CHECK (password_hash LIKE 'pbkdf2-sha256$%'),
  role          text NOT NULL CHECK (role IN ('user', 'admin')),
  tenant_id     text REFERENCES tenants (id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT console_users_tenant_check CHECK ((role = 'user') = (tenant_id IS NOT NULL))
);

-- key_prefix is the first characters of the raw key, shown so a user can tell keys
-- apart; it is too short to use. Seeded keys keep empty values.
ALTER TABLE api_keys ADD COLUMN name text NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN key_prefix text NOT NULL DEFAULT '';
