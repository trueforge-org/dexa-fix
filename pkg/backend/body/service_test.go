package bodybackend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexafix/pkg/store"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

const testSecret = "body-secret-for-tests"

type rig struct {
	svc    *Service
	client *bodyapi.Client
	signer *bodyapi.Signer
	ts     *httptest.Server
	st     store.Store
	users  []models.User
}

// newRig starts a body service over an in-memory store, with three users, and a client for it.
func newRig(t testing.TB) *rig {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, pub, priv, err := bodyapi.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(st, testSecret, bodyapi.NewVerifier(pub), WithVersion("test"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(svc.Handler())
	t.Cleanup(ts.Close)
	signer := bodyapi.NewSigner(priv)
	c, err := bodyapi.New(ts.URL, ts.Client(), testSecret, signer)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{svc: svc, client: c, signer: signer, ts: ts, st: st}
	for _, name := range []string{"alice", "bob", "carol"} {
		u, err := st.CreateUser(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		r.users = append(r.users, u)
	}
	return r
}

func (r *rig) sub(i int) bodyapi.Subject {
	return bodyapi.Subject{ID: r.users[i].ID, Params: bodyapi.EstimateParams{Timezone: "UTC", Ranking: models.DefaultRanking()}}
}

func fl(v float64) *float64 { return &v }

func day(n int) time.Time { return time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC).AddDate(0, 0, n) }

// do sends a raw request with the given headers, for the cases the client never produces.
func (r *rig) do(t *testing.T, method, path string, header map[string]string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, r.ts.URL+bodyapi.Prefix+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := r.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func (r *rig) token(subject int64, scope bodyapi.Scope) string {
	return r.signer.Sign(subject, scope, &bodyapi.EstimateParams{Timezone: "UTC"}, time.Now())
}

func TestEveryCallNeedsTheSecretAndAValidToken(t *testing.T) {
	r := newRig(t)
	uid := r.users[0].ID
	path := "/users/" + itoa(uid) + "/readings"
	good := map[string]string{bodyapi.HeaderSecret: testSecret, bodyapi.HeaderToken: r.token(uid, bodyapi.ScopeRead)}
	if resp := r.do(t, "GET", path, good, ""); resp.StatusCode != 200 {
		t.Fatalf("control: %d", resp.StatusCode)
	}

	_, _, otherPriv, _ := bodyapi.GenerateKey()
	expired := bodyapi.NewSigner(otherPriv)
	for name, h := range map[string]map[string]string{
		"no headers":         {},
		"no secret":          {bodyapi.HeaderToken: good[bodyapi.HeaderToken]},
		"wrong secret":       {bodyapi.HeaderSecret: "nope", bodyapi.HeaderToken: good[bodyapi.HeaderToken]},
		"no token":           {bodyapi.HeaderSecret: testSecret},
		"garbage token":      {bodyapi.HeaderSecret: testSecret, bodyapi.HeaderToken: "x.y"},
		"untrusted key":      {bodyapi.HeaderSecret: testSecret, bodyapi.HeaderToken: expired.Sign(uid, bodyapi.ScopeRead, nil, time.Now())},
		"expired token":      {bodyapi.HeaderSecret: testSecret, bodyapi.HeaderToken: r.signer.Sign(uid, bodyapi.ScopeRead, nil, time.Now().Add(-time.Hour))},
		"token made to last": {bodyapi.HeaderSecret: testSecret, bodyapi.HeaderToken: r.signer.Sign(uid, bodyapi.ScopeRead, nil, time.Now().Add(time.Hour))},
	} {
		if resp := r.do(t, "GET", path, h, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}
	// Even without a secret the health check is open: it tells nothing.
	if resp := r.do(t, "GET", "/healthz", nil, ""); resp.StatusCode != 200 {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
}

func TestATokenOpensOnlyItsOwnUserAndScope(t *testing.T) {
	r := newRig(t)
	a, b := r.users[0].ID, r.users[1].ID
	hdr := func(subject int64, scope bodyapi.Scope) map[string]string {
		return map[string]string{bodyapi.HeaderSecret: testSecret, bodyapi.HeaderToken: r.token(subject, scope)}
	}
	for name, tc := range map[string]struct {
		method, path string
		header       map[string]string
		body         string
		want         int
	}{
		"another user's readings":        {"GET", "/users/" + itoa(b) + "/readings", hdr(a, bodyapi.ScopeAdmin), "", 403},
		"another user's estimates":       {"GET", "/users/" + itoa(b) + "/estimates", hdr(a, bodyapi.ScopeRead), "", 403},
		"another user's export":          {"GET", "/users/" + itoa(b) + "/export", hdr(a, bodyapi.ScopeRead), "", 403},
		"write with a read token":        {"POST", "/users/" + itoa(a) + "/readings", hdr(a, bodyapi.ScopeRead), "[]", 403},
		"delete with a read token":       {"DELETE", "/users/" + itoa(a) + "/scans/1", hdr(a, bodyapi.ScopeRead), "", 403},
		"delete a user's data, write":    {"DELETE", "/users/" + itoa(a), hdr(a, bodyapi.ScopeWrite), "", 403},
		"delete another user's data":     {"DELETE", "/users/" + itoa(b), hdr(a, bodyapi.ScopeAdmin), "", 403},
		"totals with a user's token":     {"GET", "/stats", hdr(a, bodyapi.ScopeAdmin), "", 403},
		"totals with a read token":       {"GET", "/stats", hdr(0, bodyapi.ScopeRead), "", 403},
		"user ids with a write token":    {"GET", "/users", hdr(0, bodyapi.ScopeWrite), "", 403},
		"a user id that is not a number": {"GET", "/users/abc/readings", hdr(a, bodyapi.ScopeRead), "", 403},
		"admin totals":                   {"GET", "/stats", hdr(0, bodyapi.ScopeAdmin), "", 200},
		"own readings with write":        {"GET", "/users/" + itoa(a) + "/readings", hdr(a, bodyapi.ScopeWrite), "", 200},
	} {
		if resp := r.do(t, tc.method, tc.path, tc.header, tc.body); resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, tc.want)
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestRoundTripThroughTheClient(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	alice, bob := r.sub(0), r.sub(1)

	// Scans: whose scan it is comes from the token, whatever the body says.
	scan := models.DexaScan{UserID: bob.ID, MeasuredAt: day(0), WeightKg: fl(80), FatRatioPct: fl(20), Notes: "n"}
	if err := r.client.CreateScan(ctx, alice, &scan); err != nil {
		t.Fatal(err)
	}
	if scan.ID == 0 || scan.UserID != alice.ID {
		t.Fatalf("created scan: %+v", scan)
	}
	if scans, _, err := r.client.Scans(ctx, bob, bodyapi.Page{}); err != nil || len(scans) != 0 {
		t.Fatalf("bob sees %v, %v", scans, err)
	}
	scans, next, err := r.client.Scans(ctx, alice, bodyapi.Page{})
	if err != nil || len(scans) != 1 || next != "" || *scans[0].WeightKg != 80 || scans[0].Notes != "n" {
		t.Fatalf("alice's scans: %+v next %q err %v", scans, next, err)
	}

	// Readings, with paging.
	var batch []models.ScaleReading
	for i := 0; i < 25; i++ {
		batch = append(batch, models.ScaleReading{MeasuredAt: day(i), WeightKg: fl(70 + float64(i)), UserID: bob.ID})
	}
	if n, err := r.client.UpsertReadings(ctx, alice, batch); err != nil || n != 25 {
		t.Fatalf("upsert: %d, %v", n, err)
	}
	if rs, _, _ := r.client.Readings(ctx, bob, bodyapi.Page{}); len(rs) != 0 {
		t.Fatalf("bob sees alice's readings: %d", len(rs))
	}
	var seen []models.ScaleReading
	var cursor string
	for pages := 0; ; pages++ {
		page, nxt, err := r.client.Readings(ctx, alice, bodyapi.Page{Limit: 10, Before: cursor})
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, page...)
		if nxt == "" {
			break
		}
		if pages > 5 {
			t.Fatal("paging does not end")
		}
		cursor = nxt
	}
	if len(seen) != 25 || !seen[0].MeasuredAt.After(seen[24].MeasuredAt) {
		t.Fatalf("paged %d readings, newest first: %v", len(seen), len(seen) > 0 && seen[0].MeasuredAt.After(seen[len(seen)-1].MeasuredAt))
	}
	win, _, err := r.client.Readings(ctx, alice, bodyapi.Page{Since: day(10), Until: day(12)})
	if err != nil || len(win) != 3 {
		t.Fatalf("window: %d, %v", len(win), err)
	}

	// Deleting: another user's id is not found, one's own works.
	if err := r.client.DeleteReading(ctx, bob, seen[0].ID); !errors.Is(err, bodyapi.ErrNotFound) {
		t.Errorf("deleting another user's reading: %v", err)
	}
	if err := r.client.DeleteReading(ctx, alice, seen[0].ID); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := r.client.DeleteScan(ctx, alice, scan.ID); err != nil {
		t.Errorf("delete scan: %v", err)
	}

	exp, err := r.client.Export(ctx, alice)
	if err != nil || len(exp.ScaleReadings) != 24 || len(exp.DexaScans) != 0 {
		t.Fatalf("export: %d readings, %d scans, %v", len(exp.ScaleReadings), len(exp.DexaScans), err)
	}
	if st, err := r.client.Stats(ctx); err != nil || st.ScaleReadings != 24 || st.DexaScans != 0 {
		t.Fatalf("stats: %+v %v", st, err)
	}
	if ids, err := r.client.UserIDs(ctx); err != nil || len(ids) != 1 || ids[0] != alice.ID {
		t.Fatalf("user ids: %v %v", ids, err)
	}

	// Deleting a user's data leaves everyone else's alone.
	if _, err := r.client.UpsertReadings(ctx, bob, batch[:2]); err != nil {
		t.Fatal(err)
	}
	if err := r.client.DeleteUser(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	if rs, _, _ := r.client.Readings(ctx, alice, bodyapi.Page{}); len(rs) != 0 {
		t.Errorf("alice still has %d readings", len(rs))
	}
	if rs, _, _ := r.client.Readings(ctx, bob, bodyapi.Page{}); len(rs) != 2 {
		t.Errorf("bob has %d readings, want 2", len(rs))
	}
	if err := r.client.DeleteUser(ctx, alice.ID); err != nil {
		t.Errorf("deleting nothing is not an error: %v", err)
	}
}

func TestInvalidValuesAreRefusedAgain(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	alice := r.sub(0)
	bad := models.ScaleReading{MeasuredAt: day(0), WeightKg: fl(5000)}
	_, err := r.client.UpsertReadings(ctx, alice, []models.ScaleReading{{MeasuredAt: day(0), WeightKg: fl(70)}, bad})
	if !errors.Is(err, bodyapi.ErrInvalid) || !strings.Contains(err.Error(), "item 1: weight_kg must be between") {
		t.Fatalf("out of range reading: %v", err)
	}
	if rs, _, _ := r.client.Readings(ctx, alice, bodyapi.Page{}); len(rs) != 0 {
		t.Errorf("a batch with a bad item wrote %d readings", len(rs))
	}
	scan := models.DexaScan{MeasuredAt: day(0), TScore: fl(99)}
	if err := r.client.CreateScan(ctx, alice, &scan); !errors.Is(err, bodyapi.ErrInvalid) {
		t.Errorf("out of range scan: %v", err)
	}
	long := models.DexaScan{MeasuredAt: day(0), Notes: strings.Repeat("x", 501)}
	if err := r.client.CreateScan(ctx, alice, &long); !errors.Is(err, bodyapi.ErrInvalid) {
		t.Errorf("long notes: %v", err)
	}
	if _, err := r.client.UpsertReadings(ctx, alice, []models.ScaleReading{{}}); !errors.Is(err, bodyapi.ErrInvalid) {
		t.Errorf("a reading without a time: %v", err)
	}
	if _, _, err := r.client.Readings(ctx, alice, bodyapi.Page{Before: "junk"}); !errors.Is(err, bodyapi.ErrInvalid) {
		t.Errorf("junk cursor: %v", err)
	}
}

func TestEstimatesAreCachedAndFollowTheDataAndTheSettings(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	alice := r.sub(0)
	scan := models.DexaScan{MeasuredAt: day(0), WeightKg: fl(80), FatRatioPct: fl(20), FatMassKg: fl(16), FatFreeMassKg: fl(64), LeanMassKg: fl(60)}
	if err := r.client.CreateScan(ctx, alice, &scan); err != nil {
		t.Fatal(err)
	}
	var rs []models.ScaleReading
	for i := 0; i < 5; i++ {
		rs = append(rs, models.ScaleReading{MeasuredAt: day(i), WeightKg: fl(80 + float64(i)), FatRatioPct: fl(21)})
	}
	if _, err := r.client.UpsertReadings(ctx, alice, rs); err != nil {
		t.Fatal(err)
	}

	first, err := r.client.Estimates(ctx, alice, "")
	if err != nil || first.ETag == "" || first.NotModified || len(first.Result.Days) == 0 {
		t.Fatalf("first: %+v %v", first, err)
	}
	_, missesBefore := r.svc.CacheStats()
	again, err := r.client.Estimates(ctx, alice, first.ETag)
	if err != nil || !again.NotModified || again.ETag != first.ETag {
		t.Fatalf("revalidation: %+v %v", again, err)
	}
	third, err := r.client.Estimates(ctx, alice, "")
	if err != nil || len(third.Result.Days) != len(first.Result.Days) {
		t.Fatalf("third: %v", err)
	}
	if hits, misses := r.svc.CacheStats(); hits == 0 || misses != missesBefore {
		t.Errorf("not served from the cache: hits %d, misses %d (was %d)", hits, misses, missesBefore)
	}

	// Each kind of change makes the old answer stale.
	h := 181.0
	changed := map[string]func() error{
		"a new reading": func() error {
			_, err := r.client.UpsertReadings(ctx, alice, []models.ScaleReading{{MeasuredAt: day(9), WeightKg: fl(90)}})
			return err
		},
		"a deleted scan": func() error { return r.client.DeleteScan(ctx, alice, scan.ID) },
	}
	for name, change := range changed {
		old, _ := r.client.Estimates(ctx, alice, "")
		if err := change(); err != nil {
			t.Fatal(err)
		}
		now, err := r.client.Estimates(ctx, alice, old.ETag)
		if err != nil || now.NotModified || now.ETag == old.ETag {
			t.Errorf("%s: etag %q -> %q, not modified %v, %v", name, old.ETag, now.ETag, now.NotModified, err)
		}
	}
	for name, mutate := range map[string]func(p *bodyapi.EstimateParams){
		"another time zone":    func(p *bodyapi.EstimateParams) { p.Timezone = "Pacific/Auckland" },
		"another height":       func(p *bodyapi.EstimateParams) { p.HeightCm = &h },
		"athlete mode":         func(p *bodyapi.EstimateParams) { p.AthleteDefault = true },
		"another method order": func(p *bodyapi.EstimateParams) { p.Ranking = []string{"scale", "dexa"} },
	} {
		old, _ := r.client.Estimates(ctx, alice, "")
		s := alice
		s.Params.Ranking = append([]string(nil), alice.Params.Ranking...)
		mutate(&s.Params)
		now, err := r.client.Estimates(ctx, s, old.ETag)
		if err != nil || now.NotModified || now.ETag == old.ETag {
			t.Errorf("%s: etag %q -> %q, not modified %v, %v", name, old.ETag, now.ETag, now.NotModified, err)
		}
	}
	// Two users never share a cached result.
	bob, err := r.client.Estimates(ctx, r.sub(1), "")
	if err != nil || len(bob.Result.Days) != 0 {
		t.Errorf("bob's estimates: %d days, %v", len(bob.Result.Days), err)
	}
}

func TestReconcileThroughTheClientIsIdempotentAndFollowsTheTokensSettings(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	alice := r.sub(0)
	alice.Params.AthleteDefault = true
	ext := func(id string, d int, w float64) models.ScaleReading {
		return models.ScaleReading{MeasuredAt: day(d), WeightKg: fl(w), Source: "demo-scale", ExternalID: &id}
	}
	req := bodyapi.ReconcileRequest{
		Source:  "demo-scale",
		Fetched: []models.ScaleReading{ext("a", 0, 70), ext("b", 1, 71), ext("c", 2, 72)},
		Seen:    []string{"a", "b", "c"},
	}
	res, err := r.client.Reconcile(ctx, alice, req)
	if err != nil || res.Imported != 3 {
		t.Fatalf("first import: %+v %v", res, err)
	}
	stored, _, _ := r.client.Readings(ctx, alice, bodyapi.Page{})
	for _, s := range stored {
		if !s.AthleteMode {
			t.Errorf("reading %d did not follow the user's athlete default", s.ID)
		}
	}
	res, err = r.client.Reconcile(ctx, alice, req)
	if err != nil || res.Imported != 0 || res.Updated != 0 || res.Unchanged != 3 {
		t.Fatalf("repeated import: %+v %v", res, err)
	}
	if after, _, _ := r.client.Readings(ctx, alice, bodyapi.Page{}); len(after) != 3 {
		t.Fatalf("%d readings after a repeat", len(after))
	}

	// A re-import of one day removes what the source no longer returns for that day.
	req.DayMode, req.Day = true, day(1).Format("2006-01-02")
	req.Fetched, req.Seen = []models.ScaleReading{ext("b2", 1, 73)}, []string{"b2"}
	res, err = r.client.Reconcile(ctx, alice, req)
	if err != nil || res.Removed != 1 {
		t.Fatalf("day re-import: %+v %v", res, err)
	}

	// What a source sends is checked.
	for name, bad := range map[string]bodyapi.ReconcileRequest{
		"no source":         {Fetched: req.Fetched},
		"another source":    {Source: "other", Fetched: req.Fetched},
		"no external id":    {Source: "demo-scale", Fetched: []models.ScaleReading{{MeasuredAt: day(0), Source: "demo-scale"}}},
		"a bad day":         {Source: "demo-scale", DayMode: true, Day: "yesterday"},
		"an impossible one": {Source: "demo-scale", Fetched: []models.ScaleReading{ext("z", 0, 5000)}},
	} {
		if _, err := r.client.Reconcile(ctx, alice, bad); !errors.Is(err, bodyapi.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Calls that carry no user settings cannot reconcile or estimate: the body service never invents them.
func TestSettingsCallsNeedTheTokensSettings(t *testing.T) {
	r := newRig(t)
	uid := r.users[0].ID
	noParams := r.signer.Sign(uid, bodyapi.ScopeWrite, nil, time.Now())
	h := map[string]string{bodyapi.HeaderSecret: testSecret, bodyapi.HeaderToken: noParams}
	if resp := r.do(t, "GET", "/users/"+itoa(uid)+"/estimates", h, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("estimates without settings: %d", resp.StatusCode)
	}
	if resp := r.do(t, "POST", "/users/"+itoa(uid)+"/readings/reconcile", h, `{"source":"x"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("reconcile without settings: %d", resp.StatusCode)
	}
}

// A service that cannot be reached is "unavailable", whatever the reason, so callers can tell it
// from a refusal.
func TestAnUnreachableServiceIsUnavailable(t *testing.T) {
	r := newRig(t)
	r.ts.Close()
	_, _, err := r.client.Scans(context.Background(), r.sub(0), bodyapi.Page{})
	if !errors.Is(err, bodyapi.ErrUnavailable) {
		t.Fatalf("stopped service: %v", err)
	}
}

func TestConcurrentWritesAndEstimates(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	alice := r.sub(0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := r.client.UpsertReadings(ctx, alice, []models.ScaleReading{{MeasuredAt: day(i), WeightKg: fl(70)}}); err != nil {
				t.Error(err)
			}
			if _, err := r.client.Estimates(ctx, alice, ""); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if rs, _, _ := r.client.Readings(ctx, alice, bodyapi.Page{}); len(rs) != 8 {
		t.Errorf("%d readings", len(rs))
	}
}

func TestEstimatesWindowTrimsDaysNotCalibrations(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	alice := r.sub(0)
	scan := models.DexaScan{MeasuredAt: day(0), WeightKg: fl(80), FatRatioPct: fl(20), FatMassKg: fl(16), FatFreeMassKg: fl(64), LeanMassKg: fl(60)}
	if err := r.client.CreateScan(ctx, alice, &scan); err != nil {
		t.Fatal(err)
	}
	var rs []models.ScaleReading
	for i := 0; i < 40; i++ {
		rs = append(rs, models.ScaleReading{MeasuredAt: day(i), WeightKg: fl(80 + float64(i)/10), FatRatioPct: fl(21)})
	}
	if _, err := r.client.UpsertReadings(ctx, alice, rs); err != nil {
		t.Fatal(err)
	}

	full, err := r.client.Estimates(ctx, alice, "")
	if err != nil || len(full.Result.Days) < 10 {
		t.Fatalf("full: %d days, %v", len(full.Result.Days), err)
	}
	win, err := r.client.EstimatesWindow(ctx, alice, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(win.Result.Days) != 7 {
		t.Fatalf("a window of 7 gave %d days", len(win.Result.Days))
	}
	for i, d := range win.Result.Days {
		if !d.Date.Equal(full.Result.Days[i].Date) {
			t.Fatalf("day %d: %v, want %v (newest first, the same days)", i, d.Date, full.Result.Days[i].Date)
		}
	}
	if len(win.Result.Calibrations) != len(full.Result.Calibrations) {
		t.Errorf("calibrations were trimmed: %d of %d", len(win.Result.Calibrations), len(full.Result.Calibrations))
	}
	if win.ETag == "" || win.ETag == full.ETag {
		t.Errorf("a window needs an ETag of its own: %q and %q", win.ETag, full.ETag)
	}

	// A validator matches the representation it came with and no other.
	again, err := r.client.EstimatesWindow(ctx, alice, win.ETag, 7)
	if err != nil || !again.NotModified {
		t.Errorf("same window and ETag: %+v %v", again, err)
	}
	other, err := r.client.EstimatesWindow(ctx, alice, win.ETag, 30)
	if err != nil || other.NotModified || len(other.Result.Days) != 30 {
		t.Errorf("another window with the first window's ETag: %d days, not modified %v, %v", len(other.Result.Days), other.NotModified, err)
	}

	// A window longer than the history is the whole history.
	huge, err := r.client.EstimatesWindow(ctx, alice, "", 99999)
	if err != nil || len(huge.Result.Days) != len(full.Result.Days) {
		t.Errorf("a huge window: %d days of %d, %v", len(huge.Result.Days), len(full.Result.Days), err)
	}
	if _, err := r.client.EstimatesWindow(ctx, alice, "", maxEstimateWindow+1); err == nil {
		t.Error("a window beyond the bound must be refused")
	}
}
