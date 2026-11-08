-- +goose Up
-- Comma-separated keys of the daily table columns the user has hidden.
ALTER TABLE users ADD COLUMN hidden_columns TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE users DROP COLUMN hidden_columns;
