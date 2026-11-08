-- +goose Up
-- Set by an administrator: the user may store Strength Level credentials and sync automatically.
ALTER TABLE users ADD COLUMN strengthlevel_auto INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE users DROP COLUMN strengthlevel_auto;
