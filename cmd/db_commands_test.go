package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

// newDSN is a SQLite database in a new folder.
func newDSN(t *testing.T) string {
	t.Helper()
	return "file:" + filepath.Join(t.TempDir(), "d.db")
}

// openForTests opens the database as the test's own handle, without a key.
func openForTests(t *testing.T, dsn string, opts ...sqlstore.Option) *sqlstore.Store {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, dsn, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// damageMigrationTable replaces the table that records the applied migrations with one the
// migration tool cannot read, the way a restore from the wrong backup would leave it.
func damageMigrationTable(t *testing.T, dsn string, columns string) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, dsn, sqlstore.WithoutMigrate())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // a handle left open would hold the database against the command under test
	for _, q := range []string{"DROP TABLE goose_db_version", "CREATE TABLE goose_db_version (" + columns + ")"} {
		if err := st.ExecForTests(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDBMigrateWalksTheSchemaUpAndDown(t *testing.T) {
	dsn := newDSN(t)
	migrate := func(args ...string) (string, string, error) {
		return run(t, append([]string{"db", "migrate", "--dsn", dsn}, args...)...)
	}

	if out, _, err := migrate("version"); err != nil || strings.TrimSpace(out) != "0" {
		t.Fatalf("version of a new database: %q %v", out, err)
	}
	if out, _, err := migrate("up"); err != nil || !strings.Contains(out, "applied") || !strings.Contains(out, "00001_init.sql") {
		t.Fatalf("up on a new database: %q %v", out, err)
	}
	if out, _, err := migrate(); err != nil || !strings.Contains(out, "already up to date") {
		t.Fatalf("up (the default action) when there is nothing to apply: %q %v", out, err)
	}
	out, _, err := migrate("status")
	if err != nil || !strings.Contains(out, "00001_init.sql") || !strings.Contains(out, "applied ") {
		t.Fatalf("status: %q %v", out, err)
	}

	// Rolling back drops tables, so it wants --yes and says what it would drop.
	_, errText, err := migrate("down")
	if err == nil || !strings.Contains(err.Error(), "--yes") || !strings.Contains(errText, "00002_usernames.sql") {
		t.Fatalf("down without --yes: %v %q", err, errText)
	}
	if out, _, err = migrate("version"); err != nil || strings.TrimSpace(out) != "2" {
		t.Fatalf("a refused rollback changed the schema: %q %v", out, err)
	}
	if out, _, err = migrate("down", "--yes"); err != nil || !strings.Contains(out, "rolled back") {
		t.Fatalf("down --yes: %q %v", out, err)
	}
	if out, _, err = migrate("version"); err != nil || strings.TrimSpace(out) != "1" {
		t.Fatalf("version after the rollback: %q %v", out, err)
	}
	if out, _, err = migrate("up"); err != nil || !strings.Contains(out, "applied") || !strings.Contains(out, "00002_usernames.sql") {
		t.Fatalf("up after the rollback: %q %v", out, err)
	}
	if out, _, err = migrate("to", "0"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("to 0 without --yes: %q %v", out, err)
	}
	if out, _, err = migrate("to", "0", "--yes"); err != nil || !strings.Contains(out, "schema at version 0") {
		t.Fatalf("to 0 --yes: %q %v", out, err)
	}
	if out, _, err = migrate("to", "1"); err != nil || !strings.Contains(out, "schema at version 1") {
		t.Fatalf("to 1: %q %v", out, err)
	}
}

func TestDBMigrateRefusesWhatItCannotDo(t *testing.T) {
	dsn := newDSN(t)
	cases := map[string]struct {
		args []string
		want string
	}{
		"an action it does not know":        {[]string{"sideways"}, "unknown action"},
		"to without a version":              {[]string{"to"}, "usage: db migrate to"},
		"to a version that is not a number": {[]string{"to", "latest"}, "invalid version"},
		"to a version below zero":           {[]string{"to", "--", "-1"}, "invalid version"},
		"a password in the connection string": {
			[]string{"--dsn", "postgres://u:hunter2hunter2@localhost/x"}, "contains a password"},
		"a database driver that does not exist": {[]string{"--db-driver", "oracle"}, "oracle"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := run(t, append([]string{"db", "migrate", "--dsn", dsn}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

// A database that came from a newer release: its schema is ahead of this binary.
func TestDBMigrateRefusesADatabaseFromANewerRelease(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"up":                 {[]string{"up"}, "newer than this binary"},
		"down":               {[]string{"down", "--yes"}, "version not found"},
		"to a lower version": {[]string{"to", "0", "--yes"}, "version not found"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dsn := newDSN(t)
			st := openForTests(t, dsn)
			if err := st.ExecForTests(context.Background(), "INSERT INTO goose_db_version (version_id, is_applied) VALUES (7, 1)"); err != nil {
				t.Fatal(err)
			}
			st.Close()
			_, _, err := run(t, append([]string{"db", "migrate", "--dsn", dsn}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

// The migration tool gives up on a table it cannot read only after its retries, about three seconds,
// so these share one database.
func TestDBMigrateReportsADamagedMigrationTable(t *testing.T) {
	dsn := newDSN(t)
	openForTests(t, dsn).Close() // migrates
	damageMigrationTable(t, dsn, "id INTEGER")
	for _, args := range [][]string{{"version"}, {"status"}, {"down", "--yes"}, {"to", "0"}} {
		if _, _, err := run(t, append([]string{"db", "migrate", "--dsn", dsn}, args...)...); err == nil {
			t.Errorf("db migrate %v: a damaged migration table was not reported", args)
		}
	}
}

func TestDBMigrateRunsWithoutAContextFromTheRunner(t *testing.T) {
	resetFlags(t)
	dsn := newDSN(t)
	dbMigrateCmd.SetContext(nil)
	dbMigrateCmd.Flags().Set("dsn", dsn)
	dbMigrateCmd.SetOut(new(strings.Builder))
	if err := dbMigrateCmd.RunE(dbMigrateCmd, []string{"version"}); err != nil {
		t.Fatal(err)
	}
}

func TestDBCopyMovesEveryRowAndSaysHowMany(t *testing.T) {
	ctx := context.Background()
	from, to := newDSN(t), newDSN(t)
	src := openForTests(t, from)
	if _, err := src.CreateUserWithIdentity(ctx, "alice", "alice@example.com", models.Identity{Provider: models.ProviderLocal, Secret: "h"}); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, "db", "copy", "--from-dsn", from, "--to-driver", "sqlite", "--to-dsn", to)
	if err != nil || !strings.Contains(out, "users:           1") || !strings.Contains(out, "audit events:") {
		t.Fatalf("copy: %q %v", out, err)
	}
	dst := openForTests(t, to, sqlstore.RequireMigrated())
	if _, err := dst.GetUserByEmail(ctx, "alice@example.com"); err != nil {
		t.Fatalf("the user did not arrive: %v", err)
	}
	// The target must be empty: a second copy into it is refused and says so.
	if _, _, err := run(t, "db", "copy", "--from-dsn", from, "--to-driver", "sqlite", "--to-dsn", to); err == nil {
		t.Fatal("a copy into a database that already holds data was accepted")
	}
}

func TestDBCopyRefusesWhatItCannotDo(t *testing.T) {
	migrated := newDSN(t)
	openForTests(t, migrated)
	cases := map[string]struct {
		args []string
		want string
	}{
		"a password in the source": {
			[]string{"--from-dsn", "postgres://u:hunter2hunter2@localhost/x", "--to-driver", "sqlite", "--to-dsn", newDSN(t)}, "contains a password"},
		"a password in the target": {
			[]string{"--from-dsn", migrated, "--to-driver", "postgres", "--to-dsn", "postgres://u:hunter2hunter2@localhost/x"}, "contains a password"},
		"no target": {
			[]string{"--from-dsn", migrated}, "are required"},
		"no source": {
			[]string{"--to-driver", "sqlite", "--to-dsn", newDSN(t)}, "are required"},
		"a source that was never migrated": {
			[]string{"--from-dsn", newDSN(t), "--to-driver", "sqlite", "--to-dsn", newDSN(t)}, "source:"},
		"a target driver that does not exist": {
			[]string{"--from-dsn", migrated, "--to-driver", "oracle", "--to-dsn", newDSN(t)}, "target:"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := run(t, append([]string{"db", "copy"}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestDBCopyRunsWithoutAContextFromTheRunner(t *testing.T) {
	resetFlags(t)
	from := newDSN(t)
	openForTests(t, from)
	dbCopyCmd.SetContext(nil)
	dbCopyCmd.Flags().Set("from-dsn", from)
	dbCopyCmd.Flags().Set("to-driver", "sqlite")
	dbCopyCmd.Flags().Set("to-dsn", newDSN(t))
	dbCopyCmd.SetOut(new(strings.Builder))
	if err := dbCopyCmd.RunE(dbCopyCmd, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDBRekeyRefusesWhatItCannotDo(t *testing.T) {
	oldKey, newKey := strings.Repeat("11", 32), strings.Repeat("22", 32)
	ctx := context.Background()
	k, _ := sqlstore.ParseKey(oldKey)
	c, _ := sqlstore.NewCipher(k)
	encrypted := newDSN(t)
	st, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, encrypted, sqlstore.WithCipher(c))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUserWithIdentity(ctx, "alice", "alice@example.com", models.Identity{Provider: models.ProviderLocal, Secret: "h"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	keyFile := filepath.Join(t.TempDir(), "k.key")
	if err := os.WriteFile(keyFile, []byte(newKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		args []string
		want string
	}{
		"a password in the connection string": {
			[]string{"--dsn", "postgres://u:hunter2hunter2@localhost/x", "--encryption-key-file", keyFile}, "contains a password"},
		"an encryption key that does not exist for a database with encrypted values": {
			[]string{"--dsn", encrypted, "--encryption-key-file", filepath.Join(t.TempDir(), "none.key")}, "already holds encrypted values"},
		"a database that was never migrated": {
			[]string{"--dsn", newDSN(t), "--encryption-key-file", keyFile}, "migrat"},
		"a key that does not match the database": {
			[]string{"--dsn", encrypted, "--encryption-key-file", keyFile, "--dry-run"}, "does not match this database"},
		"the body service's database that was never migrated": {
			[]string{"--body", "--dsn", newDSN(t), "--encryption-key-file", keyFile}, "migrat"},
		"the body service's database with a key file that does not exist and data": {
			[]string{"--body", "--dsn", newDSN(t), "--encryption-key-file", filepath.Join(t.TempDir(), "x", "none.key")}, "encryption key file"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := run(t, append([]string{"db", "rekey"}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestDBRekeyBodyReportsValuesItCannotOpen(t *testing.T) {
	ctx := context.Background()
	dsn := newDSN(t)
	old, _ := sqlstore.ParseKey(strings.Repeat("33", 32))
	oc, _ := sqlstore.NewCipher(old)
	bs, err := sqlstore.OpenBody(ctx, sqlstore.DriverSQLite, dsn, sqlstore.WithCipher(oc))
	if err != nil {
		t.Fatal(err)
	}
	w := 80.0
	if _, err := bs.UpsertScaleReadings(ctx, 1, []models.ScaleReading{{MeasuredAt: time.Now(), WeightKg: &w, Notes: "n"}}); err != nil {
		t.Fatal(err)
	}
	bs.Close()
	t.Setenv(encryptionKeyEnv, strings.Repeat("44", 32)) // the old key is not given as a previous key
	if _, _, err := run(t, "db", "rekey", "--body", "--dsn", dsn, "--dry-run"); err == nil {
		t.Fatal("values that no key given can open were not reported")
	}
}

func TestDBRekeyRunsWithoutAContextFromTheRunner(t *testing.T) {
	resetFlags(t)
	dsn := newDSN(t)
	openForTests(t, dsn)
	dir := t.TempDir()
	dbRekeyCmd.SetContext(nil)
	dbRekeyCmd.Flags().Set("dsn", dsn)
	dbRekeyCmd.Flags().Set("encryption-key-file", filepath.Join(dir, "k.key"))
	dbRekeyCmd.Flags().Set("dry-run", "true")
	var out strings.Builder
	dbRekeyCmd.SetOut(&out)
	if err := dbRekeyCmd.RunE(dbRekeyCmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "dry run: nothing was changed") {
		t.Errorf("rekey output: %q", out.String())
	}
}

func TestDBProvisionRefusesTwoRolesWithTheSamePassword(t *testing.T) {
	resetFlags(t)
	same := writeSecretFile(t, "the-same-password\n")
	dsn := writeSecretFile(t, "postgres://postgres@127.0.0.1:1/postgres\n")
	dbProvisionCmd.SetContext(nil)
	dbProvisionCmd.Flags().Set("admin-dsn-file", dsn)
	dbProvisionCmd.Flags().Set("app-password-file", same)
	dbProvisionCmd.Flags().Set("body-password-file", same)
	err := dbProvisionCmd.RunE(dbProvisionCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "different passwords") {
		t.Fatalf("%v", err)
	}
}
