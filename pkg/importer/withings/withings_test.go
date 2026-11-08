package withings

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

// fakeWithings is a minimal Withings API: token endpoint and a paginated, date-filtered getmeas.
type fakeWithings struct {
	mu       sync.Mutex
	groups   []measureGroup
	pageSize int
	status   int // non-zero makes every call fail with this API status
	tokens   []url.Values
	meas     []url.Values
	bearers  []string
}

func (f *fakeWithings) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ParseForm()
	reply := func(body any) {
		b, _ := json.Marshal(map[string]any{"status": f.status, "error": "boom", "body": body})
		w.Write(b)
	}
	switch r.URL.Path {
	case "/v2/oauth2":
		f.tokens = append(f.tokens, r.PostForm)
		n := strconv.Itoa(len(f.tokens))
		reply(map[string]any{"userid": 123, "access_token": "at-" + n, "refresh_token": "rt-" + n, "expires_in": 10800})
	case "/measure":
		f.meas = append(f.meas, r.PostForm)
		f.bearers = append(f.bearers, r.Header.Get("Authorization"))
		var in []measureGroup
		for _, g := range f.groups {
			if s, err := strconv.ParseInt(r.PostForm.Get("startdate"), 10, 64); err == nil && g.Date < s {
				continue
			}
			if e, err := strconv.ParseInt(r.PostForm.Get("enddate"), 10, 64); err == nil && g.Date > e {
				continue
			}
			in = append(in, g)
		}
		off, _ := strconv.Atoi(r.PostForm.Get("offset"))
		end, more := len(in), 0
		if f.pageSize > 0 && off+f.pageSize < len(in) {
			end, more = off+f.pageSize, 1
		}
		if off > len(in) {
			off = len(in)
		}
		reply(map[string]any{"measuregrps": in[off:end], "more": more, "offset": end})
	default:
		http.NotFound(w, r)
	}
}

func grp(id int64, when string, kg int64) measureGroup {
	return measureGroup{GrpID: id, Category: 1, Date: ts(when).Unix(), Measures: []measure{{Value: kg, Type: 1, Unit: -3}}}
}

func setup(t *testing.T) (*Importer, *fakeWithings, store.Store) {
	t.Helper()
	f := &fakeWithings{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateUser(context.Background(), "u"); err != nil {
		t.Fatal(err)
	}
	imp := New(Config{ClientID: "id", ClientSecret: "secret", RedirectURL: "http://app/cb", APIBase: srv.URL}, nil)
	return imp, f, st
}

func TestConnectAndBulkImport(t *testing.T) {
	ctx := context.Background()
	imp, f, st := setup(t)
	f.pageSize = 2
	f.groups = []measureGroup{grp(1, "2026-03-01T07:00", 80500), grp(2, "2026-03-02T07:00", 80250), grp(3, "2026-03-03T07:00", 79900)}

	if _, err := imp.Import(ctx, st, 1, importer.Options{}); !errors.Is(err, importer.ErrNotConnected) {
		t.Fatalf("before connect: %v", err)
	}
	if imp.Connected(ctx, st, 1) {
		t.Fatal("must not be connected yet")
	}
	if err := imp.Connect(ctx, st, 1, "the-code"); err != nil {
		t.Fatal(err)
	}
	if got := f.tokens[0]; got.Get("grant_type") != "authorization_code" || got.Get("code") != "the-code" || got.Get("redirect_uri") != "http://app/cb" || got.Get("client_secret") != "secret" {
		t.Fatalf("token request: %v", got)
	}
	if !imp.Connected(ctx, st, 1) {
		t.Fatal("must be connected")
	}

	res, err := imp.Import(ctx, st, 1, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 3 || len(f.meas) != 2 {
		t.Fatalf("result %+v after %d getmeas calls (want 3 imported over 2 pages)", res, len(f.meas))
	}
	if f.bearers[0] != "Bearer at-1" || f.meas[0].Get("meastypes") != measTypes || f.meas[0].Get("category") != "1" {
		t.Fatalf("getmeas request: %v %v", f.bearers, f.meas[0])
	}
	rs, _ := st.ListScaleReadings(ctx, 1)
	if len(rs) != 3 || *rs[2].WeightKg != 80.5 || rs[2].Source != Name || rs[2].Method != models.MethodScale || rs[2].AthleteMode {
		t.Fatalf("stored: %+v", rs)
	}

	res, err = imp.Import(ctx, st, 1, importer.Options{})
	if err != nil || res.Unchanged != 3 || res.Imported != 0 || res.Updated != 0 {
		t.Fatalf("second import: %+v %v", res, err)
	}
	if rs, _ := st.ListScaleReadings(ctx, 1); len(rs) != 3 {
		t.Fatalf("not idempotent: %d rows", len(rs))
	}

	// since limits the fetch
	if _, err := imp.Import(ctx, st, 1, importer.Options{Since: ts("2026-03-02T00:00")}); err != nil {
		t.Fatal(err)
	}
	if last := f.meas[len(f.meas)-1]; last.Get("startdate") != strconv.FormatInt(ts("2026-03-02T00:00").Unix(), 10) || last.Get("enddate") != "" {
		t.Fatalf("since request: %v", last)
	}

	if err := imp.Disconnect(ctx, st, 1); err != nil {
		t.Fatal(err)
	}
	if imp.Connected(ctx, st, 1) {
		t.Fatal("still connected")
	}
}

func TestManualDaysAndDayReimport(t *testing.T) {
	ctx := context.Background()
	imp, f, st := setup(t)
	if err := imp.Connect(ctx, st, 1, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertScaleReadings(ctx, 1, []models.ScaleReading{{MeasuredAt: ts("2026-03-01T09:00"), WeightKg: fl(81)}}); err != nil {
		t.Fatal(err)
	}
	f.groups = []measureGroup{grp(1, "2026-03-01T07:00", 80000), grp(2, "2026-03-02T07:00", 79000)}

	res, err := imp.Import(ctx, st, 1, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 1 || res.Skipped != 1 || res.SkippedDays[0] != "2026-03-01" {
		t.Fatalf("bulk: %+v", res)
	}

	// re-importing the manual day is an explicit request: Withings reading is added, manual one stays
	res, err = imp.Import(ctx, st, 1, importer.Options{Day: ts("2026-03-01T00:00")})
	if err != nil || res.Imported != 1 || res.Skipped != 0 {
		t.Fatalf("day: %+v %v", res, err)
	}
	last := f.meas[len(f.meas)-1]
	if last.Get("startdate") != strconv.FormatInt(ts("2026-03-01T00:00").Unix(), 10) || last.Get("enddate") != strconv.FormatInt(ts("2026-03-01T23:59").Unix()+59, 10) {
		t.Fatalf("day window: %v", last)
	}
	if rs, _ := st.ListScaleReadings(ctx, 1); len(rs) != 3 {
		t.Fatalf("want manual + 2 withings, got %d", len(rs))
	}

	// Withings replaces the group on that day: the old one is removed, the new one imported
	f.groups = []measureGroup{grp(5, "2026-03-01T07:30", 80100), grp(2, "2026-03-02T07:00", 79000)}
	res, err = imp.Import(ctx, st, 1, importer.Options{Day: ts("2026-03-01T12:00")})
	if err != nil || res.Imported != 1 || res.Removed != 1 {
		t.Fatalf("replace: %+v %v", res, err)
	}
	rs, _ := st.ListScaleReadings(ctx, 1)
	ids := map[string]bool{}
	for _, r := range rs {
		if r.ExternalID != nil {
			ids[*r.ExternalID] = true
		}
	}
	if len(rs) != 3 || !ids["5"] || ids["1"] || !ids["2"] {
		t.Fatalf("after replace: %+v", rs)
	}
}

func TestTokenRefresh(t *testing.T) {
	ctx := context.Background()
	imp, f, st := setup(t)
	f.groups = []measureGroup{grp(1, "2026-03-01T07:00", 80000)}
	old, _ := json.Marshal(tokens{AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(-time.Hour)})
	if err := st.PutIntegration(ctx, 1, Name, old); err != nil {
		t.Fatal(err)
	}
	if _, err := imp.Import(ctx, st, 1, importer.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := f.tokens[0]; got.Get("grant_type") != "refresh_token" || got.Get("refresh_token") != "old-rt" {
		t.Fatalf("refresh request: %v", got)
	}
	if f.bearers[0] != "Bearer at-1" {
		t.Fatalf("getmeas used %q", f.bearers[0])
	}
	data, _ := st.GetIntegration(ctx, 1, Name)
	var saved tokens
	json.Unmarshal(data, &saved)
	if saved.RefreshToken != "rt-1" || !saved.ExpiresAt.After(time.Now()) {
		t.Fatalf("rotated tokens not persisted: %+v", saved)
	}
	// a fresh token is reused without another refresh
	if _, err := imp.Import(ctx, st, 1, importer.Options{}); err != nil || len(f.tokens) != 1 {
		t.Fatalf("second import: %v, %d token calls", err, len(f.tokens))
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	imp, f, st := setup(t)
	if err := imp.Connect(ctx, st, 1, "c"); err != nil {
		t.Fatal(err)
	}
	f.status = 401
	if _, err := imp.Import(ctx, st, 1, importer.Options{}); err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("api error: %v", err)
	}

	unconfigured := New(Config{}, nil)
	if unconfigured.Configured() {
		t.Fatal("must not be configured")
	}
	if _, err := unconfigured.Import(ctx, st, 1, importer.Options{}); !errors.Is(err, importer.ErrNotConfigured) {
		t.Fatalf("unconfigured: %v", err)
	}
}

func TestAuthURL(t *testing.T) {
	u, err := url.Parse(New(Config{ClientID: "id", ClientSecret: "s", RedirectURL: "http://app/cb"}, nil).AuthURL("st"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "account.withings.com" || q.Get("client_id") != "id" || q.Get("state") != "st" || q.Get("redirect_uri") != "http://app/cb" || q.Get("scope") != "user.metrics" || q.Get("response_type") != "code" {
		t.Fatalf("auth url: %s", u)
	}
}

func TestImportFollowsAthleteDefault(t *testing.T) {
	ctx := context.Background()
	imp, f, st := setup(t)
	f.groups = []measureGroup{grp(1, "2026-03-01T07:00", 80500)}
	if err := st.UpdateUserSettings(ctx, 1, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := imp.Connect(ctx, st, 1, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := imp.Import(ctx, st, 1, importer.Options{}); err != nil {
		t.Fatal(err)
	}
	rs, _ := st.ListScaleReadings(ctx, 1)
	if len(rs) != 1 || rs[0].Method != models.MethodScale || !rs[0].AthleteMode {
		t.Fatalf("stored: %+v", rs)
	}
}
