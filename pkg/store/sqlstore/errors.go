package sqlstore

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"
)

// SQLite result codes (https://www.sqlite.org/rescode.html).
const (
	sqliteConstraint           = 19   // SQLITE_CONSTRAINT
	sqliteConstraintForeignKey = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

// PostgreSQL SQLSTATE codes.
const (
	pgForeignKeyViolation = "23503"
	pgUniqueViolation     = "23505"
)

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
		case sqliteConstraint: // extended codes not reported
			return strings.Contains(err.Error(), "UNIQUE constraint failed")
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
		case sqliteConstraint:
			return strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
		}
	}
	return false
}
