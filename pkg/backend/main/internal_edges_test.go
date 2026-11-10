package mainbackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/store"
	"github.com/trueforge-org/dexafix/pkg/workers/builtin"
)

func TestTheInternalAPIAnswersItsOwnQuestions(t *testing.T) {
	r := newJobRig(t)
	if got := r.raw("GET", "/healthz", ""); got != http.StatusOK {
		t.Errorf("healthz: %d, want 200", got)
	}
	if got := r.raw("GET", "/no-such-endpoint", ""); got != http.StatusNotFound {
		t.Errorf("an endpoint that does not exist: %d, want 404", got)
	}
	// Somebody who is not signed in is sent to sign in first, not shown another person's data.
	if _, err := r.api.Import(context.Background(), 2, "withings", ""); !apiclient.IsCode(err, apiclient.CodeUnauthorized) {
		t.Errorf("a request without a session: %v, want unauthorized", err)
	}
}

func TestTheStoresRefusalsReachThePersonAsTheyAre(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrExists, http.StatusConflict, apiclient.CodeExists},
		{store.ErrInvalid, http.StatusBadRequest, apiclient.CodeInvalid},
		{store.ErrNotFound, http.StatusNotFound, apiclient.CodeNotFound},
	} {
		rec := httptest.NewRecorder()
		internalError(rec, c.err)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.code) {
			t.Errorf("%v: status %d body %q, want %d with %q", c.err, rec.Code, rec.Body.String(), c.status, c.code)
		}
	}
}

func TestAPageWithoutAWithingsProviderShowsNoWithingsCard(t *testing.T) {
	var b Backend // no providers at all
	if v := b.withingsView(httptest.NewRequest("GET", "/", nil), models.User{ID: 1}); v != nil {
		t.Fatalf("a Withings card for a backend without the provider: %+v", v)
	}
}

func TestAProviderWithoutASignInNeedsNoConnectionToImport(t *testing.T) {
	r := newJobRig(t)
	d := &demoProvider{}
	p := d.provider()
	if status, msg := r.b.importBlocker(httptest.NewRequest("POST", "/", nil), &p, 2); status != 0 || msg != "" {
		t.Fatalf("blocked: %d %q", status, msg)
	}
}

func TestIntegrationEndpointsRefuseWhatTheFrontendWouldNeverSend(t *testing.T) {
	r := newJobRig(t)
	r.allowStrengthLevel(true)
	for _, path := range []string{
		"/users/2/import/withings", "/integrations/withings/callback",
		"/users/2/strengthlevel/login", "/users/2/strengthlevel/sync", "/users/2/strengthlevel/day",
	} {
		if got := r.raw("POST", path, "{nope"); got != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", path, got)
		}
	}
}

func TestImportThroughThePublicAPIKeepsTheSinceAndShowsTheResult(t *testing.T) {
	r := newJobRig(t)
	r.connectWithings()
	r.workerFor(succeeds(`{"readings":[]}`), kindWithingsImport)
	if s, b, _ := r.call("POST", "/import/withings?wait=5&since=2026-03-01T00:00:00Z", ""); s != 200 {
		t.Fatalf("import: %d %.200q", s, b)
	}
	_, list, _ := r.call("GET", "/jobs", "")
	id := list[strings.Index(list, `"id":"`)+6:]
	id = id[:strings.Index(id, `"`)]
	if s, b, _ := r.call("GET", "/jobs/"+id, ""); s != 200 || !strings.Contains(b, `"result":`) {
		t.Fatalf("the finished job shows no result: %d %.300q", s, b)
	}
}

func TestTheSettingsPageStillWorksWhenTheSyncStatusCannotBeRead(t *testing.T) {
	ts, alice, st := newServerAndStore(t, builtin.Options{})
	signup(t, ts, alice, "alice") // the administrator
	bob := noRedirectClient()
	signup(t, ts, bob, "bob")
	post(t, alice, ts.URL+"/admin/users/2/strengthlevel", url.Values{"enabled": {"1"}})
	post(t, alice, ts.URL+"/admin/users/2/strengthlevel-bulk", url.Values{"enabled": {"1"}})
	if err := st.PutIntegration(context.Background(), 2, strengthLevelProvider, []byte(`{"email":"a@b.c","password":"pw"}`)); err != nil {
		t.Fatal(err)
	}
	breakTable(t, st, "worker_jobs")
	code, page := body(t, bob, ts.URL+"/u/2/settings")
	if code != http.StatusOK || !strings.Contains(page, `name="days"`) {
		t.Fatalf("settings page: %d", code)
	}
}
