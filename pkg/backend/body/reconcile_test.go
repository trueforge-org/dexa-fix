package bodybackend

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

// The rules are the same for every source of readings: the tests use a source that is not Withings.
const rcSource = "demo-scale"

func rcFl(v float64) *float64 { return &v }

func rcTs(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

// rcStored builds a stored reading; ext == "" means a manual reading.
func rcStored(id int64, ext, when string, w float64) models.ScaleReading {
	r := models.ScaleReading{ID: id, MeasuredAt: rcTs(when), WeightKg: rcFl(w), Source: models.SourceManual}
	if ext != "" {
		r.Source, r.ExternalID = rcSource, &ext
	}
	return r
}

func rcFetch(ext, when string, w float64) models.ScaleReading {
	return models.ScaleReading{MeasuredAt: rcTs(when), WeightKg: rcFl(w), Source: rcSource, ExternalID: &ext}
}

func rcSeen(ids ...string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func TestPlan(t *testing.T) {
	day := rcTs("2026-03-01T00:00")
	tests := []struct {
		name     string
		existing []models.ScaleReading
		fetched  []models.ScaleReading
		seen     map[string]bool
		dayMode  bool

		gap, other, del                                int
		imported, updated, unchanged, removed, skipped int
		skippedDays                                    []string
	}{
		{name: "day without readings is filled", fetched: []models.ScaleReading{rcFetch("g1", "2026-03-01T07:00", 80)}, seen: rcSeen("g1"),
			gap: 1, imported: 1},
		{name: "identical stored reading is left alone",
			existing: []models.ScaleReading{rcStored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  []models.ScaleReading{rcFetch("g1", "2026-03-01T07:00", 80)}, seen: rcSeen("g1"),
			unchanged: 1},
		{name: "changed values are updated",
			existing: []models.ScaleReading{rcStored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  []models.ScaleReading{rcFetch("g1", "2026-03-01T07:00", 79.5)}, seen: rcSeen("g1"),
			other: 1, updated: 1},
		{name: "group moved to another day is updated in place",
			existing: []models.ScaleReading{rcStored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  []models.ScaleReading{rcFetch("g1", "2026-03-02T07:00", 80)}, seen: rcSeen("g1"),
			other: 1, updated: 1},
		{name: "new group next to an imported one is added",
			existing: []models.ScaleReading{rcStored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  []models.ScaleReading{rcFetch("g2", "2026-03-01T19:00", 80.4)}, seen: rcSeen("g2"),
			other: 1, imported: 1},
		{name: "bulk import skips days with manual readings",
			existing: []models.ScaleReading{rcStored(1, "", "2026-03-01T08:00", 81)},
			fetched:  []models.ScaleReading{rcFetch("g1", "2026-03-01T07:00", 80), rcFetch("g2", "2026-03-01T19:00", 80.4), rcFetch("g3", "2026-03-02T07:00", 79)},
			seen:     rcSeen("g1", "g2", "g3"),
			gap:      1, imported: 1, skipped: 1, skippedDays: []string{"2026-03-01"}},
		{name: "bulk import still refreshes groups imported earlier on a manual day",
			existing: []models.ScaleReading{rcStored(1, "", "2026-03-01T08:00", 81), rcStored(2, "g1", "2026-03-01T07:00", 80)},
			fetched:  []models.ScaleReading{rcFetch("g1", "2026-03-01T07:00", 79)}, seen: rcSeen("g1"),
			other: 1, updated: 1},
		{name: "day re-import imports on a manual day and keeps the manual reading",
			existing: []models.ScaleReading{rcStored(1, "", "2026-03-01T08:00", 81)},
			fetched:  []models.ScaleReading{rcFetch("g1", "2026-03-01T07:00", 80)}, seen: rcSeen("g1"), dayMode: true,
			other: 1, imported: 1},
		{name: "day re-import removes groups the source no longer returns",
			existing: []models.ScaleReading{rcStored(1, "g1", "2026-03-01T07:00", 80), rcStored(2, "g2", "2026-03-01T19:00", 81), rcStored(3, "", "2026-03-01T09:00", 82), rcStored(4, "g9", "2026-03-02T07:00", 70)},
			fetched:  []models.ScaleReading{rcFetch("g1", "2026-03-01T07:00", 80)}, seen: rcSeen("g1"), dayMode: true,
			unchanged: 1, del: 1, removed: 1},
		{name: "day re-import keeps groups that were returned but filtered out",
			existing: []models.ScaleReading{rcStored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  nil, seen: rcSeen("g1"), dayMode: true},
		{name: "bulk import never deletes",
			existing: []models.ScaleReading{rcStored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  nil, seen: rcSeen(), dayMode: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := plan(rcSource, tc.existing, tc.fetched, tc.seen, tc.dayMode, day, nil)
			got := [...]int{len(c.Gap), len(c.Other), len(c.Delete), c.Imported, c.Updated, c.Unchanged, c.Removed, c.Skipped}
			want := [...]int{tc.gap, tc.other, tc.del, tc.imported, tc.updated, tc.unchanged, tc.removed, tc.skipped}
			if got != want {
				t.Errorf("gap,other,del,imported,updated,unchanged,removed,skipped = %v, want %v", got, want)
			}
			if !reflect.DeepEqual(c.SkippedDays, tc.skippedDays) {
				t.Errorf("skipped days %v, want %v", c.SkippedDays, tc.skippedDays)
			}
		})
	}
}

func TestPlanDeletesOnlyThatDaysIDs(t *testing.T) {
	existing := []models.ScaleReading{rcStored(7, "g1", "2026-03-01T07:00", 80), rcStored(8, "g2", "2026-03-02T07:00", 80)}
	c := plan(rcSource, existing, nil, rcSeen(), true, rcTs("2026-03-01T00:00"), nil)
	if !reflect.DeepEqual(c.Delete, []int64{7}) {
		t.Fatalf("delete %v, want [7]", c.Delete)
	}
}

func TestReconcileWindowCoversTheDaysOfWhatIsFetchedAndTheDayReimported(t *testing.T) {
	id := func(s string) *string { return &s }
	at := func(d int) time.Time { return time.Date(2026, 3, d, 8, 0, 0, 0, time.UTC) }
	fetched := []models.ScaleReading{
		{MeasuredAt: at(12), ExternalID: id("b")},
		{MeasuredAt: at(10), ExternalID: id("a")},
		{MeasuredAt: at(14), ExternalID: id("c")},
	}

	since, until, ids := reconcileWindow(fetched, false, time.Time{})
	if !since.Equal(at(10).Add(-reconcileMargin)) || !until.Equal(at(14).Add(reconcileMargin)) || len(ids) != 3 || ids[0] != "b" {
		t.Fatalf("fetched only: %v %v %v", since, until, ids)
	}
	if reconcileMargin < 38*time.Hour {
		t.Fatalf("the margin %v does not keep a whole day around a time in any zone", reconcileMargin)
	}

	day := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	since, until, _ = reconcileWindow(fetched, true, day)
	if !since.Equal(at(10).Add(-reconcileMargin)) || !until.Equal(day.Add(reconcileMargin)) {
		t.Fatalf("a day after what was fetched: %v %v", since, until)
	}
	early := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	since, _, _ = reconcileWindow(fetched, true, early)
	if !since.Equal(early.Add(-reconcileMargin)) {
		t.Fatalf("a day before what was fetched: %v", since)
	}

	since, until, ids = reconcileWindow(nil, true, day)
	if !since.Equal(day.Add(-reconcileMargin)) || !until.Equal(day.Add(reconcileMargin)) || len(ids) != 0 {
		t.Fatalf("a day with nothing fetched: %v %v %v", since, until, ids)
	}
	since, until, ids = reconcileWindow(nil, false, time.Time{})
	if !since.IsZero() || !until.IsZero() || len(ids) != 0 {
		t.Fatalf("nothing at all: %v %v %v", since, until, ids)
	}
}

func TestAnImportLeavesTheOldHistoryAloneAndStillFindsAReadingTheSourceMoved(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	alice := r.sub(0)
	ext := func(id string, d int, w float64) models.ScaleReading {
		return models.ScaleReading{MeasuredAt: day(d), WeightKg: fl(w), Source: "demo-scale", ExternalID: &id}
	}
	// A long history by the same source, years back, and one reading by hand.
	var history []models.ScaleReading
	for i := 0; i < 120; i++ {
		history = append(history, ext("h"+strconv.Itoa(i), -400-i, 70))
	}
	hand := models.ScaleReading{MeasuredAt: day(-500), WeightKg: fl(71), Source: models.SourceManual}
	history = append(history, hand)
	if _, err := r.client.UpsertReadings(ctx, alice, history); err != nil {
		t.Fatal(err)
	}

	// The source moved reading h5 from long ago to today and sends a new one for today as well.
	req := bodyapi.ReconcileRequest{
		Source:  "demo-scale",
		Fetched: []models.ScaleReading{ext("h5", 0, 72), ext("fresh", 0, 73)},
		Seen:    []string{"h5", "fresh"},
	}
	res, err := r.client.Reconcile(ctx, alice, req)
	if err != nil || res.Updated != 1 || res.Imported != 1 || res.Unchanged != 0 {
		t.Fatalf("import: %+v %v, want the moved reading updated and the new one imported", res, err)
	}
	all, _, err := r.client.Readings(ctx, alice, bodyapi.Page{})
	if err != nil || len(all) != 122 {
		t.Fatalf("%d readings, %v; want the 121 of the history and 1 new, with h5 moved and not duplicated", len(all), err)
	}
	h5 := 0
	for _, s := range all {
		if s.ExternalID != nil && *s.ExternalID == "h5" {
			h5++
			if !s.MeasuredAt.Equal(day(0)) {
				t.Errorf("h5 is at %v, want it moved to today", s.MeasuredAt)
			}
		}
	}
	if h5 != 1 {
		t.Fatalf("h5 is stored %d times", h5)
	}

	// Sending the same again changes nothing.
	res, err = r.client.Reconcile(ctx, alice, req)
	if err != nil || res.Imported != 0 || res.Updated != 0 || res.Unchanged != 2 {
		t.Fatalf("repeated import: %+v %v", res, err)
	}

	// Rows an import has no business with are not even read: one that cannot be opened (the manual
	// reading of a year ago) does not stop it.
	if err := r.st.(*sqlstore.Store).ExecForTests(ctx, `UPDATE scale_readings SET data = '{not json' WHERE source = 'manual'`); err != nil {
		t.Fatal(err)
	}
	req.Fetched, req.Seen = append(req.Fetched, ext("fresh2", 1, 74)), append(req.Seen, "fresh2")
	res, err = r.client.Reconcile(ctx, alice, req)
	if err != nil || res.Imported != 1 {
		t.Fatalf("import beside an unreadable old row: %+v %v", res, err)
	}
}
