package mainbackend

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strconv"
	"strings"
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

func TestReconcileFollowsTheUsersAthleteDefaultAndIsIdempotent(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	if err := f.st.UpdateUserSettings(ctx, f.user.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	u, _ := f.st.GetUser(ctx, f.user.ID)
	fetch := func() []models.ScaleReading { return []models.ScaleReading{rcFetch("g1", "2026-03-01T07:00", 80.5)} }

	res, err := f.b.reconcileReadings(ctx, u, rcSource, fetch(), rcSeen("g1"), false, time.Time{})
	if err != nil || res.Imported != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	stored, _ := f.st.ListScaleReadings(ctx, u.ID)
	if len(stored) != 1 || !stored[0].AthleteMode || stored[0].Source != rcSource {
		t.Fatalf("stored: %+v", stored)
	}
	// Applying the same result again changes nothing, which is what lets a worker report twice.
	res, err = f.b.reconcileReadings(ctx, u, rcSource, fetch(), rcSeen("g1"), false, time.Time{})
	if err != nil || res.Unchanged != 1 || res.Imported != 0 {
		t.Fatalf("second: %+v %v", res, err)
	}
	if stored, _ = f.st.ListScaleReadings(ctx, u.ID); len(stored) != 1 {
		t.Fatalf("not idempotent: %d rows", len(stored))
	}
}

func TestWithingsResultFromAWorkerIsValidated(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	apply := func(userID int64, result string) (string, error) {
		return applyWithingsImport(ctx, f.b, models.Job{UserID: userID, Kind: kindWithingsImport}, json.RawMessage(result))
	}
	ext := `"g1"`
	for name, result := range map[string]string{
		"not json":          `{`,
		"another source":    `{"readings":[{"source":"somewhere","external_id":` + ext + `,"measured_at":"2026-03-01T07:00:00Z","weight_kg":80}]}`,
		"no external id":    `{"readings":[{"source":"withings","measured_at":"2026-03-01T07:00:00Z","weight_kg":80}]}`,
		"no time":           `{"readings":[{"source":"withings","external_id":` + ext + `,"weight_kg":80}]}`,
		"unknown user":      "",
		"unreadable result": `[1,2]`,
	} {
		uid := f.user.ID
		if name == "unknown user" {
			uid, result = 9999, `{"readings":[]}`
		}
		if _, err := apply(uid, result); !errors.Is(err, errBadResult) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A reading that names another user or id is stored for the job's user, with a fresh id.
	good := `{"readings":[{"id":777,"user_id":` + strconv.FormatInt(f.admin.ID, 10) + `,"source":"withings","external_id":` + ext + `,"measured_at":"2026-03-01T07:00:00Z","weight_kg":80}],"seen":["g1"]}`
	out, err := apply(f.user.ID, good)
	if err != nil || !strings.Contains(out, `"imported":1`) {
		t.Fatalf("good result: %q %v", out, err)
	}
	if rs, _ := f.st.ListScaleReadings(ctx, f.admin.ID); len(rs) != 0 {
		t.Fatalf("a worker put a reading on another user: %+v", rs)
	}
	if rs, _ := f.st.ListScaleReadings(ctx, f.user.ID); len(rs) != 1 || rs[0].ID == 777 {
		t.Fatalf("stored: %+v", rs)
	}
}

func TestWithingsAuthURLCarriesTheStateAndTheAdminsCredentials(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	p, _ := f.b.providerNamed(providerWithings)
	if ok, _ := p.OAuth.Configured(ctx, f.b); ok {
		t.Fatal("configured without credentials")
	}
	if _, ok := f.b.oauthProvider(ctx, providerWithings); ok {
		t.Fatal("an unconfigured provider offers a connection")
	}
	setWithingsApp(t, f.st)
	if _, ok := f.b.oauthProvider(ctx, providerWithings); !ok {
		t.Fatal("configured provider is not offered")
	}
	raw, err := p.OAuth.AuthURL(ctx, f.b, "st")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "account.withings.com" || q.Get("client_id") != "id" || q.Get("state") != "st" || q.Get("redirect_uri") != "http://app/integrations/withings/callback" || q.Get("scope") != "user.metrics" || q.Get("response_type") != "code" {
		t.Fatalf("auth url: %s", u)
	}
	if strings.Contains(raw, "secret") || strings.Contains(raw, "client_secret") {
		t.Fatal("the secret is in the URL")
	}
}
