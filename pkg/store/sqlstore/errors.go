package sqlstore

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"

	"github.com/trueforge-org/dexafix/pkg/store"
)

// SQLite result codes (https://www.sqlite.org/rescode.html).
const (
	sqliteConstraintForeignKey = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

// PostgreSQL SQLSTATE codes.
const (
	pgForeignKeyViolation = "23503"
	pgUniqueViolation     = "23505"
)

// usernameIndex is the unique index on lower(username), see migration 00002.
const usernameIndex = "users_username_lower"

// uniqueError maps a unique violation to ErrUsernameTaken when it is the username's index, and to
// ErrExists otherwise.
func uniqueError(err error) error {
	if strings.Contains(err.Error(), usernameIndex) {
		return store.ErrUsernameTaken
	}
	return store.ErrExists
}

// isUnique reports whether err is a unique / primary key violation on either backend.
func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgUniqueViolation
	}
	var sqErr *sqlite.Error
	if errors.As(err, &sqErr) {
		switch sqErr.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return true
		}
	}
	return false
}

// isFK reports whether err is a foreign key violation on either backend.
func isFK(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgForeignKeyViolation
	}
	var sqErr *sqlite.Error
	if errors.As(err, &sqErr) {
		switch sqErr.Code() {
		case sqliteConstraintForeignKey:
			return true
		}
	}
	return false
}
