-- +goose NO TRANSACTION

-- SQLite cannot drop a UNIQUE constraint, so the table is rebuilt. Foreign keys must be off while
-- that happens, or dropping the old table would cascade and delete every user's data.

-- +goose Up
PRAGMA foreign_keys = OFF;
BEGIN;
CREATE TABLE users_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  athlete_default INTEGER NOT NULL DEFAULT 0,
  method_ranking TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'user',
  email TEXT NOT NULL DEFAULT '',
  height_cm REAL,
  email_lookup TEXT
);
INSERT INTO users_new (id, name, created_at, athlete_default, method_ranking, role, email, height_cm) SELECT id, name, created_at, athlete_default, method_ranking, role, email, height_cm FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
CREATE UNIQUE INDEX idx_users_email_lookup ON users(email_lookup);
COMMIT;
PRAGMA foreign_keys = ON;

-- +goose Down
PRAGMA foreign_keys = OFF;
BEGIN;
CREATE TABLE users_old (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  created_at TIMESTAMP NOT NULL,
  athlete_default INTEGER NOT NULL DEFAULT 0,
  method_ranking TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'user',
  email TEXT NOT NULL DEFAULT '',
  height_cm REAL
);
INSERT INTO users_old (id, name, created_at, athlete_default, method_ranking, role, email, height_cm) SELECT id, name, created_at, athlete_default, method_ranking, role, email, height_cm FROM users;
DROP TABLE users;
ALTER TABLE users_old RENAME TO users;
COMMIT;
PRAGMA foreign_keys = ON;
