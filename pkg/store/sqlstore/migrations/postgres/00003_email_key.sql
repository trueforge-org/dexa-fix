-- +goose Up
ALTER TABLE users DROP CONSTRAINT users_name_key;
ALTER TABLE users ADD COLUMN email_lookup TEXT;
CREATE UNIQUE INDEX idx_users_email_lookup ON users(email_lookup);

-- +goose Down
DROP INDEX idx_users_email_lookup;
ALTER TABLE users DROP COLUMN email_lookup;
ALTER TABLE users ADD CONSTRAINT users_name_key UNIQUE (name);
