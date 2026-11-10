package bodybackend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// faultyStore is the real store, except that the methods named in errs fail with the given error.
type faultyStore struct {
	store.BodyStore

	mu           sync.Mutex
	errs         map[string]error
	fixedVersion bool // BodyVersion answers 0 without looking at the request
}

func (f *faultyStore) failWith(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[method] = err
}

func (f *faultyStore) err(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.errs[method]
}

func (f *faultyStore) CreateDexaScan(ctx context.Context, s *models.DexaScan) error {
	if err := f.err("CreateDexaScan"); err != nil {
		return err
	}
	return f.BodyStore.CreateDexaScan(ctx, s)
}

func (f *faultyStore) ListDexaScans(ctx context.Context, userID int64) ([]models.DexaScan, error) {
	if err := f.err("ListDexaScans"); err != nil {
		return nil, err
	}
	return f.BodyStore.ListDexaScans(ctx, userID)
}

func (f *faultyStore) ListDexaScansPage(ctx context.Context, userID int64, fl store.ListFilter) ([]models.DexaScan, *store.Cursor, error) {
	if err := f.err("ListDexaScansPage"); err != nil {
		return nil, nil, err
	}
	return f.BodyStore.ListDexaScansPage(ctx, userID, fl)
}

func (f *faultyStore) DeleteDexaScan(ctx context.Context, userID, id int64) error {
	if err := f.err("DeleteDexaScan"); err != nil {
		return err
	}
	return f.BodyStore.DeleteDexaScan(ctx, userID, id)
}

func (f *faultyStore) UpsertScaleReadings(ctx context.Context, userID int64, rs []models.ScaleReading) (int, error) {
	if err := f.err("UpsertScaleReadings"); err != nil {
		return 0, err
	}
	return f.BodyStore.UpsertScaleReadings(ctx, userID, rs)
}

func (f *faultyStore) ListScaleReadings(ctx context.Context, userID int64) ([]models.ScaleReading, error) {
	if err := f.err("ListScaleReadings"); err != nil {
		return nil, err
	}
	return f.BodyStore.ListScaleReadings(ctx, userID)
}

func (f *faultyStore) ListScaleReadingsPage(ctx context.Context, userID int64, fl store.ListFilter) ([]models.ScaleReading, *store.Cursor, error) {
	if err := f.err("ListScaleReadingsPage"); err != nil {
		return nil, nil, err
	}
	return f.BodyStore.ListScaleReadingsPage(ctx, userID, fl)
}

func (f *faultyStore) DeleteScaleReading(ctx context.Context, userID, id int64) error {
	if err := f.err("DeleteScaleReading"); err != nil {
		return err
	}
	return f.BodyStore.DeleteScaleReading(ctx, userID, id)
}

func (f *faultyStore) BodyVersion(ctx context.Context, userID int64) (int64, error) {
	if err := f.err("BodyVersion"); err != nil {
		return 0, err
	}
	if f.fixedVersion {
		return 0, nil
	}
	return f.BodyStore.BodyVersion(ctx, userID)
}

func (f *faultyStore) DeleteUserBodyData(ctx context.Context, userID int64) error {
	if err := f.err("DeleteUserBodyData"); err != nil {
		return err
	}
	return f.BodyStore.DeleteUserBodyData(ctx, userID)
}

func (f *faultyStore) BodyUserIDs(ctx context.Context) ([]int64, error) {
	if err := f.err("BodyUserIDs"); err != nil {
		return nil, err
	}
	return f.BodyStore.BodyUserIDs(ctx)
}

func (f *faultyStore) BodyStats(ctx context.Context) (store.BodyStats, error) {
	if err := f.err("BodyStats"); err != nil {
		return store.BodyStats{}, err
	}
	return f.BodyStore.BodyStats(ctx)
}

// newFaultRig is newRig over a store whose methods can be made to fail. r.st is the real store, for
// preparing data.
func newFaultRig(t *testing.T, opts ...Option) (*rig, *faultyStore) {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fs := &faultyStore{BodyStore: st, errs: map[string]error{}}
	_, pub, priv := bodyapi.GenerateKey()
	svc, err := New(fs, testSecret, bodyapi.NewVerifier(pub), opts...)
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
	u, err := st.CreateUser(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	r.users = append(r.users, u)
	return r, fs
}

// answer is what a call got back.
type answer struct {
	status int
	code   string
	msg    string
	raw    string
}

// send makes a call as alice with a token of the given scope; withParams false leaves the user's
// settings out of the token.
func (r *rig) send(t *testing.T, method, path string, scope bodyapi.Scope, withParams bool, body string) answer {
	t.Helper()
	tok := r.token(r.users[0].ID, scope)
	if !withParams {
		tok = r.signer.Sign(r.users[0].ID, scope, nil, time.Now())
	}
	req, err := http.NewRequest(method, r.ts.URL+bodyapi.Prefix+"/users/"+itoa(r.users[0].ID)+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(bodyapi.HeaderSecret, testSecret)
	req.Header.Set(bodyapi.HeaderToken, tok)
	resp, err := r.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	a := answer{status: resp.StatusCode, raw: string(raw)}
	var e bodyapi.Error
	if json.Unmarshal(raw, &e) == nil {
		a.code, a.msg = e.Code, e.Message
	}
	return a
}

func TestMalformedCallsAreRefusedWithAMessageThatSaysWhy(t *testing.T) {
	r, _ := newFaultRig(t)
	for _, tc := range []struct {
		name       string
		method     string
		path       string
		scope      bodyapi.Scope
		noSettings bool
		body       string
		status     int
		contains   string
	}{
		{"a scan that is not JSON", "POST", "/scans", bodyapi.ScopeWrite, false, "{", 400, "invalid request"},
		{"readings that are not JSON", "POST", "/readings", bodyapi.ScopeWrite, false, "x", 400, "invalid request"},
		{"an import that is not JSON", "POST", "/readings/reconcile", bodyapi.ScopeWrite, false, "x", 400, "invalid request"},
		{"a scan to delete that is not a number", "DELETE", "/scans/abc", bodyapi.ScopeWrite, false, "", 404, "not found"},
		{"a reading to delete that is not a number", "DELETE", "/readings/abc", bodyapi.ScopeWrite, false, "", 404, "not found"},
		{"a list of scans with a bad limit", "GET", "/scans?limit=many", bodyapi.ScopeRead, false, "", 400, "invalid limit"},
		{"a list of readings with a negative limit", "GET", "/readings?limit=-1", bodyapi.ScopeRead, false, "", 400, "invalid limit"},
		{"a list of readings with a bad page", "GET", "/readings?before=junk", bodyapi.ScopeRead, false, "", 400, "invalid page"},
		{"a list of readings with a bad since", "GET", "/readings?since=yesterday", bodyapi.ScopeRead, false, "", 400, "invalid since"},
		{"a list of readings with a bad until", "GET", "/readings?until=tomorrow", bodyapi.ScopeRead, false, "", 400, "invalid until"},
		{"an import with no settings in the token", "POST", "/readings/reconcile", bodyapi.ScopeWrite, true, "{}", 400, "no user settings"},
		{"estimates with no settings in the token", "GET", "/estimates", bodyapi.ScopeRead, true, "", 400, "no user settings"},
		{"estimates for a window of no days", "GET", "/estimates?days=0", bodyapi.ScopeRead, false, "", 400, "days must be"},
		{"an import without a source", "POST", "/readings/reconcile", bodyapi.ScopeWrite, false, `{"fetched":[]}`, 400, "invalid import"},
		{"a day import with an impossible day", "POST", "/readings/reconcile", bodyapi.ScopeWrite, false, `{"source":"demo","day_mode":true,"day":"2026-13-45"}`, 400, "invalid import"},
		{"an import with an out of range reading", "POST", "/readings/reconcile", bodyapi.ScopeWrite, false,
			`{"source":"demo","fetched":[{"source":"demo","external_id":"g1","measured_at":"2026-01-01T08:00:00Z","weight_kg":5000}]}`, 400, "item 0: weight_kg must be between"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := r.send(t, tc.method, tc.path, tc.scope, !tc.noSettings, tc.body)
			if a.status != tc.status || !strings.Contains(a.raw, tc.contains) {
				t.Fatalf("status %d body %s, want %d with %q", a.status, a.raw, tc.status, tc.contains)
			}
		})
	}
}

func TestAFailingStoreIsAnsweredAsAnInternalErrorWithoutLeakingIt(t *testing.T) {
	boom := errors.New("disk on fire")
	for _, tc := range []struct {
		name   string
		fail   string
		method string
		path   string
		scope  bodyapi.Scope
		body   string
	}{
		{"listing scans", "ListDexaScansPage", "GET", "/scans", bodyapi.ScopeRead, ""},
		{"creating a scan", "CreateDexaScan", "POST", "/scans", bodyapi.ScopeWrite, `{"measured_at":"2026-01-01T08:00:00Z"}`},
		{"deleting a scan", "DeleteDexaScan", "DELETE", "/scans/1", bodyapi.ScopeWrite, ""},
		{"listing readings", "ListScaleReadingsPage", "GET", "/readings", bodyapi.ScopeRead, ""},
		{"storing readings", "UpsertScaleReadings", "POST", "/readings", bodyapi.ScopeWrite, "[]"},
		{"deleting a reading", "DeleteScaleReading", "DELETE", "/readings/1", bodyapi.ScopeWrite, ""},
		{"an import reading what is stored", "ListScaleReadings", "POST", "/readings/reconcile", bodyapi.ScopeWrite, `{"source":"demo"}`},
		{"estimates reading the data version", "BodyVersion", "GET", "/estimates", bodyapi.ScopeRead, ""},
		{"estimates reading the scans", "ListDexaScans", "GET", "/estimates", bodyapi.ScopeRead, ""},
		{"estimates reading the readings", "ListScaleReadings", "GET", "/estimates", bodyapi.ScopeRead, ""},
		{"an export reading the scans", "ListDexaScans", "GET", "/export", bodyapi.ScopeRead, ""},
		{"an export reading the readings", "ListScaleReadings", "GET", "/export", bodyapi.ScopeRead, ""},
		{"deleting a user's data", "DeleteUserBodyData", "DELETE", "", bodyapi.ScopeAdmin, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fs := newFaultRig(t)
			fs.failWith(tc.fail, boom)
			a := r.send(t, tc.method, tc.path, tc.scope, true, tc.body)
			if a.status != http.StatusInternalServerError || a.code != bodyapi.CodeInternal || a.msg != "internal error" || strings.Contains(a.raw, "disk") {
				t.Fatalf("got %+v", a)
			}
		})
	}
	t.Run("the totals", func(t *testing.T) {
		r, fs := newFaultRig(t)
		fs.failWith("BodyStats", boom)
		if _, err := r.client.Stats(context.Background()); err == nil || strings.Contains(err.Error(), "disk") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("the list of users", func(t *testing.T) {
		r, fs := newFaultRig(t)
		fs.failWith("BodyUserIDs", boom)
		if _, err := r.client.UserIDs(context.Background()); err == nil || strings.Contains(err.Error(), "disk") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestStoreErrorsThatAreNotFaultsKeepTheirMeaning(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{"nothing there", store.ErrNotFound, 404, bodyapi.CodeNotFound, "not found"},
		{"a value the store refuses", store.ErrInvalid, 400, bodyapi.CodeInvalid, store.ErrInvalid.Error()},
		{"a cancelled request", context.Canceled, 503, bodyapi.CodeUnavailable, "the request was cancelled"},
		{"a request that ran out of time", context.DeadlineExceeded, 503, bodyapi.CodeUnavailable, "the request was cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fs := newFaultRig(t)
			fs.failWith("DeleteScaleReading", tc.err)
			a := r.send(t, "DELETE", "/readings/1", bodyapi.ScopeWrite, true, "")
			if a.status != tc.status || a.code != tc.code || a.msg != tc.message {
				t.Fatalf("got %+v", a)
			}
		})
	}
}

func TestAnImportThatCannotStoreOrDeleteFailsAndSaysSo(t *testing.T) {
	ext := "g1"
	seed := func(t *testing.T, r *rig, fs *faultyStore) {
		t.Helper()
		own := models.ScaleReading{MeasuredAt: day(0), WeightKg: fl(80), Source: "demo", Method: models.MethodScale, ExternalID: &ext}
		if _, err := fs.BodyStore.UpsertScaleReadings(context.Background(), r.users[0].ID, []models.ScaleReading{own}); err != nil {
			t.Fatal(err)
		}
	}
	day0 := `"day_mode":true,"day":"2026-01-01"`
	newReading := `{"source":"demo","external_id":"g2","method":"scale","measured_at":"2026-01-01T09:00:00Z","weight_kg":81}`
	t.Run("a day without readings cannot be filled", func(t *testing.T) {
		r, fs := newFaultRig(t)
		fs.failWith("UpsertScaleReadings", errors.New("disk on fire"))
		a := r.send(t, "POST", "/readings/reconcile", bodyapi.ScopeWrite, true, `{"source":"demo","fetched":[`+newReading+`]}`)
		if a.status != 500 {
			t.Fatalf("got %+v", a)
		}
	})
	t.Run("a day with readings cannot be updated", func(t *testing.T) {
		r, fs := newFaultRig(t)
		seed(t, r, fs)
		fs.failWith("UpsertScaleReadings", errors.New("disk on fire"))
		a := r.send(t, "POST", "/readings/reconcile", bodyapi.ScopeWrite, true, `{"source":"demo",`+day0+`,"seen":["g1","g2"],"fetched":[`+newReading+`]}`)
		if a.status != 500 {
			t.Fatalf("got %+v", a)
		}
	})
	t.Run("a reading the source dropped cannot be deleted", func(t *testing.T) {
		r, fs := newFaultRig(t)
		seed(t, r, fs)
		fs.failWith("DeleteScaleReading", errors.New("disk on fire"))
		a := r.send(t, "POST", "/readings/reconcile", bodyapi.ScopeWrite, true, `{"source":"demo",`+day0+`,"fetched":[]}`)
		if a.status != 500 {
			t.Fatalf("got %+v", a)
		}
	})
	t.Run("a reading that is already gone is not a failure", func(t *testing.T) {
		r, fs := newFaultRig(t)
		seed(t, r, fs)
		fs.failWith("DeleteScaleReading", store.ErrNotFound)
		a := r.send(t, "POST", "/readings/reconcile", bodyapi.ScopeWrite, true, `{"source":"demo",`+day0+`,"fetched":[]}`)
		if a.status != 200 || !strings.Contains(a.raw, `"removed":1`) {
			t.Fatalf("got %+v", a)
		}
	})
}

func TestACallThatGivesUpWaitingForASlotSaysTheServiceIsBusy(t *testing.T) {
	r, fs := newFaultRig(t)
	fs.fixedVersion = true // so that the request reaches the slot without touching the store
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	uid := itoa(r.users[0].ID)
	serve := func(method, path string, scope bodyapi.Scope, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, bodyapi.Prefix+"/users/"+uid+path, strings.NewReader(body)).WithContext(cancelled)
		req.Header.Set(bodyapi.HeaderSecret, testSecret)
		req.Header.Set(bodyapi.HeaderToken, r.token(r.users[0].ID, scope))
		rec := httptest.NewRecorder()
		r.svc.Handler().ServeHTTP(rec, req)
		return rec
	}
	for range cap(r.svc.bulkSlots) {
		r.svc.bulkSlots <- struct{}{}
	}
	for range cap(r.svc.estimateSlots) {
		r.svc.estimateSlots <- struct{}{}
	}
	for name, tc := range map[string]struct {
		method, path string
		scope        bodyapi.Scope
		body         string
		message      string
	}{
		"storing readings": {"POST", "/readings", bodyapi.ScopeWrite, "[]", "the body service is busy"},
		"an import":        {"POST", "/readings/reconcile", bodyapi.ScopeWrite, `{"source":"demo"}`, "the body service is busy"},
		"an export":        {"GET", "/export", bodyapi.ScopeRead, "", "the body service is busy"},
		"estimates":        {"GET", "/estimates", bodyapi.ScopeRead, "", "the request was cancelled"},
	} {
		rec := serve(tc.method, tc.path, tc.scope, tc.body)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), tc.message) {
			t.Errorf("%s: status %d body %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestARotatedSecretIsAcceptedBesideTheCurrentOne(t *testing.T) {
	r, _ := newFaultRig(t, WithSpareSecrets("old-secret", "older-secret"))
	path := "/users/" + itoa(r.users[0].ID) + "/readings"
	for secret, want := range map[string]int{testSecret: 200, "old-secret": 200, "older-secret": 200, "never-issued": 401, "": 401} {
		h := map[string]string{bodyapi.HeaderSecret: secret, bodyapi.HeaderToken: r.token(r.users[0].ID, bodyapi.ScopeRead)}
		if resp := r.do(t, "GET", path, h, ""); resp.StatusCode != want {
			t.Errorf("secret %q: status %d, want %d", secret, resp.StatusCode, want)
		}
	}
}

func TestAServiceNeedsAStoreASecretAndAVerifier(t *testing.T) {
	_, fs := newFaultRig(t)
	_, pub, _ := bodyapi.GenerateKey()
	v := bodyapi.NewVerifier(pub)
	for name, build := range map[string]func() (*Service, error){
		"no store":    func() (*Service, error) { return New(nil, testSecret, v) },
		"no secret":   func() (*Service, error) { return New(fs, "", v) },
		"no verifier": func() (*Service, error) { return New(fs, testSecret, nil) },
	} {
		if svc, err := build(); err == nil || svc != nil {
			t.Errorf("%s: got %v, %v", name, svc, err)
		}
	}
	if _, _, err := NewInProcess(nil); err == nil {
		t.Error("an in-process service without a store must be refused")
	}
}

func TestAnInProcessServiceAnswersItsClientAndTakesTheProgramVersion(t *testing.T) {
	_, fs := newFaultRig(t)
	c, svc, err := NewInProcess(fs, WithVersion("one"))
	if err != nil {
		t.Fatal(err)
	}
	svc.SetVersion("two")
	if svc.version != "two" {
		t.Fatalf("version = %q", svc.version)
	}
	if _, err := c.Stats(context.Background()); err != nil {
		t.Fatalf("the in-process client must reach its service: %v", err)
	}
}

func TestABatchOfTooManyReadingsIsRefusedBeforeAnythingIsChecked(t *testing.T) {
	r, _ := newFaultRig(t)
	body := "[" + strings.Repeat("{},", maxBatch) + "{}]"
	a := r.send(t, "POST", "/readings", bodyapi.ScopeWrite, true, body)
	if a.status != 400 || a.msg != "batch too large" {
		t.Fatalf("got %d %s", a.status, a.msg)
	}
}

func TestSeveralReadingsADayImportRemovesAreDeletedInIdOrder(t *testing.T) {
	a, b := "a", "b"
	existing := []models.ScaleReading{
		{ID: 9, MeasuredAt: rcTs("2026-03-01T07:00"), Source: rcSource, ExternalID: &a},
		{ID: 3, MeasuredAt: rcTs("2026-03-01T08:00"), Source: rcSource, ExternalID: &b},
	}
	got := plan(rcSource, existing, nil, rcSeen(), true, rcTs("2026-03-01T00:00"), time.UTC)
	if len(got.Delete) != 2 || got.Delete[0] != 3 || got.Delete[1] != 9 || got.Removed != 2 {
		t.Fatalf("delete %v removed %d", got.Delete, got.Removed)
	}
}
