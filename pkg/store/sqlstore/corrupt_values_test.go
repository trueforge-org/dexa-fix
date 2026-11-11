package sqlstore

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

const unknownKeyValue = "enc:v2:deadbeef:AAAA" // sealed with a key nobody configured

// A value that no configured key opens is an error everywhere it is read, and `db rekey` names it
// instead of skipping it: silently dropping a secret would be worse than stopping.
func TestAValueNoKeyOpensIsReportedByTheReaderAndByRekey(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		corrupt string
		read    func(*Store, faultData) error
		label   string // what `db rekey` calls the value
	}{
		{"an integration", `UPDATE user_integrations SET data = '` + unknownKeyValue + `'`,
			func(s *Store, f faultData) error { return e1(s.GetIntegration(ctx, f.user, "withings")) }, "integration of user"},
		{"a setting", `UPDATE app_settings SET value = '` + unknownKeyValue + `' WHERE name = 'k'`,
			func(s *Store, f faultData) error { return e1(s.GetSetting(ctx, "k")) }, "setting k"},
		{"a sign-in provider secret", `UPDATE auth_providers SET client_secret = '` + unknownKeyValue + `'`,
			func(s *Store, f faultData) error { return e1(s.ListAuthProviders(ctx)) }, "sign-in provider github"},
		{"a two-step secret", `UPDATE user_totp SET secret = '` + unknownKeyValue + `'`,
			func(s *Store, f faultData) error { return e1(s.GetTOTP(ctx, f.user)) }, "two-step verification secret"},
		{"an e-mail address", `UPDATE users SET email = '` + unknownKeyValue + `' WHERE role = 'user'`,
			func(s *Store, f faultData) error { return e1(s.ListUsers(ctx)) }, "e-mail of user"},
		{"the payload of a job", `UPDATE worker_jobs SET payload = '` + unknownKeyValue + `' WHERE state = 'queued'`,
			func(s *Store, f faultData) error { return e1(s.GetJob(ctx, f.queuedJob)) }, "payload of job"},
		{"the result of a job", `UPDATE worker_jobs SET result = '` + unknownKeyValue + `' WHERE state = 'succeeded'`,
			func(s *Store, f faultData) error {
				return e1(s.GetJob(ctx, rawColumn(t, s, `SELECT public_id FROM worker_jobs WHERE state = 'succeeded'`)))
			}, "result of job"},
		{"a scale reading", `UPDATE scale_readings SET data = '` + unknownKeyValue + `'`,
			func(s *Store, f faultData) error { return e1(s.ListScaleReadings(ctx, f.user)) }, "scale reading"},
		{"a dexa scan", `UPDATE dexa_scans SET data = '` + unknownKeyValue + `'`,
			func(s *Store, f faultData) error { return e1(s.ListDexaScans(ctx, f.user)) }, "dexa scan"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newFaultyStore(t, testCipher(t, 1))
			fx := keyedSeed(t, s)
			if err := s.ExecForTests(ctx, c.corrupt); err != nil {
				t.Fatal(err)
			}
			if err := c.read(s, fx); err == nil {
				t.Error("reading the value did not fail")
			}
			for _, dry := range []bool{false, true} {
				if _, err := s.Rekey(ctx, dry); err == nil || !strings.Contains(err.Error(), c.label) {
					t.Errorf("rekey (dry run %v) answered %v, want an error naming %q", dry, err, c.label)
				}
			}
		})
	}
}

func TestRekeyNeedsAKey(t *testing.T) {
	s := newStore(t)
	if _, err := s.Rekey(context.Background(), false); err == nil || !strings.Contains(err.Error(), "needs an encryption key") {
		t.Fatalf("store: %v", err)
	}
	if _, err := s.BodyStore.Rekey(context.Background(), false); err == nil || !strings.Contains(err.Error(), "needs an encryption key") {
		t.Fatalf("body store: %v", err)
	}
}

func TestRekeyLeavesValuesWrittenBeforeTheKeyForTheNextStart(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	seedFaultData(t, s)
	withKeys(t, s, 1)
	stats, err := s.Rekey(ctx, false)
	if err != nil || stats.Emails+stats.Settings+stats.Integrations+stats.Providers+stats.TOTP+stats.Jobs+stats.Body != 0 {
		t.Fatalf("rekey of plain values: %+v %v", stats, err)
	}
	if v := rawColumn(t, s, rawSetting); v != "v" {
		t.Fatalf("a plain setting was changed to %q", v)
	}
}

func TestRekeyRefusesABodyValueNoKeyOpensEvenWithoutTheMainData(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, testCipher(t, 1))
	fx := keyedSeed(t, s)
	if err := s.ExecForTests(ctx, `UPDATE scale_readings SET data = '`+unknownKeyValue+`'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BodyStore.Rekey(ctx, false); err == nil {
		t.Fatal("body rekey accepted an unreadable reading")
	}
	_ = fx
}

// The body documents of a store without a key are plain JSON; one that is not JSON is reported with
// the row it belongs to.
func TestAReadingOrScanThatIsNotValidJSONIsReported(t *testing.T) {
	ctx := context.Background()
	for _, table := range []struct {
		name  string
		table string
		read  func(*Store, int64) error
	}{
		{"a scale reading", "scale_readings", func(s *Store, u int64) error { return e1(s.ListScaleReadings(ctx, u)) }},
		{"a dexa scan", "dexa_scans", func(s *Store, u int64) error { return e1(s.ListDexaScans(ctx, u)) }},
		{"a dexa scan on a page", "dexa_scans", func(s *Store, u int64) error {
			return e1(s.ListDexaScansPage(ctx, u, storeFilter()))
		}},
		{"a scale reading on a page", "scale_readings", func(s *Store, u int64) error {
			return e1(s.ListScaleReadingsPage(ctx, u, storeFilter()))
		}},
	} {
		t.Run(table.name, func(t *testing.T) {
			s, _ := newFaultyStore(t, nil)
			fx := seedFaultData(t, s)
			if err := s.ExecForTests(ctx, `UPDATE `+table.table+` SET data = '{not json'`); err != nil {
				t.Fatal(err)
			}
			if err := table.read(s, fx.user); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

// Rows sealed with a key cannot be read by a store that has none.
func TestSealedReadingsNeedTheKeyToBeRead(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	for _, table := range []string{"scale_readings", "dexa_scans"} {
		if err := s.ExecForTests(ctx, `UPDATE `+table+` SET data = '`+unknownKeyValue+`'`); err != nil {
			t.Fatal(err)
		}
	}
	for name, read := range map[string]func() error{
		"scale readings": func() error { return e1(s.ListScaleReadings(ctx, fx.user)) },
		"dexa scans":     func() error { return e1(s.ListDexaScans(ctx, fx.user)) },
	} {
		if err := read(); err == nil || !strings.Contains(err.Error(), "no encryption key is configured") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A new reading that overlaps an unreadable one cannot keep its notes, so the import stops.
func TestAnImportStopsAtAnExistingReadingItCannotRead(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	if err := s.ExecForTests(ctx, `UPDATE scale_readings SET data = '{not json'`); err != nil {
		t.Fatal(err)
	}
	_, err := s.UpsertScaleReadings(ctx, fx.user, []models.ScaleReading{{UserID: fx.user, MeasuredAt: fx.at, ExternalID: sp("e1"), Source: "csv"}})
	if err == nil {
		t.Fatal("the import went on")
	}
}

// A value that cannot be written as JSON is refused instead of being stored as something else.
func TestAReadingOrScanThatCannotBeEncodedIsRefused(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, err := s.CreateUser(ctx, "uu")
	if err != nil {
		t.Fatal(err)
	}
	nan := math.NaN()
	if err := s.CreateDexaScan(ctx, &models.DexaScan{UserID: u.ID, MeasuredAt: time.Now(), WeightKg: &nan}); err == nil {
		t.Error("a scan with a weight of NaN was stored")
	}
	_, err = s.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{{UserID: u.ID, MeasuredAt: time.Now(), WeightKg: &nan, Source: "csv"}})
	if err == nil {
		t.Error("a reading with a weight of NaN was stored")
	}
	if n, _ := s.BodyStats(ctx); n.DexaScans != 0 || n.ScaleReadings != 0 {
		t.Errorf("something was stored: %+v", n)
	}
	// The seal functions answer the same way when they are called on their own.
	if err := s.BodyStore.sealScale(&scaleRow{Vals: scaleValues{WeightKg: &nan}}); err == nil {
		t.Error("sealScale accepted NaN")
	}
	if err := s.BodyStore.sealDexa(&dexaRow{Vals: dexaValues{WeightKg: &nan}}); err == nil {
		t.Error("sealDexa accepted NaN")
	}
}

func TestEncryptedValuesWithAnUnknownShapeAreRefused(t *testing.T) {
	c := testCipher(t, 1)
	for name, tc := range map[string]struct{ value, want string }{
		"a version 2 value without a key id": {"enc:v2:nocolon", "malformed"},
		"a version that does not exist":      {"enc:v9:abc", "unknown encrypted value format"},
		"a payload that is not base64":       {"enc:v1:!!!", "illegal base64"},
		"a payload shorter than a nonce":     {"enc:v1:AAAA", "too short"},
		"a payload that is not ours":         {"enc:v1:" + strings.Repeat("A", 40), "decrypt stored value"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := c.open(tc.value, "ctx"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("open(%q): %v, want %q", tc.value, err, tc.want)
			}
		})
	}
}

func TestKeysAreChecked(t *testing.T) {
	if _, err := NewCipher([]byte("short")); err == nil || !strings.Contains(err.Error(), "must be 32 bytes") {
		t.Errorf("a short key: %v", err)
	}
	if _, err := NewCipher(make([]byte, KeySize), []byte("short")); err == nil || !strings.Contains(err.Error(), "previous key") {
		t.Errorf("a short previous key: %v", err)
	}
	key, err := ParseKey("  " + strings.Repeat("ab", KeySize/2*2) + "\n")
	if err != nil || len(key) != KeySize {
		t.Errorf("hexadecimal key: %v %v", len(key), err)
	}
	b64 := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	if key, err := ParseKey(b64); err != nil || len(key) != KeySize || key[0] != 1 {
		t.Errorf("base64 key: %v %v", key, err)
	}
	if _, err := ParseKey("not a key"); err == nil {
		t.Error("a text that is no key was accepted")
	}
}

func TestTheDatabaseErrorHelpersKnowPostgreSQLErrorsToo(t *testing.T) {
	unique := &pgconn.PgError{Code: pgUniqueViolation}
	fk := &pgconn.PgError{Code: pgForeignKeyViolation}
	if !isUnique(unique) || isUnique(fk) {
		t.Error("isUnique on PostgreSQL errors")
	}
	if !isFK(fk) || isFK(unique) {
		t.Error("isFK on PostgreSQL errors")
	}
	if isUnique(errors.New("other")) || isFK(errors.New("other")) {
		t.Error("an error that is not from the database was taken for a constraint")
	}
}

func TestAnEmptyTimeZoneColumnReadsAsUTC(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, _ := s.CreateUser(ctx, "uu")
	if err := s.ExecForTests(ctx, `UPDATE users SET timezone = ''`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetUser(ctx, u.ID); err != nil || got.Timezone != "UTC" {
		t.Fatalf("timezone %q %v", got.Timezone, err)
	}
}

func storeFilter() store.ListFilter { return store.ListFilter{Limit: 10} }
