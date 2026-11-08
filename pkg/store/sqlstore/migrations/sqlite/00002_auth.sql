-- +goose Up
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'user';
ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN height_cm REAL;
-- The oldest existing account becomes the first administrator.
UPDATE users SET role = 'admin' WHERE id = (SELECT MIN(id) FROM users);
-- One row per way of signing in. A password is the provider 'local'; external providers use their
-- slug and the provider's subject, so accounts do not depend on any one sign-in method.
CREATE TABLE user_identities (
  provider TEXT NOT NULL,
  subject TEXT NOT NULL,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  secret TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL,
  PRIMARY KEY (provider, subject)
);
CREATE INDEX idx_identities_user ON user_identities(user_id);
CREATE TABLE sessions (
  token_hash TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  method TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE TABLE access_tokens (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  prefix TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  created_at TIMESTAMP NOT NULL,
  last_used_at TIMESTAMP,
  expires_at TIMESTAMP
);
CREATE INDEX idx_access_tokens_user ON access_tokens(user_id);
CREATE TABLE app_settings (
  name TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TIMESTAMP NOT NULL
);
CREATE TABLE auth_providers (
  slug TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  kind TEXT NOT NULL,
  issuer TEXT NOT NULL DEFAULT '',
  auth_url TEXT NOT NULL DEFAULT '',
  token_url TEXT NOT NULL DEFAULT '',
  userinfo_url TEXT NOT NULL DEFAULT '',
  client_id TEXT NOT NULL,
  client_secret TEXT NOT NULL DEFAULT '',
  apple_team_id TEXT NOT NULL DEFAULT '',
  apple_key_id TEXT NOT NULL DEFAULT '',
  scopes TEXT NOT NULL DEFAULT '',
  subject_claim TEXT NOT NULL DEFAULT '',
  name_claim TEXT NOT NULL DEFAULT '',
  allow_signup INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE auth_providers;
DROP TABLE app_settings;
DROP TABLE access_tokens;
DROP TABLE sessions;
DROP TABLE user_identities;
ALTER TABLE users DROP COLUMN height_cm;
ALTER TABLE users DROP COLUMN email;
ALTER TABLE users DROP COLUMN role;
