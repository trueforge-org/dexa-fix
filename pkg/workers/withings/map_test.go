package withings

import (
	"reflect"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
)

func fl(v float64) *float64 { return &v }

func ts(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04", s)
	if err != nil {
		panic(err)
	}
	return t
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
	if r.Source != Provider || *r.ExternalID != "1" || !r.MeasuredAt.Equal(ts("2026-03-01T07:00")) {
		t.Errorf("bad identity: %+v", r)
	}
	if !reflect.DeepEqual(seen, seenOf("1", "2", "3", "4")) {
		t.Errorf("seen %v", seen)
	}
}

// Values typed into the Withings app are not the scale's measurements: they get their own method.
func TestMapGroupsMarksTypedInValues(t *testing.T) {
	at := ts("2026-03-01T07:00").Unix()
	kg := []measure{{Value: 80000, Type: 1, Unit: -3}}
	groups := []measureGroup{
		{GrpID: 1, Category: 1, Attrib: 0, Date: at, Measures: kg},      // measured by the scale
		{GrpID: 2, Category: 1, Attrib: 2, Date: at + 60, Measures: kg}, // typed in
		{GrpID: 3, Category: 1, Attrib: 4, Date: at + 120, Measures: kg},
		{GrpID: 4, Category: 1, Attrib: 7, Date: at + 180, Measures: kg}, // confirmed by the user
	}
	rs, _ := mapGroups(groups)
	got := map[string]string{}
	for _, r := range rs {
		got[*r.ExternalID] = r.Method
	}
	want := map[string]string{"1": models.MethodScale, "2": models.MethodManualEntry, "3": models.MethodManualEntry, "4": models.MethodScale}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
}

func seenOf(ids ...string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}
