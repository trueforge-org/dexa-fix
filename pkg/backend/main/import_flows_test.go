package mainbackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
)

func (f *flakyStore) CancelJobs(ctx context.Context, userID int64, kinds []string, at time.Time) error {
	if f.fails("CancelJobs") {
		return errFlaky
	}
	return f.Store.CancelJobs(ctx, userID, kinds, at)
}

func withingsOf(b *Backend) *workerProvider {
	p, _ := b.providerNamed("withings")
	return p
}

type apiResult struct {
	status int
	body   string
	header http.Header
}

func (r *jobRig) callInTheBackground(method, path string) <-chan apiResult {
	out := make(chan apiResult, 1)
	go func() {
		s, b, h := r.call(method, path, "")
		out <- apiResult{s, b, h}
	}()
	return out
}

func TestImportThroughThePublicAPITellsTheCallerHowItEnded(t *testing.T) {
	const empty = `{"readings":[]}`
	ready := func(t *testing.T) *jobRig {
		r := newJobRig(t)
		r.connectWithings()
		return r
	}
	want := func(t *testing.T, status int, body string, header http.Header, wantStatus int, wantText string) {
		t.Helper()
		if status != wantStatus || !strings.Contains(body, wantText) {
			t.Fatalf("status %d body %.200q, want %d with %q", status, body, wantStatus, wantText)
		}
	}
	t.Run("a provider that does not exist", func(t *testing.T) {
		s, b, h := ready(t).call("POST", "/import/nope", "")
		want(t, s, b, h, 404, "not found")
	})
	t.Run("a since that is not a time", func(t *testing.T) {
		s, b, h := ready(t).call("POST", "/import/withings?since=yesterday", "")
		want(t, s, b, h, 400, "since must be")
	})
	t.Run("a wait that is not a number", func(t *testing.T) {
		s, b, h := ready(t).call("POST", "/import/withings?wait=soon", "")
		want(t, s, b, h, 400, "wait must be")
	})
	t.Run("a wait that is too long", func(t *testing.T) {
		s, b, h := ready(t).call("POST", "/import/withings?wait=9999", "")
		want(t, s, b, h, 400, "wait must be")
	})
	t.Run("a server without a Withings application", func(t *testing.T) {
		s, b, h := newJobRig(t).call("POST", "/import/withings", "")
		want(t, s, b, h, 501, "not configured")
	})
	t.Run("a person who has not connected Withings", func(t *testing.T) {
		r := newJobRig(t)
		setWithingsApp(t, r.st)
		s, b, h := r.call("POST", "/import/withings", "")
		want(t, s, b, h, 409, "not connected")
	})
	t.Run("a paused kind", func(t *testing.T) {
		r := ready(t)
		r.pauseKind(kindWithingsImport)
		s, b, h := r.call("POST", "/import/withings", "")
		want(t, s, b, h, 503, "paused by an administrator")
	})
	t.Run("no worker", func(t *testing.T) {
		s, b, h := ready(t).call("POST", "/import/withings", "")
		want(t, s, b, h, 503, "No Withings worker is running")
	})
	t.Run("a database that cannot queue the import", func(t *testing.T) {
		r := ready(t)
		r.workerIdle(kindWithingsImport)
		r.flaky.failOn("EnqueueJob")
		s, b, h := r.call("POST", "/import/withings", "")
		want(t, s, b, h, 500, "internal error")
	})
	t.Run("a database that cannot be asked about the job", func(t *testing.T) {
		r := ready(t)
		r.workerFor(stillBusy(), kindWithingsImport)
		r.flaky.failOn("GetJob")
		s, b, h := r.call("POST", "/import/withings?wait=5", "")
		want(t, s, b, h, 500, "internal error")
	})
	t.Run("an import that finishes in time", func(t *testing.T) {
		r := ready(t)
		r.workerFor(succeeds(empty), kindWithingsImport)
		s, b, h := r.call("POST", "/import/withings?wait=5", "")
		want(t, s, b, h, 200, `"provider":"withings"`)
	})
	t.Run("an import that fails", func(t *testing.T) {
		r := ready(t)
		r.workerFor(failsWith("Withings said no"), kindWithingsImport)
		s, b, h := r.call("POST", "/import/withings?wait=5", "")
		want(t, s, b, h, 502, "the import failed")
	})
	t.Run("an import that is canceled", func(t *testing.T) {
		r := ready(t)
		r.workerIdle(kindWithingsImport)
		first := r.callInTheBackground("POST", "/import/withings?wait=5")
		r.waitForActiveJob(kindWithingsImport)
		if err := r.st.CancelJobs(context.Background(), 2, []string{kindWithingsImport}, time.Now()); err != nil {
			t.Fatal(err)
		}
		got := <-first
		want(t, got.status, got.body, got.header, 409, "the import was canceled")
	})
	t.Run("an import that is only queued answers at once with the job", func(t *testing.T) {
		r := ready(t)
		r.workerIdle(kindWithingsImport)
		s, b, h := r.call("POST", "/import/withings?wait=0", "")
		want(t, s, b, h, 202, `"state":"queued"`)
		if loc := h.Get("Location"); !strings.HasPrefix(loc, "/api/v1/users/2/jobs/") {
			t.Fatalf("Location %q", loc)
		}
	})
	t.Run("a caller that gives up waiting leaves the import running", func(t *testing.T) {
		r := ready(t)
		r.workerFor(stillBusy(), kindWithingsImport)
		c := &http.Client{Timeout: 400 * time.Millisecond}
		req, _ := http.NewRequest("POST", r.pub.URL+"/api/v1/users/2/import/withings?wait=30", nil)
		req.Header.Set("Authorization", "Bearer "+r.token)
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
			t.Fatalf("an import that was never finished answered %d", resp.StatusCode)
		}
		if s, b, _ := r.call("GET", "/jobs", ""); s != 200 || !strings.Contains(b, `"kind":"withings.import"`) {
			t.Fatalf("jobs after the caller left: %d %.200q", s, b)
		}
	})
}

func TestTheJobsOfThePublicAPI(t *testing.T) {
	queued := func(t *testing.T) (*jobRig, string) {
		r := newJobRig(t)
		r.connectWithings()
		r.workerIdle(kindWithingsImport)
		_, body, _ := r.call("POST", "/import/withings?wait=0", "")
		id := body[strings.Index(body, `"id":"`)+6:]
		return r, id[:strings.Index(id, `"`)]
	}
	t.Run("the list shows the job", func(t *testing.T) {
		r, id := queued(t)
		if s, b, _ := r.call("GET", "/jobs", ""); s != 200 || !strings.Contains(b, id) {
			t.Fatalf("list: %d %.200q", s, b)
		}
	})
	t.Run("the list fails when the database does", func(t *testing.T) {
		r, _ := queued(t)
		r.flaky.failOn("ListJobs")
		if s, _, _ := r.call("GET", "/jobs", ""); s != 500 {
			t.Fatalf("list: %d, want 500", s)
		}
	})
	t.Run("one job can be read and another person's job is not found", func(t *testing.T) {
		r, id := queued(t)
		if s, b, _ := r.call("GET", "/jobs/"+id, ""); s != 200 || !strings.Contains(b, id) {
			t.Fatalf("job: %d %.200q", s, b)
		}
		if s, _, _ := r.call("GET", "/jobs/nope", ""); s != 404 {
			t.Fatalf("unknown job: %d, want 404", s)
		}
	})
	t.Run("a job can be canceled", func(t *testing.T) {
		r, id := queued(t)
		if s, b, _ := r.call("DELETE", "/jobs/"+id, ""); s != 200 || !strings.Contains(b, `"state":"canceled"`) {
			t.Fatalf("cancel: %d %.200q", s, b)
		}
		if s, _, _ := r.call("DELETE", "/jobs/nope", ""); s != 404 {
			t.Fatalf("cancel unknown: %d, want 404", s)
		}
	})
}

func TestImportFromTheSettingsPageTellsThePersonHowItEnded(t *testing.T) {
	const empty = `{"readings":[]}`
	imp := func(r *jobRig, day string) (apiclient.Flash, error) { return r.api.Import(r.alice, 2, "withings", day) }
	ready := func(t *testing.T) *jobRig {
		r := newJobRig(t)
		r.connectWithings()
		return r
	}
	t.Run("a provider that does not exist", func(t *testing.T) {
		r := ready(t)
		_, err := r.api.Import(r.alice, 2, "nope", "")
		if !apiclient.IsCode(err, apiclient.CodeNotFound) {
			t.Fatalf("error %v, want not found", err)
		}
	})
	t.Run("a day that is not a date", func(t *testing.T) {
		f, err := imp(ready(t), "tomorrow")
		wantFlash(t, f, err, "day must be a date")
	})
	t.Run("a server without a Withings application", func(t *testing.T) {
		f, err := imp(newJobRig(t), "")
		wantFlash(t, f, err, "not configured")
	})
	t.Run("a paused kind", func(t *testing.T) {
		r := ready(t)
		r.pauseKind(kindWithingsImport)
		f, err := imp(r, "")
		wantFlash(t, f, err, "paused by an administrator")
	})
	t.Run("no worker", func(t *testing.T) {
		f, err := imp(ready(t), "")
		wantFlash(t, f, err, "No Withings worker is running")
	})
	t.Run("a database that cannot queue the import", func(t *testing.T) {
		r := ready(t)
		r.workerIdle(kindWithingsImport)
		r.flaky.failOn("EnqueueJob")
		_, err := imp(r, "")
		wantUnavailable(t, err)
	})
	t.Run("a database that cannot be asked about the job", func(t *testing.T) {
		r := ready(t)
		r.workerFor(stillBusy(), kindWithingsImport)
		r.flaky.failOn("GetJob")
		_, err := imp(r, "")
		wantUnavailable(t, err)
	})
	t.Run("an import that finds nothing", func(t *testing.T) {
		r := ready(t)
		r.workerFor(succeeds(empty), kindWithingsImport)
		f, err := imp(r, "2026-03-01")
		wantFlash(t, f, err, "Withings for 2026-03-01: nothing found")
	})
	t.Run("an import that fails", func(t *testing.T) {
		r := ready(t)
		r.workerFor(failsWith("Withings said no"), kindWithingsImport)
		f, err := imp(r, "")
		wantFlash(t, f, err, "Withings import failed")
	})
	t.Run("an import that is canceled", func(t *testing.T) {
		r := ready(t)
		r.workerIdle(kindWithingsImport)
		first := inTheBackground(func() (apiclient.Flash, error) { return imp(r, "") })
		r.waitForActiveJob(kindWithingsImport)
		if err := r.st.CancelJobs(context.Background(), 2, []string{kindWithingsImport}, time.Now()); err != nil {
			t.Fatal(err)
		}
		got := <-first
		wantFlash(t, got.flash, got.err, "import was canceled")
	})
	t.Run("an import that takes long goes on in the background", func(t *testing.T) {
		impatient(t)
		r := ready(t)
		r.workerFor(stillBusy(), kindWithingsImport)
		f, err := imp(r, "")
		wantFlash(t, f, err, "running in the background")
	})
	t.Run("a person who leaves while the import runs gets no answer", func(t *testing.T) {
		r := ready(t)
		r.workerFor(stillBusy(), kindWithingsImport)
		ctx, cancel := context.WithTimeout(r.alice, 400*time.Millisecond)
		defer cancel()
		if f, err := r.api.Import(ctx, 2, "withings", ""); err == nil {
			t.Fatalf("an unfinished import answered %+v", f)
		}
	})
}

func TestConnectingAndDisconnectingAProvider(t *testing.T) {
	start := func(t *testing.T, r *jobRig) (cookie, state string) {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx := apiclient.WithRequest(r.alice, apiclient.Request{Cookie: r.cookie, Secure: true, Relay: rec})
		if _, err := r.api.ConnectIntegration(ctx, 2, "withings"); err != nil {
			t.Fatal(err)
		}
		ck := relayedCookie(rec, oauthCookiePrefix+"withings")
		return ck.Name + "=" + ck.Value, strings.TrimPrefix(ck.Value, "2.")
	}
	back := func(r *jobRig, cookie, state, code string) (apiclient.Flash, error) {
		ctx := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: r.cookie + "; " + cookie})
		return r.api.IntegrationCallback(ctx, "withings", state, code, "")
	}
	t.Run("connecting a provider that does not exist", func(t *testing.T) {
		r := newJobRig(t)
		if _, err := r.api.ConnectIntegration(r.alice, 2, "nope"); !apiclient.IsCode(err, apiclient.CodeNotFound) {
			t.Fatalf("error %v, want not found", err)
		}
	})
	t.Run("connecting when the application cannot be read", func(t *testing.T) {
		r := newJobRig(t)
		setWithingsApp(t, r.st)
		r.flaky.failAfter("GetSetting:"+settingWithingsClientID, 1)
		_, err := r.api.ConnectIntegration(r.alice, 2, "withings")
		wantUnavailable(t, err)
	})
	t.Run("a callback for a provider that does not exist", func(t *testing.T) {
		r := newJobRig(t)
		if _, err := r.api.IntegrationCallback(r.alice, "nope", "s", "c", ""); !apiclient.IsCode(err, apiclient.CodeNotFound) {
			t.Fatalf("error %v, want not found", err)
		}
	})
	t.Run("a callback with a state cookie that is not one", func(t *testing.T) {
		r := newJobRig(t)
		setWithingsApp(t, r.st)
		f, err := back(r, oauthCookiePrefix+"withings=nonsense.state", "state", "code")
		_ = f
		if !apiclient.IsCode(err, apiclient.CodeInvalid) {
			t.Fatalf("error %v, want invalid", err)
		}
		_, err = back(r, oauthCookiePrefix+"withings=x.state", "state", "code")
		if !apiclient.IsCode(err, apiclient.CodeInvalid) {
			t.Fatalf("error %v, want invalid", err)
		}
	})
	t.Run("a callback without a code", func(t *testing.T) {
		r := newJobRig(t)
		setWithingsApp(t, r.st)
		cookie, state := start(t, r)
		if _, err := back(r, cookie, state, ""); !apiclient.IsCode(err, apiclient.CodeInvalid) {
			t.Fatalf("error %v, want invalid", err)
		}
	})
	t.Run("a callback when there is no worker", func(t *testing.T) {
		r := newJobRig(t)
		setWithingsApp(t, r.st)
		cookie, state := start(t, r)
		f, err := back(r, cookie, state, "code")
		wantFlash(t, f, err, "Could not connect Withings. No Withings worker is running")
	})
	t.Run("a callback when the database cannot queue the connection", func(t *testing.T) {
		r := newJobRig(t)
		setWithingsApp(t, r.st)
		r.workerIdle(kindWithingsConnect)
		cookie, state := start(t, r)
		r.flaky.failOn("EnqueueJob")
		_, err := back(r, cookie, state, "code")
		wantUnavailable(t, err)
	})
	t.Run("a callback when the database cannot be asked about the job", func(t *testing.T) {
		r := newJobRig(t)
		setWithingsApp(t, r.st)
		r.workerFor(stillBusy(), kindWithingsConnect)
		cookie, state := start(t, r)
		r.flaky.failOn("GetJob")
		_, err := back(r, cookie, state, "code")
		wantUnavailable(t, err)
	})
	t.Run("a connection that takes long goes on in the background", func(t *testing.T) {
		impatient(t)
		r := newJobRig(t)
		setWithingsApp(t, r.st)
		r.workerFor(stillBusy(), kindWithingsConnect)
		cookie, state := start(t, r)
		f, err := back(r, cookie, state, "code")
		wantFlash(t, f, err, "is being connected in the background")
	})
	t.Run("a connection that fails", func(t *testing.T) {
		r := newJobRig(t)
		setWithingsApp(t, r.st)
		r.workerFor(failsWith("Withings said no"), kindWithingsConnect)
		cookie, state := start(t, r)
		f, err := back(r, cookie, state, "code")
		wantFlash(t, f, err, "Could not connect Withings. The details are in the server log.")
	})
	t.Run("a connection that works", func(t *testing.T) {
		r := newJobRig(t)
		setWithingsApp(t, r.st)
		r.workerFor(func(l workerapi.Lease) *workerapi.Completion {
			// The worker hands the tokens over before it reports success.
			if err := r.worker.SaveCredentials(context.Background(), l.Job.ID, l.Token, []byte(`{"access_token":"a","refresh_token":"r"}`)); err != nil {
				t.Error(err)
			}
			return succeeds("{}")(l)
		}, kindWithingsConnect)
		cookie, state := start(t, r)
		f, err := back(r, cookie, state, "code")
		wantFlash(t, f, err, "Withings connected.")
	})
	t.Run("a person who leaves while the connection is made gets no answer", func(t *testing.T) {
		r := newJobRig(t)
		setWithingsApp(t, r.st)
		r.workerFor(stillBusy(), kindWithingsConnect)
		cookie, state := start(t, r)
		ctx, cancel := context.WithTimeout(apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: r.cookie + "; " + cookie}), 400*time.Millisecond)
		defer cancel()
		if f, err := r.api.IntegrationCallback(ctx, "withings", state, "code", ""); err == nil {
			t.Fatalf("an unfinished connection answered %+v", f)
		}
	})
	t.Run("disconnecting a provider that does not exist", func(t *testing.T) {
		r := newJobRig(t)
		if _, err := r.api.DisconnectIntegration(r.alice, 2, "nope"); !apiclient.IsCode(err, apiclient.CodeNotFound) {
			t.Fatalf("error %v, want not found", err)
		}
	})
	t.Run("disconnecting", func(t *testing.T) {
		r := newJobRig(t)
		r.connectWithings()
		f, err := r.api.DisconnectIntegration(r.alice, 2, "withings")
		wantFlash(t, f, err, "Withings disconnected.")
		if r.b.connected(context.Background(), withingsOf(r.b), 2) {
			t.Fatal("still connected")
		}
	})
	t.Run("disconnecting when the database cannot forget the link", func(t *testing.T) {
		r := newJobRig(t)
		r.connectWithings()
		r.flaky.failOn("DeleteIntegration")
		_, err := r.api.DisconnectIntegration(r.alice, 2, "withings")
		wantUnavailable(t, err)
	})
	t.Run("disconnecting when the database cannot stop the jobs", func(t *testing.T) {
		r := newJobRig(t)
		r.connectWithings()
		r.flaky.failOn("CancelJobs")
		_, err := r.api.DisconnectIntegration(r.alice, 2, "withings")
		wantUnavailable(t, err)
	})
}
