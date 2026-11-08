package sqlstore

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/store"
)

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations/"+dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// Every schema change has to exist for both backends, otherwise one of them silently drifts.
func TestMigrationParity(t *testing.T) {
	a, b := names(t, DriverSQLite), names(t, DriverPostgres)
	if len(a) == 0 || !reflect.DeepEqual(a, b) {
		t.Fatalf("migration files differ: sqlite=%v postgres=%v", a, b)
	}
}

func TestMigrateRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	latest, err := s.SchemaVersion(ctx)
	if err != nil || latest == 0 {
		t.Fatalf("version after open: %d %v", latest, err)
	}
	if _, err := s.MigrateTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 0 {
		t.Fatalf("version after down = %d", v)
	}
	if _, err := s.CreateUser(ctx, "x"); err == nil {
		t.Fatal("expected failure with no schema")
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := s.CreateUser(ctx, "x"); err != nil {
		t.Fatalf("create after re-migrate: %v", err)
	}
}

func TestRefusesNewerDatabase(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "n.db")
	s, err := Open(ctx, DriverSQLite, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (9999, 1)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, DriverSQLite, dsn); err == nil {
		t.Fatal("expected error for database newer than binary")
	}
}

func TestRequireMigrated(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "r.db")
	if _, err := Open(ctx, DriverSQLite, dsn, RequireMigrated()); err == nil {
		t.Fatal("expected error for unmigrated database")
	}
	s, err := Open(ctx, DriverSQLite, dsn)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, DriverSQLite, dsn, RequireMigrated())
	if err != nil {
		t.Fatalf("migrated database should open: %v", err)
	}
	s.Close()
}

func TestForeignKeysEnforcedWithoutPragmaInDSN(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, DriverSQLite, "file:"+filepath.Join(t.TempDir(), "fk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutIntegration(ctx, 12345, "withings", []byte(`{}`)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound from FK violation, got %v", err)
	}
}

// Rebuilding the users table must not cascade into the rows that reference it.
func TestEmailKeyMigrationKeepsData(t *testing.T) {
	ctx := context.Background()
	s := openFile(t, filepath.Join(t.TempDir(), "m.db"))
	if _, err := s.MigrateTo(ctx, 2); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, name, created_at) VALUES (1, 'a', '2026-01-01 00:00:00')`,
		`INSERT INTO scale_readings (user_id, measured_at, created_at) VALUES (1, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`,
		`INSERT INTO sessions (token_hash, user_id, method, created_at, expires_at) VALUES ('h', 1, 'local', '2026-01-01 00:00:00', '2099-01-01 00:00:00')`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"users", "scale_readings", "sessions"} {
		if got := rawColumn(t, s, `SELECT COUNT(*) FROM `+table); got != "1" {
			t.Errorf("%s has %s rows after the migration", table, got)
		}
	}
	if got := rawColumn(t, s, `PRAGMA foreign_keys`); got != "1" {
		t.Errorf("foreign keys left off: %s", got)
	}
	if _, err := s.CreateUser(ctx, "a"); err != nil { // the same name is allowed now
		t.Fatalf("same name: %v", err)
	}
	if err := s.DeleteUser(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if got := rawColumn(t, s, `SELECT COUNT(*) FROM scale_readings`); got != "0" {
		t.Errorf("cascade stopped working: %s rows left", got)
	}
}
