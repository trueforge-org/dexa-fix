package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// The names Provision creates. They are fixed: the connection strings that deploy init writes and the
// body service's search_path refer to them.
const (
	ProvisionAppRole    = "dexafix"      // the api's role; owns the database
	ProvisionBodyRole   = "dexafix_body" // the body service's role; owns the schema "body" only
	ProvisionDatabase   = "dexafix"
	ProvisionBodySchema = "body"
)

// ProvisionOptions are what Provision needs. AdminDSN is a superuser's connection string (to any
// database, usually "postgres"); the two passwords are the ones the application roles get.
type ProvisionOptions struct {
	AdminDSN     string
	AppPassword  string
	BodyPassword string
}

// Provision makes the PostgreSQL roles, the database and the body schema the recommended deployment
// runs on, so that no service runs as a superuser and a leak of one cannot read the other's data:
//
//	dexafix        owns the database, has no rights in the schema "body"
//	dexafix_body   owns the schema "body" and can use nothing else
//
// It can be run again and again: what exists is kept and the passwords are set again, so a changed
// password file takes effect at the next run. It never drops anything.
func Provision(ctx context.Context, o ProvisionOptions) error {
	if o.AdminDSN == "" || o.AppPassword == "" || o.BodyPassword == "" {
		return errors.New("provision: an admin connection string and both passwords are required")
	}
	if o.AppPassword == o.BodyPassword {
		return errors.New("provision: the two roles must have different passwords")
	}

	cfg, err := parsePG(o.AdminDSN)
	if err != nil {
		return err
	}
	admin := openPool(cfg)
	defer admin.Close()
	if err := admin.PingContext(ctx); err != nil {
		return fmt.Errorf("provision: connect as the administrator: %w", err)
	}

	for _, r := range []struct{ role, password string }{
		{ProvisionAppRole, o.AppPassword},
		{ProvisionBodyRole, o.BodyPassword},
	} {
		if err := ensureRole(ctx, admin, r.role, r.password); err != nil {
			return err
		}
	}

	var exists bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, ProvisionDatabase).Scan(&exists); err != nil {
		return fmt.Errorf("provision: look for the database: %w", err)
	}
	if !exists {
		// CREATE DATABASE cannot run in a transaction and takes no placeholders; both names are constants.
		if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, quoteIdent(ProvisionDatabase), quoteIdent(ProvisionAppRole))); err != nil {
			return fmt.Errorf("provision: create the database: %w", err)
		}
	}

	dbCfg := cfg.Copy()
	dbCfg.Database = ProvisionDatabase
	db := openPool(dbCfg)
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("provision: connect to the database: %w", err)
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s AUTHORIZATION %s`, quoteIdent(ProvisionBodySchema), quoteIdent(ProvisionBodyRole)),
		fmt.Sprintf(`ALTER SCHEMA %s OWNER TO %s`, quoteIdent(ProvisionBodySchema), quoteIdent(ProvisionBodyRole)),
		fmt.Sprintf(`ALTER ROLE %s SET search_path = %s`, quoteIdent(ProvisionBodyRole), quoteIdent(ProvisionBodySchema)),
		// Nobody but the owner of the database (the api's role) may use the schema "public": without this
		// every role may look at its tables' names and create objects there. The grant keeps the api's
		// role working on servers older than 15, where "public" is not owned by the database owner.
		`REVOKE ALL ON SCHEMA public FROM PUBLIC`,
		fmt.Sprintf(`GRANT ALL ON SCHEMA public TO %s`, quoteIdent(ProvisionAppRole)),
		fmt.Sprintf(`REVOKE ALL ON SCHEMA %s FROM PUBLIC`, quoteIdent(ProvisionBodySchema)),
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("provision: %s: %w", stmt, err)
		}
	}
	return tx.Commit()
}

// ensureRole creates a login role or, when it exists, sets its password again. The statement is built
// by the server with format(%I, %L), so no password or name is ever quoted by hand.
func ensureRole(ctx context.Context, db *sql.DB, role, password string) error {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil {
		return fmt.Errorf("provision: look for role %s: %w", role, err)
	}
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	var stmt string
	if err := db.QueryRowContext(ctx, `SELECT format($1::text || ' ROLE %I LOGIN PASSWORD %L', $2::text, $3::text)`, verb, role, password).Scan(&stmt); err != nil {
		return fmt.Errorf("provision: prepare role %s: %w", role, err)
	}
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		// The statement holds the password, so it is not part of the message (the server's error is not).
		return fmt.Errorf("provision: %s role %s: %w", verb, role, err)
	}
	return nil
}

// openPG opens a connection pool from a PostgreSQL connection string (URL or key/value form),
// optionally changing the parsed settings first (another database, another user).
func openPG(dsn string, change func(*pgx.ConnConfig)) (*sql.DB, error) {
	cfg, err := parsePG(dsn)
	if err != nil {
		return nil, err
	}
	if change != nil {
		change(cfg)
	}
	return openPool(cfg), nil
}

// parsePG parses a connection string without ever echoing it: it may hold a password.
func parsePG(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("provision: the connection string cannot be parsed")
	}
	return cfg, nil
}

// openPool opens a small pool for a parsed configuration. Nothing connects until the first use.
func openPool(cfg *pgx.ConnConfig) *sql.DB {
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(2)
	return db
}

// quoteIdent quotes a name that comes from a constant of this file, never from a request.
func quoteIdent(s string) string { return pgx.Identifier{s}.Sanitize() }
