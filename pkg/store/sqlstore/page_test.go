package sqlstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

func TestPagingWalksEveryReadingOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, _ := s.CreateUser(ctx, "alice")
	other, _ := s.CreateUser(ctx, "bob")

	base := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	var rs []models.ScaleReading
	for i := 0; i < 25; i++ {
		// pairs of readings share a timestamp, to exercise the id tie-break
		w := float64(70 + i)
		rs = append(rs, models.ScaleReading{MeasuredAt: base.Add(time.Duration(i/2) * time.Hour), WeightKg: &w, Source: "manual"})
	}
	if _, err := s.UpsertScaleReadings(ctx, u.ID, rs); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertScaleReadings(ctx, other.ID, rs[:3]); err != nil {
		t.Fatal(err)
	}

	seen := map[int64]bool{}
	var after *store.Cursor
	pages := 0
	for {
		page, next, err := s.ListScaleReadingsPage(ctx, u.ID, store.ListFilter{After: after, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, r := range page {
			if seen[r.ID] {
				t.Fatalf("reading %d on two pages", r.ID)
			}
			if r.UserID != u.ID {
				t.Fatalf("someone else's reading: %+v", r)
			}
			seen[r.ID] = true
		}
		if next == nil {
			break
		}
		if len(page) != 10 {
			t.Fatalf("short page %d before the end", len(page))
		}
		after = next
	}
	if len(seen) != 25 || pages != 3 {
		t.Errorf("saw %d readings over %d pages", len(seen), pages)
	}

	// Exactly one full page has no next cursor.
	if page, next, _ := s.ListScaleReadingsPage(ctx, u.ID, store.ListFilter{Limit: 25}); len(page) != 25 || next != nil {
		t.Errorf("exact page: %d readings, next %v", len(page), next)
	}
	// Newest first, and the window bounds are inclusive.
	win, _, _ := s.ListScaleReadingsPage(ctx, u.ID, store.ListFilter{Since: base.Add(2 * time.Hour), Until: base.Add(3 * time.Hour)})
	if len(win) != 4 || !win[0].MeasuredAt.Equal(base.Add(3*time.Hour)) {
		t.Errorf("window: %d readings, first %v", len(win), win[0].MeasuredAt)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := store.Cursor{MeasuredAt: time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC), ID: 42}
	got, err := store.ParseCursor(c.String())
	if err != nil || !got.MeasuredAt.Equal(c.MeasuredAt) || got.ID != 42 {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "x", "1.x", "x.1", "1.-2", "12"} {
		if _, err := store.ParseCursor(bad); err == nil {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}

func TestDexaPaging(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, _ := s.CreateUser(ctx, "alice")
	for i := 0; i < 5; i++ {
		d := models.DexaScan{UserID: u.ID, MeasuredAt: time.Date(2025, time.Month(i+1), 1, 0, 0, 0, 0, time.UTC)}
		if err := s.CreateDexaScan(ctx, &d); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := s.ListDexaScansPage(ctx, u.ID, store.ListFilter{Limit: 2})
	if err != nil || len(page) != 2 || next == nil || page[0].MeasuredAt.Month() != 5 {
		t.Fatalf("first page: %v %v %v", page, next, err)
	}
	rest, next, _ := s.ListDexaScansPage(ctx, u.ID, store.ListFilter{After: next, Limit: 10})
	if len(rest) != 3 || next != nil {
		t.Errorf("rest: %d, next %v", len(rest), next)
	}
}

func TestBulkUpsert(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, _ := s.CreateUser(ctx, "alice")
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// More than one chunk, with repeated external ids inside the batch: the last one wins.
	var rs []models.ScaleReading
	for i := 0; i < 1203; i++ {
		id := fmt.Sprintf("g%d", i%1000)
		w := float64(i)
		rs = append(rs, models.ScaleReading{MeasuredAt: day.Add(time.Duration(i) * time.Minute), WeightKg: &w, Source: "withings", ExternalID: &id})
	}
	n, err := s.UpsertScaleReadings(ctx, u.ID, rs)
	if err != nil || n != 1203 {
		t.Fatalf("upsert: %d %v", n, err)
	}
	got, _ := s.ListScaleReadings(ctx, u.ID)
	if len(got) != 1000 {
		t.Fatalf("%d rows, want 1000 distinct external ids", len(got))
	}
	for _, r := range got {
		if *r.ExternalID == "g0" && *r.WeightKg != 1000 {
			t.Errorf("g0 kept weight %v, want the last delivered (1000)", *r.WeightKg)
		}
	}
	// A second delivery updates in place.
	if _, err := s.UpsertScaleReadings(ctx, u.ID, rs[:10]); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListScaleReadings(ctx, u.ID); len(got) != 1000 {
		t.Errorf("re-import changed the row count: %d", len(got))
	}
	// A bad reading anywhere in the batch writes nothing.
	bad := append([]models.ScaleReading{{MeasuredAt: day, Source: "manual"}}, models.ScaleReading{})
	if _, err := s.UpsertScaleReadings(ctx, u.ID, bad); err == nil {
		t.Error("invalid batch accepted")
	}
	if got, _ := s.ListScaleReadings(ctx, u.ID); len(got) != 1000 {
		t.Errorf("invalid batch left %d rows", len(got))
	}
}

func TestDataVersionChangesWithEverythingEstimatesUse(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, _ := s.CreateUser(ctx, "alice")
	version := func() int64 {
		t.Helper()
		got, err := s.GetUser(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.DataVersion
	}
	last := version()
	step := func(what string, do func() error) {
		t.Helper()
		if err := do(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if v := version(); v <= last {
			t.Errorf("%s did not change the data version (%d -> %d)", what, last, v)
		} else {
			last = v
		}
	}
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var scan models.DexaScan
	step("adding a scan", func() error {
		scan = models.DexaScan{UserID: u.ID, MeasuredAt: day}
		return s.CreateDexaScan(ctx, &scan)
	})
	step("adding readings", func() error {
		_, err := s.UpsertScaleReadings(ctx, u.ID, []models.ScaleReading{{MeasuredAt: day}})
		return err
	})
	step("changing settings", func() error { return s.UpdateUserSettings(ctx, u.ID, true, nil) })
	step("changing the height", func() error { h := 180.0; return s.UpdateUserProfile(ctx, u.ID, "alice", "a@example.com", &h) })
	step("deleting a scan", func() error { return s.DeleteDexaScan(ctx, u.ID, scan.ID) })
	rs, _ := s.ListScaleReadings(ctx, u.ID)
	step("deleting a reading", func() error { return s.DeleteScaleReading(ctx, u.ID, rs[0].ID) })

	// A failed or empty write leaves the version alone.
	before := version()
	s.DeleteDexaScan(ctx, u.ID, 9999)
	s.UpsertScaleReadings(ctx, u.ID, nil)
	if version() != before {
		t.Error("a write that changed nothing bumped the version")
	}
}
