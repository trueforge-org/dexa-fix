package sqlstore

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

func testCipher(t *testing.T, fill byte) *Cipher {
	t.Helper()
	c, err := NewCipher(bytes.Repeat([]byte{fill}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func openFile(t *testing.T, path string, opts ...Option) *Store {
	t.Helper()
	s, err := Open(context.Background(), DriverSQLite, "file:"+path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func rawColumn(t *testing.T, s *Store, query string) string {
	t.Helper()
	var v string
	if err := s.db.NewRaw(query).Scan(context.Background(), &v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

const (
	rawIntegration = `SELECT data FROM user_integrations LIMIT 1`
	rawSetting     = `SELECT value FROM app_settings WHERE name = 'k'`
	rawProvider    = `SELECT client_secret FROM auth_providers LIMIT 1`
	rawEmail       = `SELECT email FROM users LIMIT 1`
)

// fillSensitive stores one value of every encrypted kind for user id.
func fillSensitive(t *testing.T, s *Store) models.User {
	t.Helper()
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUserProfile(ctx, u.ID, "alice", "alice@example.com", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIntegration(ctx, u.ID, "withings", []byte(`{"access_token":"plain-token"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(ctx, "k", "plain-setting"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAuthProvider(ctx, models.AuthProvider{Slug: "g", Label: "G", Kind: models.ProviderKindOIDC, ClientID: "c", ClientSecret: "plain-secret"}); err != nil {
		t.Fatal(err)
	}
	return u
}

func checkReadable(t *testing.T, s *Store, userID int64) {
	t.Helper()
	ctx := context.Background()
	if u, err := s.GetUser(ctx, userID); err != nil || u.Email != "alice@example.com" {
		t.Errorf("email: %q %v", u.Email, err)
	}
	if b, err := s.GetIntegration(ctx, userID, "withings"); err != nil || !strings.Contains(string(b), "plain-token") {
		t.Errorf("integration: %q %v", b, err)
	}
	if v, err := s.GetSetting(ctx, "k"); err != nil || v != "plain-setting" {
		t.Errorf("setting: %q %v", v, err)
	}
	if p, err := s.GetAuthProvider(ctx, "g"); err != nil || p.ClientSecret != "plain-secret" {
		t.Errorf("provider: %q %v", p.ClientSecret, err)
	}
	list, err := s.ListAuthProviders(ctx)
	if err != nil || len(list) != 1 || list[0].ClientSecret != "plain-secret" {
		t.Errorf("provider list: %+v %v", list, err)
	}
	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 1 || users[0].Email != "alice@example.com" {
		t.Errorf("user list: %+v %v", users, err)
	}
}

func checkEncrypted(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range []string{rawIntegration, rawSetting, rawProvider, rawEmail} {
		v := rawColumn(t, s, q)
		if !strings.HasPrefix(v, encPrefix) || strings.Contains(v, "plain") || strings.Contains(v, "alice") {
			t.Errorf("%s is not encrypted: %q", q, v)
		}
	}
}

func TestSensitiveColumnsAreEncrypted(t *testing.T) {
	s := openFile(t, filepath.Join(t.TempDir(), "a.db"), WithCipher(testCipher(t, 1)))
	u := fillSensitive(t, s)
	checkEncrypted(t, s)
	checkReadable(t, s, u.ID)
}

func TestExistingValuesAreEncryptedOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	plain := openFile(t, path)
	u := fillSensitive(t, plain)
	if v := rawColumn(t, plain, rawSetting); v != "plain-setting" {
		t.Fatalf("setup: %q", v)
	}
	plain.Close()

	s := openFile(t, path, WithCipher(testCipher(t, 1)))
	checkEncrypted(t, s)
	checkReadable(t, s, u.ID)
	if u2, err := s.GetUserByEmail(context.Background(), "alice@example.com"); err != nil || u2.ID != u.ID {
		t.Errorf("lookup after backfill: %+v %v", u2, err)
	}
	before := rawColumn(t, s, rawSetting)
	s.Close()

	again := openFile(t, path, WithCipher(testCipher(t, 1))) // already encrypted values stay as they are
	if after := rawColumn(t, again, rawSetting); after != before {
		t.Error("value was encrypted twice")
	}
	checkReadable(t, again, u.ID)
}

func TestWrongOrMissingKeyCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	s := openFile(t, path, WithCipher(testCipher(t, 1)))
	fillSensitive(t, s)
	s.Close()
	ctx := context.Background()

	if _, err := Open(ctx, DriverSQLite, "file:"+path, WithCipher(testCipher(t, 2))); err == nil || !strings.Contains(err.Error(), "encryption key") {
		t.Errorf("wrong key: %v", err)
	}

	none := openFile(t, path)
	if _, err := none.GetIntegration(ctx, 1, "withings"); err == nil || !strings.Contains(err.Error(), "no encryption key") {
		t.Errorf("no key: %v", err)
	}
}

func TestEmailIsUniqueAndSearchableWhileEncrypted(t *testing.T) {
	ctx := context.Background()
	s := openFile(t, filepath.Join(t.TempDir(), "a.db"), WithCipher(testCipher(t, 1)))
	id := models.Identity{Provider: models.ProviderLocal, Secret: "h"}
	u, err := s.CreateUserWithIdentity(ctx, "a", "Same@Example.com", id)
	if err != nil || u.Email != "same@example.com" {
		t.Fatalf("create: %+v %v", u, err)
	}
	if lookup := rawColumn(t, s, `SELECT email_lookup FROM users LIMIT 1`); strings.Contains(lookup, "same") {
		t.Fatalf("lookup is readable: %q", lookup)
	}
	if _, err := s.CreateUserWithIdentity(ctx, "b", "same@example.com", id); !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := s.GetUserByEmail(ctx, " SAME@example.COM ")
	if err != nil || got.ID != u.ID || got.Email != "same@example.com" {
		t.Fatalf("find: %+v %v", got, err)
	}
	other, err := s.CreateUser(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUserProfile(ctx, other.ID, "c", "same@example.com", nil); !errors.Is(err, store.ErrExists) {
		t.Fatalf("taken on update: %v", err)
	}
	if _, err := s.GetUserByEmail(ctx, "nobody@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestEmailBackfillClearsDuplicates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a.db")
	s := openFile(t, path)
	a, _ := s.CreateUser(ctx, "a")
	b, _ := s.CreateUser(ctx, "b")
	for _, id := range []int64{a.ID, b.ID} { // as written before addresses had to be unique
		if _, err := s.db.ExecContext(ctx, `UPDATE users SET email = 'x@example.com', email_lookup = NULL WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	again := openFile(t, path, WithCipher(testCipher(t, 1)))
	if got, err := again.GetUserByEmail(ctx, "x@example.com"); err != nil || got.ID != a.ID {
		t.Fatalf("oldest keeps it: %+v %v", got, err)
	}
	if got, _ := again.GetUser(ctx, b.ID); got.Email != "" {
		t.Fatalf("duplicate kept: %q", got.Email)
	}
}

func TestCiphertextIsBoundToItsRow(t *testing.T) {
	ctx := context.Background()
	s := openFile(t, filepath.Join(t.TempDir(), "a.db"), WithCipher(testCipher(t, 1)))
	a, _ := s.CreateUser(ctx, "a")
	b, _ := s.CreateUser(ctx, "b")
	if err := s.PutIntegration(ctx, a.ID, "withings", []byte("a-tokens")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIntegration(ctx, b.ID, "withings", []byte("b-tokens")); err != nil {
		t.Fatal(err)
	}
	stolen := rawColumn(t, s, `SELECT data FROM user_integrations WHERE user_id = 1`)
	if _, err := s.db.NewUpdate().Model((*integrationRow)(nil)).Set("data = ?", stolen).Where("user_id = ?", b.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIntegration(ctx, b.ID, "withings"); err == nil {
		t.Fatal("another user's ciphertext was accepted")
	}
}

func TestSameValueEncryptsDifferently(t *testing.T) {
	c := testCipher(t, 1)
	x := c.seal("v", "ctx")
	y := c.seal("v", "ctx")
	if x == y {
		t.Fatal("nonce reuse")
	}
	if got, _, err := c.open(x, "ctx"); err != nil || got != "v" {
		t.Fatalf("open: %q %v", got, err)
	}
}

func TestCopyKeepsEncryptedValuesReadable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := testCipher(t, 1)
	src := openFile(t, filepath.Join(dir, "src.db"), WithCipher(c))
	u := fillSensitive(t, src)
	dst := openFile(t, filepath.Join(dir, "dst.db"), WithCipher(c))
	if _, err := Copy(ctx, src, dst); err != nil {
		t.Fatal(err)
	}
	checkEncrypted(t, dst)
	checkReadable(t, dst, u.ID)
}

func TestParseKey(t *testing.T) {
	if _, err := ParseKey(strings.Repeat("ab", 32) + "\n"); err != nil {
		t.Errorf("hex: %v", err)
	}
	if _, err := ParseKey("QUJD"); err == nil {
		t.Error("short key accepted")
	}
}
