package sqlstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

func f(v float64) *float64 { return &v }

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
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
	if _, err := s.CreateUser(ctx, "alice"); !errors.Is(err, store.ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	if _, err := s.CreateUser(ctx, " "); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if _, err := s.GetUser(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	us, _ := s.ListUsers(ctx)
	if len(us) != 1 {
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

func TestRebind(t *testing.T) {
	s := &Store{driver: DriverPostgres}
	if got := s.q("a=? AND b=?"); got != "a=$1 AND b=$2" {
		t.Fatal(got)
	}
}
