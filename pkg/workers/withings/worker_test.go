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

	"github.com/trueforge-org/dexa-fix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexa-fix/pkg/workers/runner"
)

// fakeWithings is a minimal Withings API: token endpoint and a paginated, date-filtered getmeas.
type fakeWithings struct {
	mu       sync.Mutex
	groups   []measureGroup
	pageSize int
	status   int // non-zero makes every call fail with this API status
	flaky    int // the next n /measure calls fail: with 503, or with API status 601 when flaky601
	flaky601 bool
	events   []string // "token", "measure" in the order they arrived
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
		f.events = append(f.events, "token")
		f.tokens = append(f.tokens, r.PostForm)
		n := strconv.Itoa(len(f.tokens))
		reply(map[string]any{"userid": 123, "access_token": "at-" + n, "refresh_token": "rt-" + n, "expires_in": 10800})
	case "/measure":
		f.events = append(f.events, "measure")
		f.meas = append(f.meas, r.PostForm)
		if f.flaky > 0 {
			f.flaky--
			if f.flaky601 {
				w.Write([]byte(`{"status":601,"error":"too many requests"}`))
			} else {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}
			return
		}
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

var app = workerapi.OAuthApp{ClientID: "id", ClientSecret: "secret", RedirectURL: "http://app/cb"}

// recorder stands in for the backend: it keeps what the worker hands over, in order with the calls
// to Withings.
type recorder struct {
	f     *fakeWithings
	saved []tokens
	err   error
}

func (r *recorder) save(ctx context.Context, v any) error {
	if r.err != nil {
		return r.err
	}
	r.f.mu.Lock()
	r.f.events = append(r.f.events, "saved")
	r.f.mu.Unlock()
	t, ok := v.(tokens)
	if !ok {
		return errors.New("not a token set")
	}
	r.saved = append(r.saved, t)
	return nil
}

func setup(t *testing.T) (*Worker, *fakeWithings, *recorder) {
	t.Helper()
	f := &fakeWithings{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	backoffBase = time.Millisecond
	t.Cleanup(func() { backoffBase = 500 * time.Millisecond })
	return New(Config{APIBase: srv.URL}, nil), f, &recorder{f: f}
}

func creds(t *testing.T, tk tokens) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(tk)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fresh() tokens {
	return tokens{AccessToken: "fresh-at", RefreshToken: "fresh-rt", ExpiresAt: time.Now().Add(time.Hour)}
}

func TestConnectHandsOverTheTokens(t *testing.T) {
	w, f, rec := setup(t)
	if err := w.connect(context.Background(), workerapi.ConnectPayload{App: app, Code: "the-code"}, rec.save); err != nil {
		t.Fatal(err)
	}
	if got := f.tokens[0]; got.Get("grant_type") != "authorization_code" || got.Get("code") != "the-code" || got.Get("redirect_uri") != "http://app/cb" || got.Get("client_secret") != "secret" || got.Get("client_id") != "id" {
		t.Fatalf("token request: %v", got)
	}
	if len(rec.saved) != 1 || rec.saved[0].AccessToken != "at-1" || rec.saved[0].RefreshToken != "rt-1" || rec.saved[0].UserID != "123" || !rec.saved[0].ExpiresAt.After(time.Now()) {
		t.Fatalf("handed over: %+v", rec.saved)
	}
}

func TestConnectFailures(t *testing.T) {
	ctx := context.Background()
	w, f, rec := setup(t)
	if err := w.connect(ctx, workerapi.ConnectPayload{Code: "c"}, rec.save); !runner.IsPermanent(err) {
		t.Fatalf("unconfigured: %v", err)
	}
	f.status = 503 // the API's own status for a refused code
	err := w.connect(ctx, workerapi.ConnectPayload{App: app, Code: "c"}, rec.save)
	if !runner.IsPermanent(err) || len(f.tokens) != 1 {
		t.Fatalf("a refused code must not be retried: %v after %d calls", err, len(f.tokens))
	}
	f.status = 0
	// If the tokens cannot be stored the job is retried; the code was already spent, so this is best effort.
	rec.err = errors.New("backend down")
	if err := w.connect(ctx, workerapi.ConnectPayload{App: app, Code: "c"}, rec.save); err == nil {
		t.Fatal("expected an error")
	} else if ok, _ := runner.IsRetryable(err); !ok {
		t.Fatalf("storing failed: %v", err)
	}
}

func TestBulkImportPagesAndMaps(t *testing.T) {
	w, f, rec := setup(t)
	f.pageSize = 2
	f.groups = []measureGroup{grp(1, "2026-03-01T07:00", 80500), grp(2, "2026-03-02T07:00", 80250), grp(3, "2026-03-03T07:00", 79900)}
	res, err := w.importReadings(context.Background(), workerapi.ImportPayload{App: app, Credentials: creds(t, fresh())}, rec.save)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Readings) != 3 || len(res.Seen) != 3 || len(f.meas) != 2 {
		t.Fatalf("result %+v after %d getmeas calls (want 3 readings over 2 pages)", res, len(f.meas))
	}
	if f.bearers[0] != "Bearer fresh-at" || f.meas[0].Get("meastypes") != measTypes || f.meas[0].Get("category") != "1" {
		t.Fatalf("getmeas request: %v %v", f.bearers, f.meas[0])
	}
	if len(f.tokens) != 0 || len(rec.saved) != 0 {
		t.Fatal("a fresh token was refreshed")
	}
	if r := res.Readings[0]; r.Source != Provider || r.ExternalID == nil || *r.WeightKg != 80.5 {
		t.Fatalf("reading: %+v", r)
	}
}

// A refresh token works once, so the new pair must reach the backend before it is used for anything.
func TestRefreshedTokensAreHandedOverBeforeTheyAreUsed(t *testing.T) {
	w, f, rec := setup(t)
	f.groups = []measureGroup{grp(1, "2026-03-01T07:00", 80000)}
	old := tokens{AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(-time.Hour)}
	if _, err := w.importReadings(context.Background(), workerapi.ImportPayload{App: app, Credentials: creds(t, old)}, rec.save); err != nil {
		t.Fatal(err)
	}
	if got := f.tokens[0]; got.Get("grant_type") != "refresh_token" || got.Get("refresh_token") != "old-rt" {
		t.Fatalf("refresh request: %v", got)
	}
	if strings.Join(f.events, ",") != "token,saved,measure" {
		t.Fatalf("order of events: %v", f.events)
	}
	if f.bearers[0] != "Bearer at-1" || len(rec.saved) != 1 || rec.saved[0].RefreshToken != "rt-1" {
		t.Fatalf("bearer %v saved %+v", f.bearers, rec.saved)
	}
}

func TestImportStopsWhenTheNewTokensCannotBeStored(t *testing.T) {
	w, f, rec := setup(t)
	rec.err = errors.New("backend down")
	old := tokens{AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(-time.Hour)}
	_, err := w.importReadings(context.Background(), workerapi.ImportPayload{App: app, Credentials: creds(t, old)}, rec.save)
	if !runner.IsPermanent(err) {
		t.Fatalf("expected a permanent failure (the old pair is spent): %v", err)
	}
	if len(f.meas) != 0 {
		t.Fatal("measurements were fetched with tokens the backend does not know")
	}
}

func TestImportWindows(t *testing.T) {
	ctx := context.Background()
	w, f, rec := setup(t)
	f.groups = []measureGroup{
		grp(1, "2026-03-01T06:00", 80000), // 17:00 on 1 March in Sydney
		grp(2, "2026-03-01T20:00", 80500), // 07:00 on 2 March in Sydney
	}
	// since limits the fetch
	if _, err := w.importReadings(ctx, workerapi.ImportPayload{App: app, Credentials: creds(t, fresh()), Since: ts("2026-03-02T00:00")}, rec.save); err != nil {
		t.Fatal(err)
	}
	if last := f.meas[len(f.meas)-1]; last.Get("startdate") != strconv.FormatInt(ts("2026-03-02T00:00").Unix(), 10) || last.Get("enddate") != "" {
		t.Fatalf("since request: %v", last)
	}
	// A day covers the user's calendar day: 07:00 on 2 March in Sydney (UTC+11) is 20:00 on 1 March in UTC.
	res, err := w.importReadings(ctx, workerapi.ImportPayload{App: app, Credentials: creds(t, fresh()), Day: "2026-03-02", Zone: "Australia/Sydney"}, rec.save)
	if err != nil {
		t.Skip("no tz data: ", err)
	}
	if len(res.Readings) != 1 || *res.Readings[0].WeightKg != 80.5 {
		t.Fatalf("day in Sydney: %+v", res.Readings)
	}
	// In UTC the same day is a plain 24 hours.
	if _, err := w.importReadings(ctx, workerapi.ImportPayload{App: app, Credentials: creds(t, fresh()), Day: "2026-03-01"}, rec.save); err != nil {
		t.Fatal(err)
	}
	last := f.meas[len(f.meas)-1]
	if last.Get("startdate") != strconv.FormatInt(ts("2026-03-01T00:00").Unix(), 10) || last.Get("enddate") != strconv.FormatInt(ts("2026-03-01T23:59").Unix()+59, 10) {
		t.Fatalf("day window: %v", last)
	}
}

func TestMeasureCallsAreRetried(t *testing.T) {
	for name, rate := range map[string]bool{"HTTP 503": false, "status 601": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			w, f, rec := setup(t)
			f.groups = []measureGroup{grp(1, "2026-03-01T07:00", 80500)}
			p := workerapi.ImportPayload{App: app, Credentials: creds(t, fresh())}
			f.flaky, f.flaky601 = 2, rate
			res, err := w.importReadings(ctx, p, rec.save)
			if err != nil || len(res.Readings) != 1 || len(f.meas) != 3 {
				t.Fatalf("after two failures: %+v %v (%d calls)", res, err, len(f.meas))
			}
			// Still failing after the last attempt: give the job a retry of its own.
			f.flaky, f.meas = maxAttempts+5, nil
			_, err = w.importReadings(ctx, p, rec.save)
			if ok, _ := runner.IsRetryable(err); !ok || len(f.meas) != maxAttempts {
				t.Fatalf("must give up after %d attempts with a retryable error: %v (%d calls)", maxAttempts, err, len(f.meas))
			}
		})
	}
}

func TestOtherFailuresAreNotRetried(t *testing.T) {
	ctx := context.Background()
	w, f, rec := setup(t)
	p := workerapi.ImportPayload{App: app, Credentials: creds(t, fresh())}
	for _, status := range []int{293, 401} {
		f.status, f.meas = status, nil
		_, err := w.importReadings(ctx, p, rec.save)
		if !runner.IsPermanent(err) || len(f.meas) != 1 || !strings.Contains(err.Error(), "status "+strconv.Itoa(status)) {
			t.Fatalf("status %d: %v after %d calls", status, err, len(f.meas))
		}
	}
}

func TestBadImportPayloads(t *testing.T) {
	ctx := context.Background()
	w, _, rec := setup(t)
	good := creds(t, fresh())
	for name, p := range map[string]workerapi.ImportPayload{
		"not configured": {Credentials: good},
		"no credentials": {App: app},
		"empty tokens":   {App: app, Credentials: json.RawMessage(`{}`)},
		"junk tokens":    {App: app, Credentials: json.RawMessage(`"x"`)},
		"unknown zone":   {App: app, Credentials: good, Zone: "Mars/Olympus"},
		"bad day":        {App: app, Credentials: good, Day: "yesterday"},
	} {
		if _, err := w.importReadings(ctx, p, rec.save); !runner.IsPermanent(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
