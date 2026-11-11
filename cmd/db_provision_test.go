package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestDBProvisionValidatesItsFlags(t *testing.T) {
	dir := t.TempDir()
	file := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o400); err != nil {
			t.Fatal(err)
		}
		return p
	}
	reset := func() {
		for _, f := range []string{"admin-dsn", "admin-dsn-file", "app-password-file", "body-password-file"} {
			dbProvisionCmd.Flags().Set(f, "")
			dbProvisionCmd.Flags().Lookup(f).Changed = false
		}
	}
	t.Cleanup(reset)

	cases := map[string]struct {
		flags map[string]string
		want  string
	}{
		"a password on the command line": {
			map[string]string{"admin-dsn": "postgres://postgres:hunter2hunter2@db/postgres"}, "contains a password"},
		"no app password file": {
			map[string]string{"admin-dsn-file": file("dsn", "postgres://postgres:x@db/postgres")}, "--app-password-file is required"},
		"no body password file": {
			map[string]string{"admin-dsn-file": file("dsn2", "postgres://postgres:x@db/postgres"), "app-password-file": file("app", "a")}, "--body-password-file is required"},
		"an empty password file": {
			map[string]string{"admin-dsn-file": file("dsn3", "postgres://postgres:x@db/postgres"), "app-password-file": file("empty", "\n"), "body-password-file": file("b", "b")}, "is empty"},
		"no admin connection string": {
			map[string]string{"app-password-file": file("a2", "a"), "body-password-file": file("b2", "b")}, "administrator connection string is required"},
	}
	for name, c := range cases {
		reset()
		for k, v := range c.flags {
			if err := dbProvisionCmd.Flags().Set(k, v); err != nil {
				t.Fatal(err)
			}
		}
		err := dbProvisionCmd.RunE(dbProvisionCmd, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the password was echoed: %v", name, err)
		}
	}
}

// Against a real server the command makes what the deployment runs on, says so, and can be run
// again. It is skipped without a PostgreSQL, like the store's own provision tests.
func TestDBProvisionSetsUpTheRolesTheDatabaseAndTheSchema(t *testing.T) {
	dsn := os.Getenv("DEXAFIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("DEXAFIX_TEST_POSTGRES_DSN is not set")
	}
	// The store's tests provision the same roles and database in another package, at the same time:
	// wait for them, with the lock they hold (provisionLockKey in pkg/store/sqlstore/provision_test.go).
	ctx := context.Background()
	lock, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close(ctx) }) // closing the connection releases the lock
	if _, err := lock.Exec(ctx, "SELECT pg_advisory_lock($1)", 7274707); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o400); err != nil {
			t.Fatal(err)
		}
		return p
	}
	args := []string{"db", "provision",
		"--admin-dsn-file", file("admin_dsn", dsn),
		"--app-password-file", file("db_password", "app-password-for-the-test"),
		"--body-password-file", file("body_db_password", "body-password-for-the-test")}
	for round := 1; round <= 2; round++ { // the second run keeps what the first made
		stdout, stderr, err := run(t, args...)
		if err != nil {
			t.Fatalf("round %d: %v (stderr %q)", round, err, stderr)
		}
		if !strings.Contains(stdout, "are in place") {
			t.Fatalf("round %d: the command did not say that everything is in place: %q", round, stdout)
		}
	}
}
