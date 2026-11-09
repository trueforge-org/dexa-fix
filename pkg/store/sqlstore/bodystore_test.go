package sqlstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

func openBodyFile(t *testing.T, path string, opts ...Option) *BodyStore {
	t.Helper()
	b, err := OpenBody(context.Background(), DriverSQLite, "file:"+path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// A body database holds the body domain and nothing else: no users, sessions or settings.
func TestBodyStoreHasOnlyItsOwnTables(t *testing.T) {
	ctx := context.Background()
	b := openBodyFile(t, filepath.Join(t.TempDir(), "body.db"))
	var tables []string
	if err := b.db.NewRaw(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`).Scan(ctx, &tables); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(tables, ",")
	if want := "body_meta,body_user_state,dexa_scans,goose_body_version,scale_readings"; got != want {
		t.Fatalf("tables = %s, want %s", got, want)
	}
	if v, err := b.SchemaVersion(ctx); err != nil || v != 1 {
		t.Fatalf("schema version %d, %v", v, err)
	}
	// It works as a BodyStore: no user needs to exist.
	w := 80.0
	if _, err := b.UpsertScaleReadings(ctx, 42, []models.ScaleReading{{MeasuredAt: time.Now(), WeightKg: &w}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := b.BodyVersion(ctx, 42); v != 1 {
		t.Errorf("body version %d", v)
	}
}

// The main database opened WithoutBody has no body tables, and a combined one has both sets, each
// counted by its own version table.
func TestMainStoreWithoutBodyAndCombined(t *testing.T) {
	ctx := context.Background()
	only := openFile(t, filepath.Join(t.TempDir(), "main.db"), WithoutBody())
	if only.BodyStore != nil {
		t.Fatal("a store opened WithoutBody has a body store")
	}
	for _, name := range []string{"scale_readings", "dexa_scans", "body_user_state", "body_meta", "goose_body_version"} {
		if n := rawColumn(t, only, `SELECT COUNT(*) FROM sqlite_master WHERE name = '`+name+`'`); n != "0" {
			t.Errorf("a main database without the body domain has %s", name)
		}
	}
	if _, err := only.CreateUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}

	both := openFile(t, filepath.Join(t.TempDir(), "both.db"))
	for _, name := range []string{"scale_readings", "dexa_scans", "body_user_state", "body_meta", "goose_body_version", "goose_db_version", "users"} {
		if n := rawColumn(t, both, `SELECT COUNT(*) FROM sqlite_master WHERE name = '`+name+`'`); n != "1" {
			t.Errorf("a combined database lacks %s", name)
		}
	}
	if v, _ := both.SchemaVersion(ctx); v != 1 {
		t.Errorf("main schema version %d", v)
	}
	if v, _ := both.BodyStore.SchemaVersion(ctx); v != 1 {
		t.Errorf("body schema version %d", v)
	}
	if pending, err := both.HasPending(ctx); err != nil || pending {
		t.Errorf("pending = %v, %v", pending, err)
	}
}

func fillBodyOnly(t *testing.T, b *BodyStore) {
	t.Helper()
	ctx := context.Background()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	w, f := 81.234, 19.5
	if _, err := b.UpsertScaleReadings(ctx, 1, []models.ScaleReading{{MeasuredAt: day, WeightKg: &w, FatRatioPct: &f, Notes: "reading secret"}}); err != nil {
		t.Fatal(err)
	}
	sw := 79.875
	scan := models.DexaScan{UserID: 1, MeasuredAt: day, WeightKg: &sw, Notes: "scan secret"}
	if err := b.CreateDexaScan(ctx, &scan); err != nil {
		t.Fatal(err)
	}
}

// A body process has a key of its own: it checks that key against its data, refuses a wrong one,
// and the main key does not open the data unless it is the same key.
func TestBodyStoreKeyCheckAndRotation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "body.db")
	b := openBodyFile(t, path, WithCipher(cipherWith(t, 7)))
	fillBodyOnly(t, b)
	if raw := rawBody(t, b, `SELECT data FROM scale_readings`); !strings.HasPrefix(raw, encV2) || strings.Contains(raw, "81.234") {
		t.Fatalf("not encrypted: %q", raw)
	}
	if v := rawBody(t, b, `SELECT value FROM body_meta WHERE name = 'crypto.check'`); len(v) != 64 {
		t.Fatalf("check value: %q", v)
	}
	b.Close()

	if _, err := OpenBody(ctx, DriverSQLite, "file:"+path, WithCipher(cipherWith(t, 8))); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong key: %v", err)
	}
	if has, err := HasEncryptedBodyValues(ctx, DriverSQLite, "file:"+path); err != nil || !has {
		t.Fatalf("HasEncryptedBodyValues = %v, %v", has, err)
	}

	// Rotation: the new key reads the data through the previous one, then re-encrypts it.
	rot := openBodyFile(t, path, WithCipher(cipherWith(t, 8, 7)))
	dry, err := rot.Rekey(ctx, true)
	if err != nil || dry.Changed != 2 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	done, err := rot.Rekey(ctx, false)
	if err != nil || done.Changed != 2 {
		t.Fatalf("rekey: %+v %v", done, err)
	}
	if again, _ := rot.Rekey(ctx, false); again.Changed != 0 {
		t.Errorf("second rekey changed %d", again.Changed)
	}
	rot.Close()
	only := openBodyFile(t, path, WithCipher(cipherWith(t, 8)))
	rs, err := only.ListScaleReadings(ctx, 1)
	if err != nil || len(rs) != 1 || *rs[0].WeightKg != 81.234 || rs[0].Notes != "reading secret" {
		t.Fatalf("after rotation: %+v, %v", rs, err)
	}
}

func rawBody(t *testing.T, b *BodyStore, query string) string {
	t.Helper()
	var v string
	if err := b.db.NewRaw(query).Scan(context.Background(), &v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

// Sharing the key file of the main backend gives the body process the same key: data written in a
// combined database can be moved to a body database of its own and read there.
func TestBodyKeyDerivedFromTheMainKeyIsTheSameInASeparateProcess(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	combined := openFile(t, filepath.Join(dir, "both.db"), WithCipher(testCipher(t, 3)))
	u := fillBody(t, combined)
	raw := rawColumn(t, combined, `SELECT data FROM scale_readings`)
	uid := rawColumn(t, combined, `SELECT row_uid FROM scale_readings`)
	combined.Close()

	standalone := openBodyFile(t, filepath.Join(dir, "body.db"), WithCipher(testCipher(t, 3)))
	if _, _, err := standalone.cipher.open(raw, scaleContext(u.ID, uid)); err != nil {
		t.Fatalf("a body process with the same key file cannot open a value of the combined one: %v", err)
	}
	if _, err := standalone.ListScaleReadings(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
}
