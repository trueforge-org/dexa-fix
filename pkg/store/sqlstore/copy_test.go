package sqlstore

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

func TestCopyPreservesData(t *testing.T) {
	ctx := context.Background()
	src, err := Open(ctx, DriverSQLite, "file:"+filepath.Join(t.TempDir(), "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := newStore(t)

	u, _ := src.CreateUser(ctx, "alice")
	if err := src.UpdateUserSettings(ctx, u.ID, true, []string{models.MethodScale}); err != nil {
		t.Fatal(err)
	}
	ext := "g1"
	at := time.Date(2024, 3, 1, 7, 30, 0, 0, time.UTC)
	if _, err := src.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{
		{MeasuredAt: at, WeightKg: f(80.5), ExternalID: &ext, Source: "withings", AthleteMode: true},
		{MeasuredAt: at.Add(time.Hour), WeightKg: f(80.1)},
	}); err != nil {
		t.Fatal(err)
	}
	d := &models.DexaScan{UserID: u.ID, MeasuredAt: at, WeightKg: f(81), BMDGCm2: f(1.2), Notes: "n"}
	if err := src.CreateDexaScan(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := src.PutIntegration(ctx, u.ID, "withings", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}

	stats, err := Copy(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (CopyStats{Users: 1, DexaScans: 1, ScaleReads: 2, Integrations: 1}) {
		t.Fatalf("stats = %+v", stats)
	}

	gu, _ := dst.GetUser(ctx, u.ID)
	wu, _ := src.GetUser(ctx, u.ID)
	if !reflect.DeepEqual(gu, wu) {
		t.Fatalf("user differs: %+v vs %+v", gu, wu)
	}
	gs, _ := dst.ListScaleReadings(ctx, u.ID)
	ws, _ := src.ListScaleReadings(ctx, u.ID)
	if !reflect.DeepEqual(gs, ws) {
		t.Fatalf("readings differ:\n%+v\n%+v", gs, ws)
	}
	gd, _ := dst.ListDexaScans(ctx, u.ID)
	wd, _ := src.ListDexaScans(ctx, u.ID)
	if !reflect.DeepEqual(gd, wd) {
		t.Fatalf("scans differ:\n%+v\n%+v", gd, wd)
	}
	if b, _ := dst.GetIntegration(ctx, u.ID, "withings"); string(b) != `{"a":1}` {
		t.Fatalf("integration = %s", b)
	}

	// New rows after the copy must get fresh ids.
	nu, err := dst.CreateUser(ctx, "bob")
	if err != nil || nu.ID <= u.ID {
		t.Fatalf("new user after copy: %+v %v", nu, err)
	}
}

func TestCopyRefusesNonEmptyTarget(t *testing.T) {
	ctx := context.Background()
	src, dst := newStore(t), func() *Store {
		s, err := Open(ctx, DriverSQLite, "file:"+filepath.Join(t.TempDir(), "d.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}()
	if _, err := dst.CreateUser(ctx, "existing"); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, src, dst); err == nil {
		t.Fatal("expected error for non-empty target")
	}
}
