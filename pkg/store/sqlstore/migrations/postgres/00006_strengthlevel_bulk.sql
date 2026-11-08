-- +goose Up
-- Set by an administrator: the user may also run the multi-day Strength Level sync from Settings.
ALTER TABLE users ADD COLUMN strengthlevel_bulk BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE users DROP COLUMN strengthlevel_bulk;
