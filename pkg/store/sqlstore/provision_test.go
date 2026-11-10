package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// provisionDSN is the administrator's connection string; the tests need a real PostgreSQL server, and
// they create the roles "dexafix" and "dexafix_body" and the database "dexafix" in it.
func provisionDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DEXAFIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("DEXAFIX_TEST_POSTGRES_DSN is not set")
	}
	if err := checkProvisionAdmin(dsn); err != nil {
		t.Fatal(err)
	}
	return dsn
}

// checkProvisionAdmin refuses an administrator that is one of the roles Provision makes. Provision
// sets their passwords, so the next connection of that administrator, and with it every other test
// of the package that uses the same DSN, would fail with "password authentication failed".
func checkProvisionAdmin(dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return err
	}
	if cfg.User == ProvisionAppRole || cfg.User == ProvisionBodyRole {
		return fmt.Errorf("DEXAFIX_TEST_POSTGRES_DSN logs in as %q, a role that Provision creates and gives a new password: "+
			"use another administrator, for example postgres://postgres:x@localhost:5432/postgres", cfg.User)
	}
	return nil
}

func TestCheckProvisionAdmin(t *testing.T) {
	for dsn, wantErr := range map[string]bool{
		"postgres://postgres:x@localhost:5432/postgres?sslmode=disable":          false,
		"postgres://admin:x@localhost:5432/dexafix_test?sslmode=disable":         false,
		"postgres://dexafix:dexafix@localhost:5432/dexafix_test?sslmode=disable": true,
		"postgres://dexafix_body:x@localhost:5432/dexafix_test?sslmode=disable":  true,
	} {
		if err := checkProvisionAdmin(dsn); (err != nil) != wantErr {
			t.Errorf("checkProvisionAdmin(%q) = %v, want error: %v", dsn, err, wantErr)
		}
	}
}

// openAs connects as another role to the database "dexafix".
func openAs(t *testing.T, adminDSN, user, password string) *sql.DB {
	t.Helper()
	db, err := openPG(adminDSN, func(c *pgx.ConnConfig) {
		c.User, c.Password, c.Database = user, password, ProvisionDatabase
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestProvisionIsolatesTheRoles(t *testing.T) {
	ctx := context.Background()
	dsn := provisionDSN(t)
	o := ProvisionOptions{AdminDSN: dsn, AppPassword: "app-pw-with-'quote-and-\\backslash", BodyPassword: "body-pw-1"}

	// Twice: the second run keeps what exists.
	for i := 0; i < 2; i++ {
		if err := Provision(ctx, o); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}

	app := openAs(t, dsn, ProvisionAppRole, o.AppPassword)
	body := openAs(t, dsn, ProvisionBodyRole, o.BodyPassword)

	if _, err := app.ExecContext(ctx, `CREATE TABLE public.prov_app (id int)`); err != nil {
		t.Fatalf("the api's role must work in public: %v", err)
	}
	t.Cleanup(func() { app.ExecContext(ctx, `DROP TABLE IF EXISTS public.prov_app`) })

	if _, err := body.ExecContext(ctx, `CREATE TABLE prov_body (id int)`); err != nil { // search_path = body
		t.Fatalf("the body role must work in its schema: %v", err)
	}
	var schema string
	if err := body.QueryRowContext(ctx, `SELECT table_schema FROM information_schema.tables WHERE table_name = 'prov_body'`).Scan(&schema); err != nil || schema != ProvisionBodySchema {
		t.Fatalf("search_path of the body role: schema %q, err %v", schema, err)
	}
	t.Cleanup(func() { body.ExecContext(ctx, `DROP TABLE IF EXISTS body.prov_body`) })

	for name, c := range map[string]struct {
		db  *sql.DB
		sql string
	}{
		"api creates in body":    {app, `CREATE TABLE body.prov_x (id int)`},
		"api reads body":         {app, `SELECT * FROM body.prov_body`},
		"body creates in public": {body, `CREATE TABLE public.prov_x (id int)`},
		"body reads public":      {body, `SELECT * FROM public.prov_app`},
	} {
		if _, err := c.db.ExecContext(ctx, c.sql); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: want permission denied, got %v", name, err)
		}
	}
}

func TestProvisionAppliesAChangedPassword(t *testing.T) {
	ctx := context.Background()
	dsn := provisionDSN(t)
	o := ProvisionOptions{AdminDSN: dsn, AppPassword: "first-app-pw", BodyPassword: "first-body-pw"}
	if err := Provision(ctx, o); err != nil {
		t.Fatal(err)
	}
	o.AppPassword = "second-app-pw"
	if err := Provision(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := openAs(t, dsn, ProvisionAppRole, "second-app-pw").PingContext(ctx); err != nil {
		t.Fatalf("the new password must work: %v", err)
	}
	if err := openAs(t, dsn, ProvisionAppRole, "first-app-pw").PingContext(ctx); err == nil {
		t.Fatal("the old password must not work any more")
	}
}

func TestProvisionRefusesBadInput(t *testing.T) { // needs no server
	ctx := context.Background()
	for name, o := range map[string]ProvisionOptions{
		"no dsn":         {AppPassword: "a", BodyPassword: "b"},
		"no password":    {AdminDSN: "postgres://x@h/postgres", AppPassword: "a"},
		"same passwords": {AdminDSN: "postgres://x@h/postgres", AppPassword: "a", BodyPassword: "a"},
	} {
		if err := Provision(ctx, o); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	err := Provision(ctx, ProvisionOptions{AdminDSN: "postgres://:not a url\x7f", AppPassword: "a", BodyPassword: "b"})
	if err == nil || strings.Contains(err.Error(), "not a url") {
		t.Errorf("a bad connection string must be refused without echoing it: %v", err)
	}
}
