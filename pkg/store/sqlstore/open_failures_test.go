package sqlstore

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trueforge-org/dexafix/pkg/models"
)

func fileDSN(t *testing.T) string {
	t.Helper()
	return "file:" + filepath.Join(t.TempDir(), "db.sqlite")
}

// refuseWrites makes SQLite refuse the kind of write named, on one table, for as long as the file lives.
func refuseWrites(t *testing.T, dsn, event, table string) {
	t.Helper()
	s, err := Open(context.Background(), DriverSQLite, dsn, WithoutMigrate())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	q := `CREATE TRIGGER refuse BEFORE ` + event + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'writes refused'); END`
	if err := s.ExecForTests(context.Background(), q); err != nil {
		t.Fatal(err)
	}
}

// Starting with a key is all or nothing: when the check or the encryption of old rows cannot be
// written, the process does not start, rather than run with half-encrypted data.
func TestStartingWithAKeyFailsWhenTheDatabaseRefusesTheWrites(t *testing.T) {
	ctx := context.Background()
	key := WithCipher(testCipher(t, 1))
	for _, c := range []struct {
		name      string
		prepare   func(*testing.T, *Store)
		event     string
		table     string
		wantError string
	}{
		{"the key check of the main database", func(*testing.T, *Store) {}, "INSERT", "app_settings", "writes refused"},
		{"the key check of the readings", func(*testing.T, *Store) {}, "INSERT", "body_meta", "writes refused"},
		{"a setting written before the key", func(t *testing.T, s *Store) {
			if err := s.PutSetting(ctx, "k", "v"); err != nil {
				t.Fatal(err)
			}
		}, "UPDATE", "app_settings", "encrypt existing data"},
		{"a scan written before the key", func(t *testing.T, s *Store) {
			u, _ := s.CreateUser(ctx, "u")
			if err := s.CreateDexaScan(ctx, &models.DexaScan{UserID: u.ID, MeasuredAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}, "UPDATE", "dexa_scans", "encrypt existing readings and scans"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dsn := fileDSN(t)
			s, err := Open(ctx, DriverSQLite, dsn)
			if err != nil {
				t.Fatal(err)
			}
			c.prepare(t, s)
			s.Close()
			refuseWrites(t, dsn, c.event, c.table)
			if _, err := Open(ctx, DriverSQLite, dsn, key); err == nil || !strings.Contains(err.Error(), c.wantError) {
				t.Fatalf("got %v, want an error with %q", err, c.wantError)
			}
		})
	}
}

// The same for a database that holds only the readings and scans.
func TestTheBodyDatabaseStartsWithAKeyOnlyWhenItsWritesWork(t *testing.T) {
	ctx := context.Background()
	key := WithCipher(testCipher(t, 1))
	t.Run("the key check", func(t *testing.T) {
		dsn := fileDSN(t)
		b, err := OpenBody(ctx, DriverSQLite, dsn)
		if err != nil {
			t.Fatal(err)
		}
		b.Close()
		refuseWrites(t, dsn, "INSERT", "body_meta")
		if _, err := OpenBody(ctx, DriverSQLite, dsn, key); err == nil || !strings.Contains(err.Error(), "writes refused") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a scan written before the key", func(t *testing.T) {
		dsn := fileDSN(t)
		b, err := OpenBody(ctx, DriverSQLite, dsn)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.CreateDexaScan(ctx, &models.DexaScan{UserID: 1, MeasuredAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		b.Close()
		refuseWrites(t, dsn, "UPDATE", "dexa_scans")
		if _, err := OpenBody(ctx, DriverSQLite, dsn, key); err == nil || !strings.Contains(err.Error(), "encrypt existing data") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a database from a newer version", func(t *testing.T) {
		dsn := fileDSN(t)
		b, err := OpenBody(ctx, DriverSQLite, dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.db.ExecContext(ctx, `INSERT INTO `+bodyMigrationTable+` (version_id, is_applied) VALUES (9999, 1)`); err != nil {
			t.Fatal(err)
		}
		b.Close()
		if _, err := OpenBody(ctx, DriverSQLite, dsn); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("pending migrations when it may not migrate", func(t *testing.T) {
		if _, err := OpenBody(ctx, DriverSQLite, fileDSN(t), RequireMigrated()); err == nil || !strings.Contains(err.Error(), "pending migrations") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a database that cannot be reached", func(t *testing.T) {
		if _, err := OpenBody(ctx, "oracle", "x"); err == nil {
			t.Fatal("an unknown driver was accepted")
		}
	})
	t.Run("a pool that the store does not own stays open", func(t *testing.T) {
		s := newStore(t)
		if err := s.BodyStore.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.BodyStore.Ping(ctx); err != nil {
			t.Fatalf("closing the body part closed the shared pool: %v", err)
		}
		if got := s.BodyStore.MaxConns(); got != s.MaxConns() || got == 0 {
			t.Fatalf("MaxConns %d, store %d", got, s.MaxConns())
		}
	})
}

func TestThePostgreSQLPoolIsSetUpEvenWhenTheServerCannotBeReached(t *testing.T) {
	ctx := context.Background()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nobody listens there now
	start := time.Now()
	if _, err := Open(ctx, DriverPostgres, "postgres://u:p@"+addr+"/db?sslmode=disable&connect_timeout=2"); err == nil {
		t.Fatal("opened a database nobody listens for")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	if _, err := Open(ctx, DriverPostgres, "postgres://u@host:notaport/db"); err == nil {
		t.Fatal("a connection string that cannot be parsed was accepted")
	}
	if _, err := Open(ctx, "oracle", "x"); err == nil || !strings.Contains(err.Error(), "unsupported db driver") {
		t.Fatalf("unknown driver: %v", err)
	}
}

func TestTheFileOfASQLiteURLIsFoundWhateverItsForm(t *testing.T) {
	for dsn, want := range map[string]string{
		"file:/var/db/x.sqlite?mode=rwc":                "/var/db/x.sqlite",
		"file:relative.db":                              "relative.db",
		"file:///var/db/x.sqlite":                       "/var/db/x.sqlite",
		"file://localhost/var/db/x.sqlite?_pragma=a(1)": "/var/db/x.sqlite",
		"file::memory:?cache=shared":                    "",
	} {
		if got := sqliteFilePath(dsn); got != want {
			t.Errorf("sqliteFilePath(%q) = %q, want %q", dsn, got, want)
		}
	}
}

// ---- migrations ----

func TestMigrationsGoDownAndUpAgain(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	latest, err := s.SchemaVersion(ctx)
	if err != nil || latest == 0 {
		t.Fatalf("%d %v", latest, err)
	}
	if res, err := s.MigrateTo(ctx, latest); err != nil || res != nil {
		t.Fatalf("migrating to the version it is at did something: %v %v", res, err)
	}
	st, err := s.MigrationStatus(ctx)
	if err != nil || len(st) == 0 || st[len(st)-1].State != "applied" {
		t.Fatalf("status %v %v", st, err)
	}
	if res, err := s.MigrateDown(ctx); err != nil || res.Source.Version != latest {
		t.Fatalf("down: %+v %v", res, err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 0 {
		t.Fatalf("version after down: %d", v)
	}
	if res, err := s.MigrateTo(ctx, latest); err != nil || len(res) == 0 {
		t.Fatalf("up: %v %v", res, err)
	}
	if v, _ := s.SchemaVersion(ctx); v != latest {
		t.Fatalf("version after up: %d", v)
	}
}

func TestADatabaseFromANewerVersionIsRefusedByBothDomains(t *testing.T) {
	ctx := context.Background()
	for _, table := range []string{"goose_db_version", bodyMigrationTable} {
		t.Run(table, func(t *testing.T) {
			s, _ := newFaultyStore(t, nil)
			if err := s.ExecForTests(ctx, `INSERT INTO `+table+` (version_id, is_applied) VALUES (9999, 1)`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
				t.Fatalf("Migrate: %v", err)
			}
			if _, err := s.BodyStore.Migrate(ctx); table == bodyMigrationTable && (err == nil || !strings.Contains(err.Error(), "newer than this binary")) {
				t.Fatalf("body Migrate: %v", err)
			}
		})
	}
}

func TestHasPendingSeesAPendingBodyMigration(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	if pending, err := s.HasPending(ctx); err != nil || pending {
		t.Fatalf("fresh store: %v %v", pending, err)
	}
	if pending, err := s.BodyStore.HasPending(ctx); err != nil || pending {
		t.Fatalf("body: %v %v", pending, err)
	}
	if err := s.ExecForTests(ctx, `DELETE FROM `+bodyMigrationTable+` WHERE version_id > 0`); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.HasPending(ctx); err != nil || !pending {
		t.Fatalf("after forgetting the body migration: %v %v", pending, err)
	}
	if v, err := s.BodyStore.SchemaVersion(ctx); err != nil || v != 0 {
		t.Fatalf("body version %d %v", v, err)
	}
}

func TestAStoreWithoutTheBodyHasNothingPendingThere(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, DriverSQLite, fileDSN(t), WithoutBody())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if pending, err := s.HasPending(ctx); err != nil || pending {
		t.Fatalf("%v %v", pending, err)
	}
}

// ---- Copy ----

func TestCopyRefusesDatabasesAtDifferentVersions(t *testing.T) {
	ctx := context.Background()
	src, _ := newFaultyStore(t, nil)
	dst, _ := newFaultyStore(t, nil)
	if _, err := dst.MigrateTo(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, src, dst); err == nil || !strings.Contains(err.Error(), "schema versions differ") {
		t.Fatalf("got %v", err)
	}
}

// A table that cannot be read from the source, or written to the target, names itself in the error and
// leaves the target as empty as it was.
func TestCopyNamesTheTableThatFailed(t *testing.T) {
	ctx := context.Background()
	tables := []struct{ label, table string }{
		{"users", "users"}, {"user_integrations", "user_integrations"}, {"user_identities", "user_identities"},
		{"passkeys", "passkeys"}, {"user_totp", "user_totp"}, {"user_recovery_codes", "user_recovery_codes"},
		{"access_tokens", "access_tokens"}, {"app_settings", "app_settings"}, {"auth_providers", "auth_providers"},
		{"audit_log", "audit_log"}, {"dexa_scans", "dexa_scans"}, {"scale_readings", "scale_readings"},
		{"body_user_state", "body_user_state"}, {"body_meta", "body_meta"},
	}
	for _, tc := range tables {
		t.Run("the source cannot read "+tc.table, func(t *testing.T) {
			src, _ := newFaultyStore(t, nil)
			dst, _ := newFaultyStore(t, nil)
			if err := src.ExecForTests(ctx, `ALTER TABLE `+tc.table+` RENAME TO gone`); err != nil {
				t.Fatal(err)
			}
			if _, err := Copy(ctx, src, dst); err == nil || !strings.HasPrefix(err.Error(), tc.label+":") {
				t.Fatalf("got %v, want an error that starts with %q", err, tc.label)
			}
		})
	}
	t.Run("the target refuses the rows", func(t *testing.T) {
		src, _ := newFaultyStore(t, nil)
		seedFaultData(t, src)
		dst, _ := newFaultyStore(t, nil)
		if err := dst.ExecForTests(ctx, `CREATE TRIGGER refuse BEFORE INSERT ON users BEGIN SELECT RAISE(ABORT, 'writes refused'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := Copy(ctx, src, dst); err == nil || !strings.Contains(err.Error(), "writes refused") {
			t.Fatalf("got %v", err)
		}
	})
	for _, table := range []string{"app_settings", "body_meta"} {
		t.Run("the target refuses to drop the old key check of "+table, func(t *testing.T) {
			src, _ := newFaultyStore(t, nil)
			dst, _ := newFaultyStore(t, nil)
			// The target has a key, so it has a check of its own, and the copy has to make room for the source's.
			if err := dst.ExecForTests(ctx, `INSERT INTO `+table+` (name, value, updated_at) VALUES ('crypto.check', 'x', CURRENT_TIMESTAMP)`); err != nil {
				t.Fatal(err)
			}
			if err := dst.ExecForTests(ctx, `CREATE TRIGGER refuse BEFORE DELETE ON `+table+` BEGIN SELECT RAISE(ABORT, 'writes refused'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := Copy(ctx, src, dst); err == nil || !strings.HasPrefix(err.Error(), table+":") {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, c := range []struct{ name, label string }{
		{"user_integrations", "user_integrations"}, {"user_identities", "user_identities"}, {"passkeys", "passkeys"},
		{"user_totp", "user_totp"}, {"user_recovery_codes", "user_recovery_codes"}, {"access_tokens", "access_tokens"},
		{"auth_providers", "auth_providers"}, {"audit_log", "audit_log"}, {"dexa_scans", "dexa_scans"},
		{"scale_readings", "scale_readings"}, {"body_user_state", "body_user_state"}, {"body_meta", "body_meta"},
	} {
		t.Run("the target refuses the rows of "+c.name, func(t *testing.T) {
			src, _ := newFaultyStore(t, nil)
			fx := seedFaultData(t, src)
			// The key checks only exist on a store that has a key, so make one for the table that needs it.
			if c.name == "body_meta" {
				if err := src.ExecForTests(ctx, `INSERT INTO body_meta (name, value, updated_at) VALUES ('x', 'y', CURRENT_TIMESTAMP)`); err != nil {
					t.Fatal(err)
				}
			}
			_ = fx
			dst, _ := newFaultyStore(t, nil)
			if err := dst.ExecForTests(ctx, `CREATE TRIGGER refuse BEFORE INSERT ON `+c.name+` BEGIN SELECT RAISE(ABORT, 'writes refused'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := Copy(ctx, src, dst); err == nil || !strings.HasPrefix(err.Error(), c.label+":") {
				t.Fatalf("got %v, want an error that starts with %q", err, c.label)
			}
		})
	}
	t.Run("the target refuses the settings", func(t *testing.T) {
		src, _ := newFaultyStore(t, nil)
		seedFaultData(t, src)
		dst, _ := newFaultyStore(t, nil)
		if err := dst.ExecForTests(ctx, `CREATE TRIGGER refuse BEFORE INSERT ON app_settings BEGIN SELECT RAISE(ABORT, 'writes refused'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := Copy(ctx, src, dst); err == nil || !strings.HasPrefix(err.Error(), "app_settings:") {
			t.Fatalf("got %v", err)
		}
	})
}

// ---- the key check ----

func TestAWrongKeyIsRefusedWithoutAKeyCheckToCompareTo(t *testing.T) {
	ctx := context.Background()
	for _, domain := range []string{"main", "body"} {
		t.Run(domain, func(t *testing.T) {
			s, _ := newFaultyStore(t, testCipher(t, 1))
			keyedSeed(t, s)
			verify, clear := s.verifyKey, `DELETE FROM app_settings WHERE name = 'crypto.check'`
			if domain == "body" {
				verify, clear = s.BodyStore.verifyKey, `DELETE FROM body_meta`
			}
			if err := s.ExecForTests(ctx, clear); err != nil {
				t.Fatal(err)
			}
			// A key that made none of the stored values.
			withKeys(t, s, 3)
			if _, err := verify(ctx); err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("wrong key: %v", err)
			}
			// The key the values were made with, now only a previous one: accepted, with nothing to write.
			withKeys(t, s, 2, 1)
			if write, err := verify(ctx); err != nil || write != nil {
				t.Fatalf("previous key: write %v, err %v", write != nil, err)
			}
		})
	}
}

func TestStatsOfAKeyedStoreWithoutAKeyCheckStillWork(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, testCipher(t, 1))
	if st, err := s.Stats(ctx); err != nil || !st.Encrypted {
		t.Fatalf("%+v %v", st, err)
	}
}

// Each kind of encrypted value is found, one at a time, by the search that decides whether a key is
// needed at all.
func TestTheSearchForAnEncryptedValueFindsEveryKind(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		put  func(*Store, models.User) error
		ctx  func(models.User) string
	}{
		{"an integration", func(s *Store, u models.User) error { return s.PutIntegration(ctx, u.ID, "withings", []byte("{}")) },
			func(u models.User) string { return integrationContext(u.ID, "withings") }},
		{"an e-mail address", func(s *Store, u models.User) error { return s.UpdateUserProfile(ctx, u.ID, "u", "u@example.com", nil) },
			func(u models.User) string { return emailContext(u.ID) }},
		{"a two-step secret", func(s *Store, u models.User) error { return s.PutPendingTOTP(ctx, u.ID, "SECRET") },
			func(u models.User) string { return totpContext(u.ID) }},
		{"a provider secret", func(s *Store, u models.User) error {
			return s.PutAuthProvider(ctx, models.AuthProvider{Slug: "g", Label: "G", Kind: models.ProviderKindOIDC, ClientID: "c", ClientSecret: "s"})
		}, func(models.User) string { return providerContext("g") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newFaultyStore(t, testCipher(t, 1))
			u, err := s.CreateUser(ctx, "u")
			if err != nil {
				t.Fatal(err)
			}
			if err := c.put(s, u); err != nil {
				t.Fatal(err)
			}
			value, sealCtx, found, err := firstEncrypted(ctx, s.db)
			if err != nil || !found || !strings.HasPrefix(value, encPrefix) || sealCtx != c.ctx(u) {
				t.Fatalf("found %v, context %q (want %q), %v", found, sealCtx, c.ctx(u), err)
			}
		})
	}
	t.Run("a scan", func(t *testing.T) {
		s, _ := newFaultyStore(t, testCipher(t, 1))
		if err := s.CreateDexaScan(ctx, &models.DexaScan{UserID: 7, MeasuredAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		_, sealCtx, found, err := firstEncryptedBody(ctx, s.db)
		if err != nil || !found || !strings.HasPrefix(sealCtx, "dexa:7:") {
			t.Fatalf("found %v, context %q, %v", found, sealCtx, err)
		}
	})
}

func TestTheSearchForEncryptedValuesWithoutOpeningTheStore(t *testing.T) {
	ctx := context.Background()
	if _, err := HasEncryptedValues(ctx, "oracle", "x"); err == nil {
		t.Error("an unknown driver was accepted")
	}
	if _, err := HasEncryptedBodyValues(ctx, "oracle", "x"); err == nil {
		t.Error("an unknown driver was accepted for the body database")
	}
	// An empty file has no tables, so nothing can be encrypted in it.
	if found, err := HasEncryptedValues(ctx, DriverSQLite, fileDSN(t)); err != nil || found {
		t.Errorf("empty database: %v %v", found, err)
	}
	// Any other failure is passed on: here the table is there but not readable as expected.
	dsn := fileDSN(t)
	s, err := Open(ctx, DriverSQLite, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ExecForTests(ctx, `ALTER TABLE user_totp RENAME COLUMN secret TO other`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := HasEncryptedValues(ctx, DriverSQLite, dsn); err == nil {
		t.Error("a broken table was taken for an empty one")
	}
}

// ---- PostgreSQL helpers that need no server ----

func TestTheProvisionHelpersThatNeedNoServer(t *testing.T) {
	if got := quoteIdent(`we"ird`); got != `"we""ird"` {
		t.Errorf("quoteIdent: %s", got)
	}
	db, err := openPG("postgres://u:p@127.0.0.1:1/db", nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	var database string
	db, err = openPG("postgres://u:p@127.0.0.1:1/db", func(c *pgx.ConnConfig) { database = c.Database; c.Database = "other" })
	if err != nil || database != "db" {
		t.Fatalf("%v %q", err, database)
	}
	db.Close()
	if _, err := openPG("postgres://u@host:notaport/db", nil); err == nil {
		t.Error("a connection string that cannot be parsed was accepted")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	err = Provision(context.Background(), ProvisionOptions{AdminDSN: "postgres://postgres:x@" + addr + "/postgres?sslmode=disable&connect_timeout=2", AppPassword: "a", BodyPassword: "b"})
	if err == nil || !strings.Contains(err.Error(), "connect as the administrator") {
		t.Errorf("Provision against nothing: %v", err)
	}
}
