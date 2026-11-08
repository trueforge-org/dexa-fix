package withings

import (
	"reflect"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

func fl(v float64) *float64 { return &v }

func ts(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

// stored builds a stored reading; ext == "" means a manual reading.
func stored(id int64, ext, when string, w float64) models.ScaleReading {
	r := models.ScaleReading{ID: id, MeasuredAt: ts(when), WeightKg: fl(w), Source: models.SourceManual}
	if ext != "" {
		r.Source, r.ExternalID = Name, &ext
	}
	return r
}

func fetch(ext, when string, w float64) models.ScaleReading {
	return models.ScaleReading{MeasuredAt: ts(when), WeightKg: fl(w), Source: Name, ExternalID: &ext}
}

func seenOf(ids ...string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func TestPlan(t *testing.T) {
	day := ts("2026-03-01T00:00")
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
		{name: "day without readings is filled", fetched: []models.ScaleReading{fetch("g1", "2026-03-01T07:00", 80)}, seen: seenOf("g1"),
			gap: 1, imported: 1},
		{name: "identical stored reading is left alone",
			existing: []models.ScaleReading{stored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  []models.ScaleReading{fetch("g1", "2026-03-01T07:00", 80)}, seen: seenOf("g1"),
			unchanged: 1},
		{name: "changed values are updated",
			existing: []models.ScaleReading{stored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  []models.ScaleReading{fetch("g1", "2026-03-01T07:00", 79.5)}, seen: seenOf("g1"),
			other: 1, updated: 1},
		{name: "group moved to another day is updated in place",
			existing: []models.ScaleReading{stored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  []models.ScaleReading{fetch("g1", "2026-03-02T07:00", 80)}, seen: seenOf("g1"),
			other: 1, updated: 1},
		{name: "new group next to an imported one is added",
			existing: []models.ScaleReading{stored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  []models.ScaleReading{fetch("g2", "2026-03-01T19:00", 80.4)}, seen: seenOf("g2"),
			other: 1, imported: 1},
		{name: "bulk import skips days with manual readings",
			existing: []models.ScaleReading{stored(1, "", "2026-03-01T08:00", 81)},
			fetched:  []models.ScaleReading{fetch("g1", "2026-03-01T07:00", 80), fetch("g2", "2026-03-01T19:00", 80.4), fetch("g3", "2026-03-02T07:00", 79)},
			seen:     seenOf("g1", "g2", "g3"),
			gap:      1, imported: 1, skipped: 1, skippedDays: []string{"2026-03-01"}},
		{name: "bulk import still refreshes groups imported earlier on a manual day",
			existing: []models.ScaleReading{stored(1, "", "2026-03-01T08:00", 81), stored(2, "g1", "2026-03-01T07:00", 80)},
			fetched:  []models.ScaleReading{fetch("g1", "2026-03-01T07:00", 79)}, seen: seenOf("g1"),
			other: 1, updated: 1},
		{name: "day re-import imports on a manual day and keeps the manual reading",
			existing: []models.ScaleReading{stored(1, "", "2026-03-01T08:00", 81)},
			fetched:  []models.ScaleReading{fetch("g1", "2026-03-01T07:00", 80)}, seen: seenOf("g1"), dayMode: true,
			other: 1, imported: 1},
		{name: "day re-import removes groups Withings no longer returns",
			existing: []models.ScaleReading{stored(1, "g1", "2026-03-01T07:00", 80), stored(2, "g2", "2026-03-01T19:00", 81), stored(3, "", "2026-03-01T09:00", 82), stored(4, "g9", "2026-03-02T07:00", 70)},
			fetched:  []models.ScaleReading{fetch("g1", "2026-03-01T07:00", 80)}, seen: seenOf("g1"), dayMode: true,
			unchanged: 1, del: 1, removed: 1},
		{name: "day re-import keeps groups that were returned but filtered out",
			existing: []models.ScaleReading{stored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  nil, seen: seenOf("g1"), dayMode: true},
		{name: "bulk import never deletes",
			existing: []models.ScaleReading{stored(1, "g1", "2026-03-01T07:00", 80)},
			fetched:  nil, seen: seenOf(), dayMode: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := plan(tc.existing, tc.fetched, tc.seen, tc.dayMode, day, nil)
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
	existing := []models.ScaleReading{stored(7, "g1", "2026-03-01T07:00", 80), stored(8, "g2", "2026-03-02T07:00", 80)}
	c := plan(existing, nil, seenOf(), true, ts("2026-03-01T00:00"), nil)
	if !reflect.DeepEqual(c.Delete, []int64{7}) {
		t.Fatalf("delete %v, want [7]", c.Delete)
	}
}

func TestMapGroups(t *testing.T) {
	groups := []measureGroup{
		{GrpID: 1, Category: 1, Date: ts("2026-03-01T07:00").Unix(), Measures: []measure{
			{Value: 65750, Type: 1, Unit: -3}, {Value: 215, Type: 6, Unit: -1}, {Value: 3, Type: 170, Unit: 0}, {Value: 99, Type: 54, Unit: 0}}},
		{GrpID: 2, Category: 1, Attrib: 1, Date: ts("2026-03-01T08:00").Unix(), Measures: []measure{{Value: 90000, Type: 1, Unit: -3}}}, // guest
		{GrpID: 3, Category: 2, Date: ts("2026-03-01T09:00").Unix(), Measures: []measure{{Value: 60000, Type: 1, Unit: -3}}},            // goal
		{GrpID: 4, Category: 1, Date: ts("2026-03-01T10:00").Unix(), Measures: []measure{{Value: 99, Type: 54, Unit: 0}}},               // nothing we map
	}
	rs, seen := mapGroups(groups)
	if len(rs) != 1 {
		t.Fatalf("got %d readings, want 1", len(rs))
	}
	r := rs[0]
	if *r.WeightKg != 65.75 || *r.FatRatioPct != 21.5 || *r.VisceralFatIndex != 3 || r.FatMassKg != nil {
		t.Errorf("bad mapping: %+v", r)
	}
	if r.Source != Name || *r.ExternalID != "1" || !r.MeasuredAt.Equal(ts("2026-03-01T07:00")) {
		t.Errorf("bad identity: %+v", r)
	}
	if !reflect.DeepEqual(seen, seenOf("1", "2", "3", "4")) {
		t.Errorf("seen %v", seen)
	}
}
