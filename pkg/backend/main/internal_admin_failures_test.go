package mainbackend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
	"github.com/trueforge-org/dexafix/pkg/workers/builtin"
)

// flakyStore is a store that fails the calls a test asks it to fail and passes everything else to
// the real one. A call is named by its method ("ListUsers"), and a setting call by the method and
// the setting ("GetSetting:base_url"), so a test breaks one step of a handler and nothing around it.
type flakyStore struct {
	store.Store
	mu      sync.Mutex
	failing map[string]bool
	allowed map[string]int
	calls   map[string]int
}

var errFlaky = errors.New("the database let us down")

func (f *flakyStore) Unwrap() store.Store { return f.Store }

func (f *flakyStore) failOn(calls ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing == nil {
		f.failing = map[string]bool{}
	}
	for _, c := range calls {
		f.failing[c] = true
	}
}

// heal makes every call work again.
func (f *flakyStore) heal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing, f.allowed = nil, nil
}

// failAfter lets a call succeed n times and fails it from then on.
func (f *flakyStore) failAfter(call string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.allowed == nil {
		f.allowed = map[string]int{}
	}
	f.allowed[call] = n
}

// called says how many times a store call that can be made to fail was made.
func (f *flakyStore) called(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[call]
}

func (f *flakyStore) fails(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[call]++
	if left, ok := f.allowed[call]; ok {
		if left == 0 {
			return true
		}
		f.allowed[call] = left - 1
	}
	return f.failing[call]
}

func (f *flakyStore) GetSetting(ctx context.Context, name string) (string, error) {
	if f.fails("GetSetting:" + name) {
		return "", errFlaky
	}
	return f.Store.GetSetting(ctx, name)
}

func (f *flakyStore) PutSetting(ctx context.Context, name, value string) error {
	if f.fails("PutSetting:" + name) {
		return errFlaky
	}
	return f.Store.PutSetting(ctx, name, value)
}

func (f *flakyStore) DeleteSetting(ctx context.Context, name string) error {
	if f.fails("DeleteSetting:" + name) {
		return errFlaky
	}
	return f.Store.DeleteSetting(ctx, name)
}

func (f *flakyStore) ListUsers(ctx context.Context) ([]models.User, error) {
	if f.fails("ListUsers") {
		return nil, errFlaky
	}
	users, err := f.Store.ListUsers(ctx)
	if f.fails("ListUsers:without-bob") { // bob was deleted a moment after the list was read
		users = slices.DeleteFunc(users, func(u models.User) bool { return u.Name == "bob" })
	}
	return users, err
}

func (f *flakyStore) TwoFactorUsers(ctx context.Context) (map[int64]bool, error) {
	if f.fails("TwoFactorUsers") {
		return nil, errFlaky
	}
	return f.Store.TwoFactorUsers(ctx)
}

func (f *flakyStore) DeleteUserSessions(ctx context.Context, userID int64) error {
	if f.fails("DeleteUserSessions") {
		return errFlaky
	}
	return f.Store.DeleteUserSessions(ctx, userID)
}

func (f *flakyStore) SettingsGeneration(ctx context.Context) (int64, error) {
	if f.fails("SettingsGeneration") {
		return 0, errFlaky
	}
	return f.Store.SettingsGeneration(ctx)
}

// adminEnv is a whole stack with an administrator (user 1, "root") and an ordinary user (user 2,
// "bob") who both have a session, over a store whose calls a test can make fail.
type adminEnv struct {
	ts          *httptest.Server
	root, bob   *http.Client
	st          *sqlstore.Store
	flaky       *flakyStore
	backend     *Backend
	rootSession string
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutSetting(context.Background(), settingSignup, valueEnabled); err != nil {
		t.Fatal(err)
	}
	flaky := &flakyStore{Store: st}
	var ts *httptest.Server
	if splitTests() {
		ts = newSplitServer(t, flaky, builtin.Options{})
	} else {
		srv, err := newStack(flaky, builtin.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Close)
		ts = httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		testServers.Store(ts, srv)
	}
	env := &adminEnv{ts: ts, root: noRedirectClient(), bob: noRedirectClient(), st: st, flaky: flaky, backend: serverOf(ts).Backend}
	signup(t, ts, env.root, "root")
	signup(t, ts, env.bob, "bob")
	env.rootSession = env.root.Transport.(cookieTransport).cookie.Value
	return env
}

// do sends a form as the administrator and returns the status and the body.
func (e *adminEnv) do(t *testing.T, method, path string, form url.Values) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.root.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "" {
		return resp.StatusCode, loc
	}
	return resp.StatusCode, readBody(resp)
}

// refuseWrites makes every insert, update and delete of the rows of table where col is val fail, as
// if the database refused that one write.
func refuseWrites(t *testing.T, st *sqlstore.Store, table, col, val string) {
	t.Helper()
	for _, c := range []struct{ event, row string }{{"INSERT", "NEW"}, {"UPDATE", "NEW"}, {"DELETE", "OLD"}} {
		q := "CREATE TRIGGER refuse_" + strings.ToLower(c.event) + "_" + table + " BEFORE " + c.event + " ON " + table +
			" WHEN CAST(" + c.row + "." + col + " AS TEXT) = '" + val + "' BEGIN SELECT RAISE(ABORT, 'refused'); END"
		if err := st.ExecForTests(context.Background(), q); err != nil {
			t.Fatalf("refuse %s on %s: %v", c.event, table, err)
		}
	}
}

// An administrator's change that the database refuses is an internal error on the page, not a
// half-saved change that looks done.
func TestAdminChangesFailCleanlyWhenTheStoreLetsThemDown(t *testing.T) {
	const unavailable = "temporarily unavailable" // a failure of the backend reaches the browser as a 503
	scenarios := []struct {
		name   string
		break_ func(*testing.T, *adminEnv)
		method string
		path   string
		form   url.Values
	}{
		{"the page when the access tokens table is gone", func(t *testing.T, e *adminEnv) { breakTable(t, e.st, "access_tokens") }, "GET", "/admin", nil},
		{"the page when the user list fails", func(t *testing.T, e *adminEnv) { e.flaky.failOn("ListUsers") }, "GET", "/admin", nil},
		{"the page when the passkeys table is gone", func(t *testing.T, e *adminEnv) { breakTable(t, e.st, "passkeys") }, "GET", "/admin", nil},
		{"the page when the providers table is gone", func(t *testing.T, e *adminEnv) { breakTable(t, e.st, "auth_providers") }, "GET", "/admin", nil},
		{"the page when the sign-up setting cannot be read", func(t *testing.T, e *adminEnv) { e.flaky.failOn("GetSetting:" + settingSignup) }, "GET", "/admin", nil},
		{"the page when the base URL cannot be read", func(t *testing.T, e *adminEnv) { e.flaky.failOn("GetSetting:" + settingBaseURL) }, "GET", "/admin", nil},
		{"the page when the e-mail settings cannot be read", func(t *testing.T, e *adminEnv) { e.flaky.failOn("GetSetting:" + settingSMTPHost) }, "GET", "/admin", nil},
		{"the page when the TLS names cannot be read", func(t *testing.T, e *adminEnv) { e.flaky.failOn("GetSetting:" + settingTLSNames) }, "GET", "/admin", nil},
		{"the page when the two-factor list fails", func(t *testing.T, e *adminEnv) { e.flaky.failOn("TwoFactorUsers") }, "GET", "/admin", nil},
		{"the page when the audit log table is gone", func(t *testing.T, e *adminEnv) { breakTable(t, e.st, "audit_log") }, "GET", "/admin", nil},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			e := newAdminEnv(t)
			sc.break_(t, e)
			code, out := e.do(t, sc.method, sc.path, sc.form)
			if code != http.StatusServiceUnavailable || !strings.Contains(out, unavailable) {
				t.Fatalf("%s %s: %d %.80q, want 503 %q", sc.method, sc.path, code, out, unavailable)
			}
		})
	}
}

func TestAdminPageStillWorksWhenTheReadingsTableIsGone(t *testing.T) {
	e := newAdminEnv(t)
	breakTable(t, e.st, "scale_readings")
	code, out := e.do(t, "GET", "/admin", nil)
	if code != http.StatusOK || !strings.Contains(out, "Unavailable") {
		t.Fatalf("page: %d, want 200 with the counts shown as Unavailable", code)
	}
}
