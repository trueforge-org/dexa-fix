package sqlstore

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// fillBody stores one reading and one scan, both with values and a note worth hiding.
func fillBody(t *testing.T, s *Store) models.User {
	t.Helper()
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	w, f := 81.234, 19.5
	if _, err := s.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{{MeasuredAt: day, WeightKg: &w, FatRatioPct: &f, Notes: "reading secret"}}); err != nil {
		t.Fatal(err)
	}
	sw := 79.875
	scan := models.DexaScan{UserID: u.ID, MeasuredAt: day, WeightKg: &sw, Notes: "scan secret"}
	if err := s.CreateDexaScan(ctx, &scan); err != nil {
		t.Fatal(err)
	}
	return u
}

func checkBodyReadable(t *testing.T, s *Store, uid int64) {
	t.Helper()
	ctx := context.Background()
	rs, err := s.ListScaleReadings(ctx, uid)
	if err != nil || len(rs) != 1 || rs[0].WeightKg == nil || *rs[0].WeightKg != 81.234 || *rs[0].FatRatioPct != 19.5 || rs[0].Notes != "reading secret" {
		t.Fatalf("readings: %+v, %v", rs, err)
	}
	ds, err := s.ListDexaScans(ctx, uid)
	if err != nil || len(ds) != 1 || ds[0].WeightKg == nil || *ds[0].WeightKg != 79.875 || ds[0].Notes != "scan secret" {
		t.Fatalf("scans: %+v, %v", ds, err)
	}
}

func TestBodyDataIsEncryptedAtRest(t *testing.T) {
	s := openFile(t, filepath.Join(t.TempDir(), "a.db"), WithCipher(testCipher(t, 1)))
	u := fillBody(t, s)
	checkBodyReadable(t, s, u.ID)

	for _, q := range []string{`SELECT data FROM scale_readings`, `SELECT data FROM dexa_scans`} {
		raw := rawColumn(t, s, q)
		if !strings.HasPrefix(raw, encV2) {
			t.Errorf("%s: not encrypted: %q", q, raw)
		}
		for _, leak := range []string{"81.234", "79.875", "19.5", "secret", "weight"} {
			if strings.Contains(raw, leak) {
				t.Errorf("%s: %q is visible in %q", q, leak, raw)
			}
		}
	}
}

// The body domain has a key of its own. A value sealed for it does not open with the main key, and
// the other way round, so a leak of one domain's key opens nothing of the other.
func TestBodyAndMainKeysAreSeparate(t *testing.T) {
	s := openFile(t, filepath.Join(t.TempDir(), "a.db"), WithCipher(testCipher(t, 1)))
	u := fillBody(t, s)

	raw := rawColumn(t, s, `SELECT data FROM scale_readings`)
	uid := rawColumn(t, s, `SELECT row_uid FROM scale_readings`)
	ctx := scaleContext(u.ID, uid)
	if _, _, err := s.cipher.open(raw, ctx); err == nil {
		t.Error("the main key opened a body value")
	}
	if _, _, err := s.bodyCipher.open(raw, ctx); err != nil {
		t.Errorf("the body key does not open a body value: %v", err)
	}
	mainValue, err := s.cipher.seal(`{"weight_kg":1}`, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.bodyCipher.open(mainValue, ctx); err == nil {
		t.Error("the body key opened a main value")
	}
}

// Ciphertext is tied to its row: moving it to another row, or to another user, must fail.
func TestBodyCiphertextCannotBeMoved(t *testing.T) {
	ctx := context.Background()
	s := openFile(t, filepath.Join(t.TempDir(), "a.db"), WithCipher(testCipher(t, 1)))
	u := fillBody(t, s)
	bob, _ := s.CreateUser(ctx, "bob")
	day := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	w := 60.0
	if _, err := s.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{{MeasuredAt: day, WeightKg: &w}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertScaleReadings(ctx, bob.ID, []models.ScaleReading{{MeasuredAt: day, WeightKg: &w}}); err != nil {
		t.Fatal(err)
	}

	// Another row of the same user: the data of the oldest reading goes into the newest.
	first := rawColumn(t, s, `SELECT data FROM scale_readings WHERE user_id = `+strconv.FormatInt(u.ID, 10)+` ORDER BY id LIMIT 1`)
	if _, err := s.db.ExecContext(ctx, `UPDATE scale_readings SET data = ? WHERE user_id = ? AND id = (SELECT MAX(id) FROM scale_readings WHERE user_id = ?)`, first, u.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListScaleReadings(ctx, u.ID); err == nil {
		t.Error("data moved to another row of the same user was accepted")
	}

	// Another user: bob's data into alice's newest reading.
	bobs := rawColumn(t, s, `SELECT data FROM scale_readings WHERE user_id = `+strconv.FormatInt(bob.ID, 10))
	if _, err := s.db.ExecContext(ctx, `UPDATE scale_readings SET data = ? WHERE user_id = ?`, bobs, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListScaleReadings(ctx, u.ID); err == nil {
		t.Error("data moved to another user was accepted")
	}
	if rs, err := s.ListScaleReadings(ctx, bob.ID); err != nil || len(rs) != 1 {
		t.Errorf("bob's own reading: %v, %v", rs, err)
	}
}

// Without a key the values stay readable, like the other sensitive columns; when a key is
// configured later they are encrypted on the next start.
func TestBodyDataIsEncryptedWhenAKeyArrives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	plain := openFile(t, path)
	u := fillBody(t, plain)
	if raw := rawColumn(t, plain, `SELECT data FROM scale_readings`); strings.HasPrefix(raw, encPrefix) || !strings.Contains(raw, "81.234") {
		t.Fatalf("without a key: %q", raw)
	}
	checkBodyReadable(t, plain, u.ID)
	plain.Close()

	keyed := openFile(t, path, WithCipher(testCipher(t, 1)))
	for _, q := range []string{`SELECT data FROM scale_readings`, `SELECT data FROM dexa_scans`} {
		if raw := rawColumn(t, keyed, q); !strings.HasPrefix(raw, encV2) || strings.Contains(raw, "secret") {
			t.Errorf("%s after enabling a key: %q", q, raw)
		}
	}
	checkBodyReadable(t, keyed, u.ID)
}

func TestRekeyReachesBodyData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a.db")
	old := openFile(t, path, WithCipher(cipherWith(t, 1)))
	u := fillBody(t, old)
	before := rawColumn(t, old, `SELECT data FROM scale_readings`)
	old.Close()

	s := openFile(t, path, WithCipher(cipherWith(t, 2, 1)))
	checkBodyReadable(t, s, u.ID) // still readable with the previous key
	dry, err := s.Rekey(ctx, true)
	if err != nil || dry.Body != 2 {
		t.Fatalf("dry run: %+v, %v", dry, err)
	}
	if rawColumn(t, s, `SELECT data FROM scale_readings`) != before {
		t.Fatal("a dry run changed the data")
	}
	done, err := s.Rekey(ctx, false)
	if err != nil || done.Body != 2 {
		t.Fatalf("rekey: %+v, %v", done, err)
	}
	if rawColumn(t, s, `SELECT data FROM scale_readings`) == before {
		t.Error("the reading was not re-encrypted")
	}
	again, err := s.Rekey(ctx, false)
	if err != nil || again.Body != 0 {
		t.Errorf("second rekey: %+v, %v", again, err)
	}
	s.Close()

	only := openFile(t, path, WithCipher(cipherWith(t, 2))) // the previous key is no longer needed
	checkBodyReadable(t, only, u.ID)
}

// A database whose only encrypted values are readings and scans must still tell a wrong key from the
// right one, and the serve command must still see that the database holds encrypted values.
func TestKeyCheckWithOnlyBodyData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a.db")
	s := openFile(t, path, WithCipher(cipherWith(t, 1)))
	fillBody(t, s) // the user has no e-mail address: nothing else is encrypted
	if _, err := s.db.ExecContext(ctx, `DELETE FROM app_settings WHERE name = 'crypto.check'`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if has, err := HasEncryptedValues(ctx, DriverSQLite, "file:"+path); err != nil || !has {
		t.Fatalf("HasEncryptedValues = %v, %v", has, err)
	}
	if _, err := Open(ctx, DriverSQLite, "file:"+path, WithCipher(cipherWith(t, 2))); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong key: %v", err)
	}
	right := openFile(t, path, WithCipher(cipherWith(t, 1)))
	checkBodyReadable(t, right, 1)
}

// A re-import that carries no notes keeps the user's note, with and without encryption.
func TestReimportKeepsUserNotesEncrypted(t *testing.T) {
	ctx := context.Background()
	s := openFile(t, filepath.Join(t.TempDir(), "a.db"), WithCipher(testCipher(t, 1)))
	u, _ := s.CreateUser(ctx, "alice")
	at := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	id := "g1"
	w := 80.0
	first := models.ScaleReading{MeasuredAt: at, WeightKg: &w, Source: "withings", ExternalID: &id, Notes: "after run"}
	if _, err := s.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{first}); err != nil {
		t.Fatal(err)
	}
	uid1 := rawColumn(t, s, `SELECT row_uid FROM scale_readings`)
	w2 := 81.0
	again := models.ScaleReading{MeasuredAt: at, WeightKg: &w2, Source: "withings", ExternalID: &id}
	if _, err := s.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{again}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListScaleReadings(ctx, u.ID)
	if err != nil || len(got) != 1 || *got[0].WeightKg != 81 || got[0].Notes != "after run" {
		t.Fatalf("after re-import: %+v, %v", got, err)
	}
	if uid2 := rawColumn(t, s, `SELECT row_uid FROM scale_readings`); uid2 == uid1 {
		t.Error("row_uid was not renewed by the write")
	}
	again.Notes = "edited"
	if _, err := s.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{again}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ListScaleReadings(ctx, u.ID); got[0].Notes != "edited" {
		t.Fatalf("notes not updated: %+v", got[0])
	}
}
