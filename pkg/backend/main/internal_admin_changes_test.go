package mainbackend

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// adminFlash reads the message an admin change leaves on the Admin page (the redirect's msg).
func adminFlash(t *testing.T, location string) string {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil || (u.Path != "/admin" && u.Path != "/admin/workers") {
		t.Fatalf("not a redirect to an Admin page: %q", location)
	}
	return u.Query().Get("msg")
}

type adminChange struct {
	name       string
	setup      func(*testing.T, *adminEnv)
	path       string
	form       url.Values
	wantFlash  string // the message shown, or "" when the change must fail
	wantStatus int    // the failure's status when wantFlash is empty (default 503)
}

func runAdminChanges(t *testing.T, scenarios []adminChange) {
	t.Helper()
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			e := newAdminEnv(t)
			if sc.setup != nil {
				sc.setup(t, e)
			}
			code, out := e.do(t, "POST", sc.path, sc.form)
			if sc.wantFlash == "" {
				want := sc.wantStatus
				if want == 0 {
					want = http.StatusServiceUnavailable
				}
				if code != want {
					t.Fatalf("POST %s: %d %.80q, want %d", sc.path, code, out, want)
				}
				return
			}
			if code != http.StatusSeeOther {
				t.Fatalf("POST %s: %d %.80q, want a redirect with %q", sc.path, code, out, sc.wantFlash)
			}
			if got := adminFlash(t, out); !strings.Contains(got, sc.wantFlash) {
				t.Fatalf("POST %s: message %q, want it to contain %q", sc.path, got, sc.wantFlash)
			}
		})
	}
}

func enableSL(t *testing.T, e *adminEnv) {
	t.Helper()
	if err := e.st.SetUserStrengthLevelAuto(context.Background(), 2, true); err != nil {
		t.Fatal(err)
	}
}

func (f *flakyStore) DeleteTOTP(ctx context.Context, userID int64) error {
	if f.fails("DeleteTOTP") {
		return errFlaky
	}
	return f.Store.DeleteTOTP(ctx, userID)
}

func formWith(base url.Values, k, v string) url.Values {
	c := url.Values{}
	for kk, vv := range base {
		c[kk] = vv
	}
	c.Set(k, v)
	return c
}

func TestAdminUserChangesTellTheAdministratorWhatHappenedOrFailCleanly(t *testing.T) {
	refuseBob := func(table, col string) func(*testing.T, *adminEnv) {
		return func(t *testing.T, e *adminEnv) { refuseWrites(t, e.st, table, col, "2") }
	}
	good := url.Values{"name": {"carol"}, "email": {"carol@example.com"}, "password": {testPassword}}
	runAdminChanges(t, []adminChange{
		{name: "adding a user with a bad e-mail address", path: "/admin/users", form: formWith(good, "email", "nope"), wantFlash: "valid e-mail"},
		{name: "adding a user with a short password", path: "/admin/users", form: formWith(good, "password", "x"), wantFlash: "assword"},
		{name: "adding a user without a name", path: "/admin/users", form: formWith(good, "name", ""), wantFlash: "1-64 characters"},
		{name: "adding a user when the identities table is gone", setup: func(t *testing.T, e *adminEnv) { breakTable(t, e.st, "user_identities") }, path: "/admin/users", form: good},
		{name: "opening sign-up when the setting cannot be saved", setup: func(t *testing.T, e *adminEnv) { e.flaky.failOn("PutSetting:" + settingSignup) }, path: "/admin/signup", form: url.Values{"enabled": {"1"}}},
		{name: "closing sign-up when the setting cannot be saved", setup: func(t *testing.T, e *adminEnv) { e.flaky.failOn("DeleteSetting:" + settingSignup) }, path: "/admin/signup", form: url.Values{"enabled": {"0"}}},
		{name: "opening sign-up", path: "/admin/signup", form: url.Values{"enabled": {"1"}}, wantFlash: "New accounts are open"},
		{name: "making somebody an administrator when the write is refused", setup: refuseBob("users", "id"), path: "/admin/users/2/role", form: url.Values{"role": {"admin"}}},
		{name: "enabling Strength Level when the write is refused", setup: refuseBob("users", "id"), path: "/admin/users/2/strengthlevel", form: url.Values{"enabled": {"1"}}},
		{name: "disabling Strength Level", setup: enableSL, path: "/admin/users/2/strengthlevel", form: url.Values{"enabled": {"0"}}, wantFlash: "Strength Level sync is off for bob"},
		{name: "the multi-day sync before Strength Level is enabled", path: "/admin/users/2/strengthlevel-bulk", form: url.Values{"enabled": {"1"}}, wantFlash: "first"},
		{name: "enabling the multi-day sync", setup: enableSL, path: "/admin/users/2/strengthlevel-bulk", form: url.Values{"enabled": {"1"}}, wantFlash: "may now run"},
		{name: "disabling the multi-day sync", setup: enableSL, path: "/admin/users/2/strengthlevel-bulk", form: url.Values{"enabled": {"0"}}, wantFlash: "multi-day Strength Level sync is off"},
		{name: "the multi-day sync when the write is refused", setup: func(t *testing.T, e *adminEnv) { enableSL(t, e); refuseWrites(t, e.st, "users", "id", "2") }, path: "/admin/users/2/strengthlevel-bulk", form: url.Values{"enabled": {"1"}}},
		{name: "a short password for a user", path: "/admin/users/2/password", form: url.Values{"password": {"x"}}, wantFlash: "assword"},
		{name: "a new password when the sessions cannot be ended", setup: func(t *testing.T, e *adminEnv) { e.flaky.failOn("DeleteUserSessions") }, path: "/admin/users/2/password", form: url.Values{"password": {"another password"}}},
		{name: "a new password when the identity cannot be written", setup: refuseBob("user_identities", "user_id"), path: "/admin/users/2/password", form: url.Values{"password": {"another password"}}},
		{name: "a new password when the access tokens cannot be removed", setup: func(t *testing.T, e *adminEnv) { breakTable(t, e.st, "access_tokens") }, path: "/admin/users/2/password", form: url.Values{"password": {"another password"}}},
		{name: "deleting a user when the delete is refused", setup: refuseBob("users", "id"), path: "/admin/users/2/delete", form: url.Values{}},
		{name: "deleting a user whose sessions cannot be ended afterwards", setup: func(t *testing.T, e *adminEnv) { e.flaky.failOn("DeleteUserSessions") }, path: "/admin/users/2/delete", form: url.Values{}, wantFlash: "bob and their data were deleted"},
		{name: "removing two-step verification when the database cannot", setup: func(t *testing.T, e *adminEnv) { e.flaky.failOn("DeleteTOTP") }, path: "/admin/users/2/2fa/delete", form: url.Values{}},
	})
}
