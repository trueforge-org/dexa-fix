-- +goose Up
-- The user's IANA time zone: it decides which calendar day a reading belongs to, and how manual
-- date-times are read and shown. UTC keeps the behaviour of existing accounts.
ALTER TABLE users ADD COLUMN timezone TEXT NOT NULL DEFAULT 'UTC';

-- +goose Down
ALTER TABLE users DROP COLUMN timezone;
