-- +goose Up
-- The display name becomes a unique username (case-insensitive). Names that only differ in case were
-- allowed before, so every later account of such a group gets its id appended to stay unique.
UPDATE users SET name = name || '-' || id WHERE EXISTS (
  SELECT 1 FROM users o WHERE lower(o.name) = lower(users.name)
    AND (o.created_at < users.created_at OR (o.created_at = users.created_at AND o.id < users.id))
);
ALTER TABLE users RENAME COLUMN name TO username;
CREATE UNIQUE INDEX users_username_lower ON users (lower(username));
ALTER TABLE users ADD COLUMN first_name TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN last_name TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE users DROP COLUMN last_name;
ALTER TABLE users DROP COLUMN first_name;
DROP INDEX users_username_lower;
ALTER TABLE users RENAME COLUMN username TO name;
