package mainbackend

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/store"
	"github.com/trueforge-org/dexafix/pkg/workers/builtin"
)

func TestDeletingADexaScanRemovesItAndKeepsOthersSafe(t *testing.T) {
	ts, alice, st := newServerAndStore(t, builtin.Options{})
	signup(t, ts, alice, "alice")
	bob := noRedirectClient()
	signup(t, ts, bob, "bob")
	scan := url.Values{"measured_at": {"2026-03-02"}, "weight_kg": {"80"}, "bmd_g_cm2": {"1.25"}}
	post(t, alice, ts.URL+"/u/1/dexa", scan)
	if _, page := body(t, alice, ts.URL+"/u/1/dexa"); !strings.Contains(page, "1.25") {
		t.Fatalf("the scan is not on the page:\n%s", page)
	}
	scans, _, err := st.ListDexaScansPage(context.Background(), 1, store.ListFilter{})
	if err != nil || len(scans) != 1 {
		t.Fatalf("scans in the store: %d %v", len(scans), err)
	}
	id := scans[0].ID
	deletePath := ts.URL + "/u/1/dexa/" + strconv.FormatInt(id, 10) + "/delete"

	// Somebody else cannot delete it, and neither can a made-up id.
	if r := post(t, bob, deletePath, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("another user deleting the scan: %d, want 404", r.StatusCode)
	}
	if r := post(t, alice, ts.URL+"/u/1/dexa/"+strconv.FormatInt(id+99, 10)+"/delete", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("deleting a scan that does not exist: %d, want 404", r.StatusCode)
	}
	if r := post(t, alice, ts.URL+"/u/1/dexa/abc/delete", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("deleting scan \"abc\": %d, want 404", r.StatusCode)
	}
	if scans, _, _ := st.ListDexaScansPage(context.Background(), 1, store.ListFilter{}); len(scans) != 1 {
		t.Fatal("a refused delete removed the scan")
	}

	// The owner deletes it, and the delete is in the audit log.
	r := post(t, alice, deletePath, nil)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/u/1/dexa" {
		t.Fatalf("deleting the scan: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if _, page := body(t, alice, ts.URL+"/u/1/dexa"); strings.Contains(page, "1.25") {
		t.Fatal("the deleted scan is still on the page")
	}
	events, err := st.ListAuditEvents(context.Background(), store.AuditFilter{UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		found = found || e.Action == auditScanDeleted
	}
	if !found {
		t.Fatal("the delete is not in the audit log")
	}
}

func TestDeletingADexaScanWhenTheBodyServiceFails(t *testing.T) {
	cases := map[string]func(*testing.T, *adminEnv){
		"is down":                 func(t *testing.T, e *adminEnv) { e.useBody(bodyDown(t)) },
		"refuses the credentials": func(t *testing.T, e *adminEnv) { e.useBody(bodyAnswering(t, http.StatusForbidden, "forbidden")) },
	}
	for name, breakIt := range cases {
		t.Run("the body service "+name, func(t *testing.T) {
			e := newAdminEnv(t)
			breakIt(t, e)
			code, out := e.do(t, "POST", "/u/1/dexa/1/delete", url.Values{})
			if code != http.StatusServiceUnavailable {
				t.Fatalf("status %d %.80q, want 503", code, out)
			}
		})
	}
}

func TestRemovingAStrengthLevelLoginFromTheSettingsPage(t *testing.T) {
	fake := fakeStrengthLevel(t, map[string]map[string]any{})
	ts, root, st := newServerAndStore(t, builtin.Options{StrengthLevelBase: fake.url})
	signup(t, ts, root, "alice") // the administrator
	bob := noRedirectClient()
	signup(t, ts, bob, "bob")
	post(t, root, ts.URL+"/admin/users/2/strengthlevel", url.Values{"enabled": {"1"}})
	post(t, bob, ts.URL+"/u/2/strengthlevel/login", url.Values{"email": {"a@b.c"}, "password": {"pw"}})
	stored := func() bool { _, err := st.GetIntegration(context.Background(), 2, "strengthlevel"); return err == nil }
	if !stored() {
		t.Fatal("the login was not saved")
	}
	// Nobody but the user can remove it.
	if r := post(t, root, ts.URL+"/u/2/strengthlevel/login/delete", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("an administrator removing another person's login: %d, want 404", r.StatusCode)
	}
	if !stored() {
		t.Fatal("a refused request removed the login")
	}
	r := post(t, bob, ts.URL+"/u/2/strengthlevel/login/delete", nil)
	if loc := r.Header.Get("Location"); r.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "Strength+Level+login+removed") {
		t.Fatalf("removing the login: %d %q", r.StatusCode, loc)
	}
	if stored() {
		t.Fatal("the login is still stored")
	}
	// Removing it again changes nothing and still answers.
	if r := post(t, bob, ts.URL+"/u/2/strengthlevel/login/delete", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("removing it twice: %d", r.StatusCode)
	}
	// A database that cannot remove it is reported, and the login stays.
	post(t, bob, ts.URL+"/u/2/strengthlevel/login", url.Values{"email": {"a@b.c"}, "password": {"pw"}})
	breakTable(t, st, "user_integrations")
	if r := post(t, bob, ts.URL+"/u/2/strengthlevel/login/delete", nil); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("removing the login without the database: %d, want 503", r.StatusCode)
	}
}
