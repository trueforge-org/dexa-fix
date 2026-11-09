package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

func f(v float64) *float64 { return &v }

// newStore returns a fresh, migrated store. It uses in-memory SQLite unless DEXAFIX_TEST_POSTGRES_DSN is
// set, in which case every test gets its own schema in that Postgres database. Run the suite both ways.
func newStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	if pg := os.Getenv("DEXAFIX_TEST_POSTGRES_DSN"); pg != "" {
		return newPostgresStore(t, pg)
	}
	s, err := Open(ctx, DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newPostgresStore(t *testing.T, dsn string) *Store {
	t.Helper()
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("t_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("pgx", dsn)
		if err == nil {
			a.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
			a.Close()
		}
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := Open(ctx, DriverPostgres, dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUsers(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, err := s.CreateUser(ctx, "  alice ")
	if err != nil || u.Name != "alice" || u.ID == 0 {
		t.Fatalf("create: %+v %v", u, err)
	}
	if _, err := s.CreateUser(ctx, "alice"); err != nil { // names need not be unique
		t.Fatalf("same name: %v", err)
	}
	if _, err := s.CreateUser(ctx, " "); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if _, err := s.GetUser(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	us, _ := s.ListUsers(ctx)
	if len(us) != 2 {
		t.Fatalf("list: %v", us)
	}
}

func TestDexaRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, _ := s.CreateUser(ctx, "a")
	other, _ := s.CreateUser(ctx, "b")
	d := &models.DexaScan{UserID: u.ID, MeasuredAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), WeightKg: f(80.5), BMDGCm2: f(1.25), Notes: "x"}
	if err := s.CreateDexaScan(ctx, d); err != nil || d.ID == 0 {
		t.Fatal(err)
	}
	got, err := s.ListDexaScans(ctx, u.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	g := got[0]
	if *g.WeightKg != 80.5 || *g.BMDGCm2 != 1.25 || g.FatMassKg != nil || g.Notes != "x" || !g.MeasuredAt.Equal(d.MeasuredAt) {
		t.Fatalf("mismatch: %+v", g)
	}
	if err := s.DeleteDexaScan(ctx, other.ID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete other user's scan: %v", err)
	}
	if err := s.DeleteDexaScan(ctx, u.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDexaScan(ctx, &models.DexaScan{UserID: 999, MeasuredAt: d.MeasuredAt}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestScaleUpsertIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, _ := s.CreateUser(ctx, "a")
	ext := "grp-1"
	at := time.Date(2026, 3, 2, 7, 30, 0, 0, time.UTC)
	batch := []models.ScaleReading{
		{MeasuredAt: at, WeightKg: f(81), Source: "withings", ExternalID: &ext},
		{MeasuredAt: at.Add(time.Hour), WeightKg: f(82)}, // manual, no external id
	}
	if n, err := s.UpsertScaleReadings(ctx, u.ID, batch); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	batch[0].WeightKg = f(81.4)
	if _, err := s.UpsertScaleReadings(ctx, u.ID, batch[:1]); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListScaleReadings(ctx, u.ID)
	if len(got) != 2 {
		t.Fatalf("want 2 rows after re-import, got %d", len(got))
	}
	// newest first: the manual reading is 1h later
	if got[0].Source != "manual" || *got[1].WeightKg != 81.4 || *got[1].ExternalID != "grp-1" {
		t.Fatalf("unexpected: %+v", got)
	}
	if _, err := s.UpsertScaleReadings(ctx, 999, batch[:1]); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	if err := s.DeleteScaleReading(ctx, u.ID, got[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedDriver(t *testing.T) {
	if _, err := Open(context.Background(), "mysql", ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestIntegrations(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, err := s.CreateUser(ctx, "ann")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIntegration(ctx, u.ID, "withings"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	for _, data := range []string{`{"a":1}`, `{"a":2}`} { // second put overwrites
		if err := s.PutIntegration(ctx, u.ID, "withings", []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetIntegration(ctx, u.ID, "withings")
	if err != nil || string(got) != `{"a":2}` {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := s.PutIntegration(ctx, 999, "withings", []byte("{}")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	for i := 0; i < 2; i++ { // deleting twice is fine
		if err := s.DeleteIntegration(ctx, u.ID, "withings"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetIntegration(ctx, u.ID, "withings"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestUserSettings(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, _ := s.CreateUser(ctx, "a")
	got, err := s.GetUser(ctx, u.ID)
	if err != nil || got.AthleteDefault || !reflect.DeepEqual(got.MethodRanking, models.DefaultRanking()) {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	if err := s.UpdateUserSettings(ctx, u.ID, true, []string{"scale", "bogus", "caliper", "scale"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetUser(ctx, u.ID)
	want := []string{"scale", "caliper", "hydrostatic", "bodpod", "tape", "visual-ai", "manual-entry"}
	if !got.AthleteDefault || !reflect.DeepEqual(got.MethodRanking, want) {
		t.Fatalf("saved: %+v", got)
	}
	if us, _ := s.ListUsers(ctx); len(us) != 1 || !us[0].AthleteDefault {
		t.Fatalf("list: %+v", us)
	}
	if err := s.UpdateUserSettings(ctx, 999, true, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestScaleMethodAndAthleteModeAreKeptOnUpsert(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, _ := s.CreateUser(ctx, "a")
	ext := "grp-1"
	at := time.Date(2026, 3, 2, 7, 30, 0, 0, time.UTC)
	r := models.ScaleReading{MeasuredAt: at, WeightKg: f(81), Source: "withings", ExternalID: &ext, Method: models.MethodTape, AthleteMode: true}
	if _, err := s.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{r}); err != nil {
		t.Fatal(err)
	}
	// a re-import carries the importer's defaults; the stored flags must survive
	r.WeightKg, r.Method, r.AthleteMode = f(82), models.MethodScale, false
	if _, err := s.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{r}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListScaleReadings(ctx, u.ID)
	if len(got) != 1 || *got[0].WeightKg != 82 || got[0].Method != models.MethodTape || !got[0].AthleteMode {
		t.Fatalf("unexpected: %+v", got)
	}
	// an empty method means a smart scale; an unknown one is rejected
	if _, err := s.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{{MeasuredAt: at, WeightKg: f(80)}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListScaleReadings(ctx, u.ID); got[0].Method != models.MethodScale || got[0].AthleteMode {
		t.Fatalf("defaults: %+v", got[0])
	}
	if _, err := s.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{{MeasuredAt: at, Method: "nope"}}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("unknown method: %v", err)
	}
}
