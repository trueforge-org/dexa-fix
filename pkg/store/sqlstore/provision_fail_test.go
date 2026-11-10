package sqlstore

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// shadowDSN returns the administrator's DSN with a search_path that finds the objects of a fresh
// schema before pg_catalog does, so that the statements Provision sends fail on the server for real.
// The schema is made, filled with setup (SCHEMA. stands for its name) and dropped again.
func shadowDSN(t *testing.T, dsn, setup string) string {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("provshadow_%d_%d", os.Getpid(), time.Now().UnixNano())
	admin, err := openPG(dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	t.Cleanup(func() {
		if a, err := openPG(dsn, nil); err == nil {
			a.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
			a.Close()
		}
	})
	for _, stmt := range []string{`CREATE SCHEMA ` + schema, strings.ReplaceAll(setup, "SCHEMA.", schema+".")} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema+",pg_catalog")
	u.RawQuery = q.Encode()
	return u.String()
}

func provisionOptions(dsn string) ProvisionOptions {
	return ProvisionOptions{AdminDSN: dsn, AppPassword: "fail-app-pw", BodyPassword: "fail-body-pw"}
}

func TestProvisionReportsWhatTheServerRefuses(t *testing.T) {
	ctx := context.Background()
	dsn := provisionDSN(t)
	if err := Provision(ctx, provisionOptions(dsn)); err != nil { // the roles and the database exist from here on
		t.Fatal(err)
	}
	inDexafix := func(c *pgx.ConnConfig) { c.Database = ProvisionDatabase }

	t.Run("an administrator that may not manage roles", func(t *testing.T) {
		admin, err := openPG(dsn, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close()
		name := fmt.Sprintf("provtest_plain_%d", os.Getpid())
		if _, err := admin.ExecContext(ctx, `CREATE ROLE `+name+` LOGIN PASSWORD 'plain-pw' NOSUPERUSER NOCREATEROLE NOCREATEDB`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+name) })
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(name, "plain-pw")
		if err := Provision(ctx, provisionOptions(u.String())); err == nil || !strings.Contains(err.Error(), "ALTER role dexafix") {
			t.Fatalf("want the refused ALTER ROLE, got %v", err)
		}
	})

	t.Run("the role lookup fails", func(t *testing.T) {
		o := provisionOptions(shadowDSN(t, dsn, `CREATE TABLE SCHEMA.pg_roles (other int)`))
		if err := Provision(ctx, o); err == nil || !strings.Contains(err.Error(), "look for role") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("the role statement cannot be prepared", func(t *testing.T) {
		o := provisionOptions(shadowDSN(t, dsn, `CREATE FUNCTION SCHEMA.format(text, text, text) RETURNS text LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'no format today'; END $$`))
		if err := Provision(ctx, o); err == nil || !strings.Contains(err.Error(), "prepare role") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("the database lookup fails", func(t *testing.T) {
		o := provisionOptions(shadowDSN(t, dsn, `CREATE TABLE SCHEMA.pg_database (other int)`))
		if err := Provision(ctx, o); err == nil || !strings.Contains(err.Error(), "look for the database") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("the database cannot be created", func(t *testing.T) {
		// The lookup finds no database, so Provision tries to create the one that exists.
		o := provisionOptions(shadowDSN(t, dsn, `CREATE TABLE SCHEMA.pg_database (datname name)`))
		if err := Provision(ctx, o); err == nil || !strings.Contains(err.Error(), "create the database") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("the database does not accept connections", func(t *testing.T) {
		admin, err := openPG(dsn, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close()
		if _, err := admin.ExecContext(ctx, `ALTER DATABASE `+ProvisionDatabase+` ALLOW_CONNECTIONS false`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if a, err := openPG(dsn, nil); err == nil {
				a.ExecContext(ctx, `ALTER DATABASE `+ProvisionDatabase+` ALLOW_CONNECTIONS true`)
				a.Close()
			}
		})
		if err := Provision(ctx, provisionOptions(dsn)); err == nil || !strings.Contains(err.Error(), "connect to the database") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a statement of the schema step is refused", func(t *testing.T) {
		db, err := openPG(dsn, inDexafix)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		t.Cleanup(func() {
			if d, err := openPG(dsn, inDexafix); err == nil {
				d.ExecContext(ctx, `DROP EVENT TRIGGER IF EXISTS provtest_refuse`)
				d.ExecContext(ctx, `DROP FUNCTION IF EXISTS public.provtest_refuse()`)
				d.Close()
			}
		})
		for _, stmt := range []string{
			`CREATE FUNCTION public.provtest_refuse() RETURNS event_trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused by the test'; END $$`,
			`CREATE EVENT TRIGGER provtest_refuse ON ddl_command_start WHEN TAG IN ('REVOKE') EXECUTE FUNCTION public.provtest_refuse()`,
		} {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatal(err)
			}
		}
		if err := Provision(ctx, provisionOptions(dsn)); err == nil || !strings.Contains(err.Error(), "refused by the test") {
			t.Fatalf("got %v", err)
		}
	})

	// The failures above must leave a Provision that works.
	if err := Provision(ctx, provisionOptions(dsn)); err != nil {
		t.Fatalf("after the failures: %v", err)
	}
}
