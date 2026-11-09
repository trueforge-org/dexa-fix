package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

func TestKeyFileBesideDatabase(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	tests := []struct {
		name            string
		driver, dsn, kf string
		want            bool
	}{
		{"same directory", "sqlite", "file:" + filepath.Join(dir, "d.db"), filepath.Join(dir, "d.key"), true},
		{"dsn with options", "sqlite", "file:" + filepath.Join(dir, "d.db") + "?_pragma=busy_timeout(1)", filepath.Join(dir, "d.key"), true},
		{"dsn with slashes", "sqlite", "file://" + filepath.Join(dir, "d.db"), filepath.Join(dir, "d.key"), true},
		{"separate mount", "sqlite", "file:" + filepath.Join(dir, "d.db"), filepath.Join(other, "d.key"), false},
		{"postgres", "postgres", "postgres://localhost/x", filepath.Join(dir, "d.key"), false},
		{"in memory", "sqlite", "file::memory:", filepath.Join(dir, "d.key"), false},
	}
	for _, tt := range tests {
		if got := keyFileBesideDatabase(tt.driver, tt.dsn, tt.kf); got != tt.want {
			t.Errorf("%s: got %v", tt.name, got)
		}
	}

	// Relative paths in the working directory, as with the defaults.
	t.Chdir(dir)
	if !keyFileBesideDatabase("sqlite", "file:dexa-fix.db", "dexa-fix.key") {
		t.Error("relative defaults not detected")
	}
}

func TestMissingKeyFileIsNotReplacedWhenDatabaseIsEncrypted(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "d.db")
	keyPath := filepath.Join(dir, "d.key")
	ctx := context.Background()

	// A new database: the key is created without steps.
	hasData := func() (bool, error) { return sqlstore.HasEncryptedValues(ctx, sqlstore.DriverSQLite, dsn) }
	key, created, err := resolveEncryptionKey("", keyPath, hasData)
	if err != nil || !created {
		t.Fatalf("fresh database: %v %v", created, err)
	}
	c, err := sqlstore.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, dsn, sqlstore.WithCipher(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveEncryptionKey("", keyPath, hasData); err == nil || !strings.Contains(err.Error(), "already holds encrypted values") {
		t.Fatalf("missing key file: %v", err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key file was created: %v", err)
	}
}

func TestExistingPlaintextDatabaseStillGetsAKey(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "d.db")
	ctx := context.Background()
	plain, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.PutSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	plain.Close()
	hasData := func() (bool, error) { return sqlstore.HasEncryptedValues(ctx, sqlstore.DriverSQLite, dsn) }
	if _, created, err := resolveEncryptionKey("", filepath.Join(dir, "d.key"), hasData); err != nil || !created {
		t.Fatalf("plaintext database: %v %v", created, err)
	}
}

func TestResolvePreviousKeys(t *testing.T) {
	a, b, c := strings.Repeat("aa", 32), strings.Repeat("bb", 32), strings.Repeat("cc", 32)
	file := filepath.Join(t.TempDir(), "prev")
	if err := os.WriteFile(file, []byte("# old keys\n"+c+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := resolvePreviousKeys(a+" , "+b+",", file)
	if err != nil || len(keys) != 3 {
		t.Fatalf("keys: %d %v", len(keys), err)
	}
	if none, err := resolvePreviousKeys("", ""); err != nil || len(none) != 0 {
		t.Fatalf("none: %d %v", len(none), err)
	}
	if _, err := resolvePreviousKeys("nope", ""); err == nil {
		t.Error("bad key accepted")
	}
	if _, err := resolvePreviousKeys("", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestRekeyCommandEndToEnd(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "d.db")
	oldKey, newKey := strings.Repeat("11", 32), strings.Repeat("22", 32)
	ctx := context.Background()

	k, _ := sqlstore.ParseKey(oldKey)
	c, _ := sqlstore.NewCipher(k)
	st, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, dsn, sqlstore.WithCipher(c))
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUserWithIdentity(ctx, "alice", "alice@example.com", models.Identity{Provider: models.ProviderLocal, Secret: "h"})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		RootCmd.SetOut(&out)
		RootCmd.SetArgs(append([]string{"db", "rekey", "--dsn", dsn, "--encryption-key-file", filepath.Join(dir, "none.key")}, args...))
		if err := RootCmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("rekey %v: %v", args, err)
		}
		return out.String()
	}
	t.Setenv(encryptionKeyEnv, newKey)
	t.Setenv(encryptionKeyPreviousEnv, oldKey)
	if out := run("--dry-run"); !strings.Contains(out, "dry run") || !strings.Contains(out, "e-mail addresses to re-encrypt: 1") {
		t.Fatalf("dry run output: %q", out)
	}
	run("--dry-run=false")
	if out := run("--dry-run=false"); !strings.Contains(out, "e-mail addresses re-encrypted: 0") || !strings.Contains(out, "scale readings and DEXA scans re-encrypted: 0") {
		t.Fatalf("second run: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "none.key")); !os.IsNotExist(err) {
		t.Fatal("a key file was created")
	}

	// Only the new key is needed afterwards.
	nk, _ := sqlstore.ParseKey(newKey)
	nc, _ := sqlstore.NewCipher(nk)
	after, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, dsn, sqlstore.WithCipher(nc))
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	if got, err := after.GetUserByEmail(ctx, "alice@example.com"); err != nil || got.ID != u.ID {
		t.Fatalf("sign-in lookup: %+v %v", got, err)
	}
}
