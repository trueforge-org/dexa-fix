-- +goose Up
CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  athlete_default INTEGER NOT NULL DEFAULT 0,
  method_ranking TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'user',
  email TEXT NOT NULL DEFAULT '',
  -- Keyed hash of the e-mail address, so it can be looked up while the address itself is encrypted.
  email_lookup TEXT,
  height_cm REAL,
  -- Comma-separated keys of the daily table columns the user has hidden.
  hidden_columns TEXT NOT NULL DEFAULT '',
  -- Set by an administrator: the user may store Strength Level credentials and sync automatically.
  strengthlevel_auto INTEGER NOT NULL DEFAULT 0,
  -- Counts changes to everything the estimates are computed from, so cached estimates can tell when
  -- they are out of date (see Store.bumpVersion).
  data_version INTEGER NOT NULL DEFAULT 0,
  -- Set by an administrator: the user may also run the multi-day Strength Level sync from Settings.
  strengthlevel_bulk INTEGER NOT NULL DEFAULT 0,
  -- The user's IANA time zone: it decides which calendar day a reading belongs to, and how manual
  -- date-times are read and shown.
  timezone TEXT NOT NULL DEFAULT 'UTC',
  -- Random handle that identifies the user to passkeys (WebAuthn user.id). It is not personal data.
  passkey_handle TEXT
);
CREATE UNIQUE INDEX idx_users_email_lookup ON users(email_lookup);
CREATE UNIQUE INDEX idx_users_passkey_handle ON users(passkey_handle);
CREATE TABLE scale_readings (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  measured_at TIMESTAMP NOT NULL,
  weight_kg REAL, fat_ratio_pct REAL, fat_mass_kg REAL, fat_free_mass_kg REAL,
  muscle_mass_kg REAL, hydration_kg REAL, bone_mass_kg REAL, visceral_fat_index REAL,
  source TEXT NOT NULL DEFAULT 'manual',
  method TEXT NOT NULL DEFAULT 'scale',
  athlete_mode INTEGER NOT NULL DEFAULT 0,
  external_id TEXT,
  notes TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL,
  UNIQUE (user_id, source, external_id)
);
CREATE INDEX idx_scale_user_time ON scale_readings(user_id, measured_at);
CREATE TABLE dexa_scans (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  measured_at DATE NOT NULL,
  weight_kg REAL, fat_ratio_pct REAL, fat_mass_kg REAL, fat_free_mass_kg REAL,
  lean_mass_kg REAL, bone_mass_kg REAL, visceral_fat_mass_kg REAL,
  bmd_g_cm2 REAL, t_score REAL, z_score REAL, android_fat_pct REAL, gynoid_fat_pct REAL,
  notes TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_dexa_user_time ON dexa_scans(user_id, measured_at);
CREATE TABLE user_integrations (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT '{}',
  updated_at TIMESTAMP NOT NULL,
  PRIMARY KEY (user_id, provider)
);
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
-- Two-step verification with a time-based one-time password (TOTP). The secret is encrypted
-- (context totp:<user id>); confirmed_at is NULL while the user is still setting it up. last_step
-- is the newest 30-second step that was accepted, so a code cannot be used twice.
CREATE TABLE user_totp (
  user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  secret TEXT NOT NULL,
  last_step INTEGER NOT NULL DEFAULT 0,
  confirmed_at TIMESTAMP,
  created_at TIMESTAMP NOT NULL
);
-- Passkeys (WebAuthn) for passwordless sign-in. credential is the JSON of the credential (public key,
-- transports, flags): public keys are not secret, so it is not encrypted. credential_id is base64url.
CREATE TABLE passkeys (
  credential_id TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  credential TEXT NOT NULL,
  sign_count INTEGER NOT NULL DEFAULT 0,
  backup_eligible INTEGER NOT NULL DEFAULT 0,
  backup_state INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMP NOT NULL,
  last_used_at TIMESTAMP
);
CREATE INDEX idx_passkeys_user ON passkeys(user_id);
-- Single-use password reset links. Only a hash of the token is stored, like sessions. A link is
-- deleted when it is used, replaced or expires, and with the user. Not copied by `db copy`.
CREATE TABLE password_resets (
  token_hash TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_password_resets_user ON password_resets(user_id);
-- Single-use recovery codes, stored as hashes of long random values.
CREATE TABLE user_recovery_codes (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  code_hash TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  PRIMARY KEY (user_id, code_hash)
);
-- Logging in deletes expired sessions and the admin page counts them; both filter on expires_at.
CREATE INDEX idx_sessions_expires ON sessions(expires_at);
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
-- generation counts changes to app_settings, so that every process that caches the settings can
-- tell cheaply that it has to read them again (the web and api processes, or several replicas).
CREATE TABLE app_settings_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  generation BIGINT NOT NULL
);
INSERT INTO app_settings_meta (id, generation) VALUES (1, 0);
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
-- Security-relevant events. The people involved are kept as nullable references, so deleting a
-- user keeps the event but not who it was about.
CREATE TABLE audit_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at TIMESTAMP NOT NULL,
  actor_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
  target_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
  action TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_audit_created ON audit_log(created_at);
CREATE INDEX idx_audit_actor ON audit_log(actor_id);
CREATE INDEX idx_audit_target ON audit_log(target_id);

-- Single-use values and failed attempts live here, not in memory, so that every backend process
-- sees the same: a sealed value that was spent on one cannot be spent again on another, and a
-- lock-out counts the attempts made against all of them.
CREATE TABLE used_nonces (
  nonce TEXT PRIMARY KEY,
  expires_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_used_nonces_expires ON used_nonces(expires_at);
CREATE TABLE auth_failures (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  fail_key TEXT NOT NULL,
  failed_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_auth_failures_key ON auth_failures(fail_key, failed_at);
CREATE INDEX idx_auth_failures_at ON auth_failures(failed_at);
-- The status of a user's Strength Level sync (a JSON view), written by the backend that runs it and
-- read by any backend. A 'running' state that stopped being updated belongs to a dead process.
CREATE TABLE strengthlevel_jobs (
  user_id INTEGER NOT NULL PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  state TEXT NOT NULL,
  updated_at TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE strengthlevel_jobs;
DROP TABLE auth_failures;
DROP TABLE used_nonces;
DROP TABLE passkeys;
DROP TABLE password_resets;
DROP TABLE user_recovery_codes;
DROP TABLE user_totp;
DROP TABLE audit_log;DROP TABLE auth_providers;
DROP TABLE app_settings_meta;
DROP TABLE app_settings;
DROP TABLE access_tokens;
DROP TABLE sessions;
DROP TABLE user_identities;
DROP TABLE user_integrations;
DROP TABLE dexa_scans;
DROP TABLE scale_readings;
DROP TABLE users;
