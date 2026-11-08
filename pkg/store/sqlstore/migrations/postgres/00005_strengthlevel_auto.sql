-- +goose Up
-- Set by an administrator: the user may store Strength Level credentials and sync automatically.
ALTER TABLE users ADD COLUMN strengthlevel_auto BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE users DROP COLUMN strengthlevel_auto;
