package mainbackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
	"github.com/trueforge-org/dexafix/pkg/store/valkeystate"
	"github.com/trueforge-org/dexafix/pkg/workers/builtin"
)

// newValkeyServer builds the combined stack with the failed-attempt counters and the nonces in a
// (fake) Valkey server instead of the database.
func newValkeyServer(t *testing.T) (*httptest.Server, *sqlstore.Store, *miniredis.Miniredis) {
	t.Helper()
	skipInSplit(t) // the state store is the backend's; the split changes nothing about it
	mr := miniredis.RunT(t)
	vs, err := valkeystate.Open(context.Background(), "redis://"+mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vs.Close)
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutSetting(context.Background(), settingSignup, valueEnabled); err != nil {
		t.Fatal(err)
	}
	srv, err := newStack(st, builtin.Options{}, WithState(vs))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	testServers.Store(ts, srv)
	return ts, st, mr
}

// With Valkey configured a flood of wrong passwords is counted there and the database sees none of
// it, and the lock-out works exactly as it does with the database.
func TestValkeyHoldsTheFailureCounters(t *testing.T) {
	ts, st, mr := newValkeyServer(t)
	signup(t, ts, noRedirectClient(), "root")
	form := url.Values{"email": {"root@example.com"}, "password": {"wrong password"}}
	var last int
	for i := 0; i < maxLoginFail+2; i++ {
		last = post(t, noRedirectClient(), ts.URL+"/login", form).StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: %d", maxLoginFail+2, last)
	}
	good := url.Values{"email": {"root@example.com"}, "password": {testPassword}}
	if r := post(t, noRedirectClient(), ts.URL+"/login", good); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("right password while blocked: %d", r.StatusCode)
	}

	var keys []string
	for _, k := range mr.Keys() {
		if strings.HasPrefix(k, "dexafix:fail:") {
			keys = append(keys, k)
		}
	}
	if len(keys) < 2 { // the address and the login name
		t.Errorf("failure counters in Valkey: %v", keys)
	}
	if n, err := st.CountFailures(context.Background(), loginKey("root@example.com"), time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("the database holds %d failures (%v): the flood must not reach it", n, err)
	}
}

// When Valkey does not answer, nobody signs in (the counters fail closed), and the page says so the
// same way as a lock-out; when it is back, they can.
func TestValkeyDownFailsClosed(t *testing.T) {
	ts, _, mr := newValkeyServer(t)
	signup(t, ts, noRedirectClient(), "root")
	good := url.Values{"email": {"root@example.com"}, "password": {testPassword}}
	if r := post(t, noRedirectClient(), ts.URL+"/login", good); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in with Valkey up: %d", r.StatusCode)
	}
	mr.Close()
	if r := post(t, noRedirectClient(), ts.URL+"/login", good); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("sign-in with Valkey down: %d, want it refused", r.StatusCode)
	}
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r := post(t, noRedirectClient(), ts.URL+"/login", good); r.StatusCode == http.StatusSeeOther {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sign-in does not come back when Valkey does")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The limiter and the nonce ledger mean the same on either backend, including what they do when it
// fails: lock out, and refuse the value.
func TestStateBackendsAgree(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	vs, err := valkeystate.Open(ctx, "redis://"+mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vs.Close)
	db, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}

	for name, b := range map[string]struct {
		st     StateStore
		break_ func()
	}{
		"database": {db, func() { db.Close() }},
		"valkey":   {vs, mr.Close},
	} {
		t.Run(name, func(t *testing.T) {
			l := newFailureLimiter(b.st)
			n := spentNonces{b.st}
			key := "login:" + name
			for i := 0; i < 3; i++ {
				if l.Blocked(ctx, key, 3) {
					t.Fatalf("blocked after %d failures, limit 3", i)
				}
				l.Fail(ctx, key)
			}
			if !l.Blocked(ctx, key, 3) {
				t.Error("not blocked at the limit")
			}
			l.Reset(ctx, key)
			if l.Blocked(ctx, key, 3) {
				t.Error("still blocked after a reset")
			}
			// An old failure is outside the window.
			if err := b.st.RecordFailure(ctx, key, time.Now().Add(-failWindow-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if l.Blocked(ctx, key, 1) {
				t.Error("a failure older than the window counted")
			}

			exp := time.Now().Add(time.Hour)
			if n.seen(ctx, name+"-nonce") {
				t.Error("an unused nonce is seen")
			}
			if !n.use(ctx, name+"-nonce", exp) {
				t.Error("first use refused")
			}
			if n.use(ctx, name+"-nonce", exp) {
				t.Error("second use allowed")
			}
			if !n.seen(ctx, name+"-nonce") {
				t.Error("a used nonce is not seen")
			}

			b.break_()
			if !l.Blocked(ctx, key, 1000) {
				t.Error("a broken state store must block (fail closed)")
			}
			if !n.seen(ctx, "another-"+name) {
				t.Error("a broken state store must refuse the value (seen)")
			}
			if n.use(ctx, "another-"+name, exp) {
				t.Error("a broken state store must refuse the value (use)")
			}
		})
	}
}

// /status shows Valkey when it is configured, and as down when it does not answer. The reason is
// for administrators only.
func TestStatusShowsValkey(t *testing.T) {
	noCache(t)
	ts, _, mr := newValkeyServer(t)
	anon := noRedirectClient()
	code, page := body(t, anon, ts.URL+"/status")
	if code != http.StatusOK || !strings.Contains(page, "Valkey") {
		t.Fatalf("/status with Valkey: %d, shows Valkey: %v", code, strings.Contains(page, "Valkey"))
	}
	mr.Close()
	code, page = body(t, anon, ts.URL+"/status")
	if code != http.StatusServiceUnavailable || !strings.Contains(page, `state-down">Down</span> <span>Valkey`) {
		t.Errorf("/status with Valkey down: %d", code)
	}
}

// Without Valkey /status has no row for it.
func TestStatusWithoutValkey(t *testing.T) {
	ts, _, _ := newAuthServer(t)
	if _, page := body(t, noRedirectClient(), ts.URL+"/status"); strings.Contains(page, "Valkey") {
		t.Error("/status mentions Valkey although none is configured")
	}
}
