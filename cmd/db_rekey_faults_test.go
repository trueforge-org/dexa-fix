package cmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

func TestDBRekeyStopsWhenAValueCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	oldKey, newKey := strings.Repeat("11", 32), strings.Repeat("22", 32)
	k, _ := sqlstore.ParseKey(oldKey)
	c, _ := sqlstore.NewCipher(k)
	t.Setenv(encryptionKeyEnv, newKey)
	t.Setenv(encryptionKeyPreviousEnv, oldKey)
	none := filepath.Join(t.TempDir(), "none.key")

	t.Run("in the main database", func(t *testing.T) {
		dsn := newDSN(t)
		st, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, dsn, sqlstore.WithCipher(c))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateUserWithIdentity(ctx, "alice", "alice@example.com", models.Identity{Provider: models.ProviderLocal, Secret: "h"}); err != nil {
			t.Fatal(err)
		}
		if err := st.ExecForTests(ctx, "CREATE TRIGGER keep_users BEFORE UPDATE ON users WHEN NEW.email <> OLD.email BEGIN SELECT RAISE(ABORT, 'kept'); END"); err != nil {
			t.Fatal(err)
		}
		st.Close()
		_, _, err = run(t, "db", "rekey", "--dsn", dsn, "--encryption-key-file", none)
		if err == nil || !strings.Contains(err.Error(), "kept") {
			t.Fatalf("a rekey that could not write was reported as done: %v", err)
		}
	})
	t.Run("in the body service's database", func(t *testing.T) {
		dsn := newDSN(t)
		bs, err := sqlstore.OpenBody(ctx, sqlstore.DriverSQLite, dsn, sqlstore.WithCipher(c))
		if err != nil {
			t.Fatal(err)
		}
		w := 80.0
		if _, err := bs.UpsertScaleReadings(ctx, 1, []models.ScaleReading{{MeasuredAt: time.Now(), WeightKg: &w, Notes: "n"}}); err != nil {
			t.Fatal(err)
		}
		bs.Close()
		raw, err := sql.Open("sqlite", dsn) // the body store does not expose its connection
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, "CREATE TRIGGER keep_readings BEFORE UPDATE ON scale_readings BEGIN SELECT RAISE(ABORT, 'kept'); END"); err != nil {
			t.Fatal(err)
		}
		raw.Close()
		_, _, err = run(t, "db", "rekey", "--body", "--dsn", dsn, "--encryption-key-file", none)
		if err == nil || !strings.Contains(err.Error(), "kept") {
			t.Fatalf("a rekey that could not write was reported as done: %v", err)
		}
	})
}
