package sqlstore

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
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
	// The body counter goes along, or the estimates of the copy would start again from version 0.
	gv, _ := dst.BodyVersion(ctx, u.ID)
	wv, _ := src.BodyVersion(ctx, u.ID)
	if wv == 0 || gv != wv {
		t.Fatalf("body version: copy %d, source %d", gv, wv)
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

func TestCopyIncludesAccounts(t *testing.T) {
	ctx := context.Background()
	src, err := Open(ctx, DriverSQLite, "file:"+filepath.Join(t.TempDir(), "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := newStore(t)

	height := 180.5
	u, err := src.CreateUserWithIdentity(ctx, "alice", "a@example.com", models.Identity{Provider: models.ProviderLocal, Secret: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.UpdateUserProfile(ctx, u.ID, "alice", "a@example.com", &height); err != nil {
		t.Fatal(err)
	}
	if err := src.CreateAccessToken(ctx, &models.AccessToken{UserID: u.ID, Name: "t", Prefix: "p", Hash: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := src.PutSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := src.PutAuthProvider(ctx, models.AuthProvider{Slug: "g", Label: "G", Kind: models.ProviderKindOIDC, ClientID: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, src, dst); err != nil {
		t.Fatal(err)
	}

	got, err := dst.GetUser(ctx, u.ID)
	if err != nil || !got.IsAdmin() || got.Email != "a@example.com" || got.HeightCm == nil || *got.HeightCm != height {
		t.Fatalf("user: %+v %v", got, err)
	}
	if id, err := dst.GetIdentity(ctx, models.ProviderLocal, "1"); err != nil || id.Secret != "hash" {
		t.Fatalf("identity: %+v %v", id, err)
	}
	if _, err := dst.UserByTokenHash(ctx, "h"); err != nil {
		t.Fatalf("token: %v", err)
	}
	if v, err := dst.GetSetting(ctx, "k"); err != nil || v != "v" {
		t.Fatalf("setting: %q %v", v, err)
	}
	if p, err := dst.GetAuthProvider(ctx, "g"); err != nil || p.Label != "G" {
		t.Fatalf("provider: %+v %v", p, err)
	}
}

func TestLastAdminGuards(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a, _ := s.CreateUser(ctx, "a")
	b, _ := s.CreateUser(ctx, "b")
	if !a.IsAdmin() || b.IsAdmin() {
		t.Fatalf("roles: %s %s", a.Role, b.Role)
	}
	if err := s.SetUserRole(ctx, a.ID, models.RoleUser); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("demote last: %v", err)
	}
	if err := s.DeleteUser(ctx, a.ID); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("delete last: %v", err)
	}
	if err := s.SetUserRole(ctx, 99, models.RoleUser); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if err := s.SetUserRole(ctx, b.ID, models.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, a.ID); err != nil {
		t.Fatalf("delete with another admin: %v", err)
	}
}
