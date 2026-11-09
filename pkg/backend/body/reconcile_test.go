package bodybackend

import (
	"reflect"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
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
