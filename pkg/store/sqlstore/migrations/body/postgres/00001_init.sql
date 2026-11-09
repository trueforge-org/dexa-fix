-- +goose Up
-- The body domain: a user's scale readings and DEXA scans. user_id is an opaque reference with no
-- foreign key: this domain can live in a database of its own, so deleting a user is an explicit call
-- (the main backend makes it, and a maintenance sweep removes what was missed). What identifies and orders a row stays in
-- plain columns (indexes, paging and the upsert need them). The measured values and the notes are
-- one JSON document in data, encrypted with the body key when an encryption key is configured. The
-- context of the encryption names the user and row_uid, so a value cannot be moved to another row.
-- row_uid is random and set whenever data is written.
CREATE TABLE scale_readings (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL,
  measured_at TIMESTAMPTZ NOT NULL,
  source TEXT NOT NULL DEFAULT 'manual',
  method TEXT NOT NULL DEFAULT 'scale',
  athlete_mode BOOLEAN NOT NULL DEFAULT FALSE,
  external_id TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  row_uid TEXT NOT NULL UNIQUE,
  data TEXT NOT NULL,
  UNIQUE (user_id, source, external_id)
);
CREATE INDEX idx_scale_user_time ON scale_readings(user_id, measured_at);
CREATE TABLE dexa_scans (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL,
  measured_at DATE NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  row_uid TEXT NOT NULL UNIQUE,
  data TEXT NOT NULL
);
CREATE INDEX idx_dexa_user_time ON dexa_scans(user_id, measured_at);
-- Counts changes to a user's scans and readings, so cached estimates can tell when they are out of
-- date (see bumpBodyVersion). The body domain's own counter: it does not touch the users table.
CREATE TABLE body_user_state (
  user_id BIGINT PRIMARY KEY,
  data_version BIGINT NOT NULL DEFAULT 0
);
-- The key check of the body key: the same idea as app_settings.crypto.check in the main database,
-- kept here because this domain can live in a database of its own, with a key of its own.
CREATE TABLE body_meta (
  name TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE body_meta;
DROP TABLE body_user_state;
DROP TABLE dexa_scans;
DROP TABLE scale_readings;
