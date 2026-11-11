package frontend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// configAnswer makes the backend answer its frontend config with the given JSON.
func configAnswer(json string) faultFn { return answering(http.StatusOK, json) }

// waitForLog polls until what the process logged contains want.
func waitForLog(t *testing.T, out *logOutput, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), want); {
		if time.Now().After(deadline) {
			t.Fatalf("the log never said %q:\n%s", want, out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTheFrontendKeepsItsSettingsWhenTheBackendCannotGiveNewOnes(t *testing.T) {
	fs := newAnonymousStack(t)
	ctx := context.Background()
	before := fs.f.cfg()

	fs.fail(serverError, "GET", "/frontend-config")
	if err := fs.f.RefreshConfig(ctx); err == nil {
		t.Error("a failing backend was not reported")
	}
	for name, body := range map[string]string{
		"a base URL that is not an origin":      `{"base_url":"ftp://nope"}`,
		"trusted proxies that are no addresses": `{"trusted_proxies":["nonsense"]}`,
	} {
		fs.fail(configAnswer(body), "GET", "/frontend-config")
		if err := fs.f.RefreshConfig(ctx); err == nil || !strings.Contains(err.Error(), "from the backend") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if fs.f.cfg() != before {
		t.Error("the settings changed although every answer was refused")
	}
}

func TestSettingsFromTheBackendAreMadeSafe(t *testing.T) {
	out := captureLog(t)
	fs := newAnonymousStack(t)
	fs.fail(configAnswer(`{"tls_mode":"bogus"}`), "GET", "/frontend-config")
	if err := fs.f.RefreshConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l := fs.f.cfg(); l.tlsMode != apiclient.TLSOff || l.httpsPort != defaultHTTPSPort {
		t.Errorf("an unknown TLS mode or a missing port was not replaced: %+v", l)
	}

	fs.fail(configAnswer(`{"tls_mode":"upload","cert_pem":"junk","key_pem":"junk"}`), "GET", "/frontend-config")
	if err := fs.f.RefreshConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fs.f.cfg().tlsCert != nil || fs.f.cfg().http3 {
		t.Error("a certificate that is no certificate was used")
	}
	waitForLog(t, out, "ignoring the TLS certificate from the backend")
}

func TestTheFrontendWarnsOnceAboutAnotherVersionOfTheBackend(t *testing.T) {
	out := captureLog(t)
	fs := newAnonymousStack(t)
	fs.f.Version = "1.0.0"
	fs.fail(configAnswer(`{"version":"2.0.0"}`), "GET", "/frontend-config")
	for range 2 {
		if err := fs.f.RefreshConfig(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(out.String(), "run different versions"); n != 1 {
		t.Fatalf("warned %d times:\n%s", n, out.String())
	}
}

func TestWaitingForTheBackend(t *testing.T) {
	t.Run("a backend that refuses the service secret is named as the cause", func(t *testing.T) {
		fs := newAnonymousStack(t)
		fs.fail(answering(http.StatusUnauthorized, `{"code":"unauthorized","message":"no"}`), "GET", "/frontend-config")
		err := fs.f.WaitForBackend(context.Background(), 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "refused the service secret") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a backend that stays away is given up on", func(t *testing.T) {
		fs := newAnonymousStack(t)
		fs.failAll(serverError)
		err := fs.f.WaitForBackend(context.Background(), 100*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "the backend is not available") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a backend that comes up while waiting is used", func(t *testing.T) {
		out := captureLog(t)
		fs := newAnonymousStack(t)
		var calls atomic.Int32
		fs.fail(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				serverError(w, r)
				return
			}
			configAnswer(`{"generation":1,"tls_mode":"off"}`)(w, r)
		}, "GET", "/frontend-config")
		if err := fs.f.WaitForBackend(context.Background(), 10*time.Second); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 || !strings.Contains(out.String(), "waiting for the backend") {
			t.Errorf("%d calls, log:\n%s", calls.Load(), out.String())
		}
	})
	t.Run("a caller that stops waiting ends the wait", func(t *testing.T) {
		fs := newAnonymousStack(t)
		fs.failAll(serverError)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := fs.f.WaitForBackend(ctx, time.Minute); err == nil {
			t.Fatal("waited although the caller was gone")
		}
	})
}

func TestThePollWarnsWhenTheBackendCannotBeAsked(t *testing.T) {
	out := captureLog(t)
	old := configPollInterval
	configPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { configPollInterval = old })
	fs := newAnonymousStack(t)
	fs.fail(serverError, "GET", "/frontend-config")
	fs.f.StartConfigPoll()
	waitForLog(t, out, "could not refresh the settings from the backend; keeping the last ones")
}

func TestAnAdminChangeThatNeedsNewSettingsStillAnswersWhenTheyCannotBeFetched(t *testing.T) {
	out := captureLog(t)
	fs := newFaultStack(t)
	fs.fail(answering(http.StatusOK, `{"message":"Saved.","config_changed":true}`), "POST", "/admin/signup")
	fs.fail(serverError, "GET", "/frontend-config")
	res := fs.post("/admin/signup", v("enabled", "1"))
	if res.code != http.StatusSeeOther || !strings.Contains(res.header.Get("Location"), "msg=Saved.") {
		t.Fatalf("%d %v", res.code, res.header)
	}
	waitForLog(t, out, "could not fetch the new settings from the backend")
}

func TestTheSessionCheckFailingIsTheBackendBeingUnavailable(t *testing.T) {
	fs := newFaultStack(t)
	fs.fail(serverError, "GET", "/session")
	res := fs.get("/u/1/estimates")
	if res.code != http.StatusServiceUnavailable || res.header.Get("Retry-After") != "5" {
		t.Fatalf("%d, Retry-After %q", res.code, res.header.Get("Retry-After"))
	}
}

func TestNothingIsAnsweredToAVisitorWhoWentAway(t *testing.T) {
	fs := newFaultStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	fs.f.backendDown(rec, httptest.NewRequest("GET", "/", nil).WithContext(ctx), context.Canceled)
	if rec.Body.Len() != 0 || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("answered anyway: %d %q", rec.Code, rec.Body.String())
	}
}

func TestSignInPostsAreLimitedMoreStrictlyThanOtherRequests(t *testing.T) {
	fs := newAnonymousStack(t)
	fs.f.unlimited = false // the stack turns the limits off
	limited := httptest.NewServer(fs.f.Handler())
	t.Cleanup(limited.Close)
	fs.failAll(serverError)
	var last *http.Response
	for range signInBurst + 1 {
		resp, err := http.PostForm(limited.URL+"/login", url.Values{"email": {"a@b.c"}, "password": {"x"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp
	}
	if last.StatusCode != http.StatusTooManyRequests || last.Header.Get("Retry-After") != "2" {
		t.Fatalf("the request over the burst: %d, Retry-After %q", last.StatusCode, last.Header.Get("Retry-After"))
	}
	// Reading pages is not a sign-in: it has the larger general budget.
	resp, err := http.Get(limited.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Error("a page view was limited like a sign-in")
	}
}

func TestOnlyRequestsOfPeopleWhoAreNotSignedInAreCountedAsSignIns(t *testing.T) {
	for path, want := range map[string]bool{
		"/login": true, "/signup": true, "/password/forgot": true, "/password/reset": true, "/login/2fa": true,
		"/login/2fa/setup": true, "/passkey/login/begin": true, "/passkey/login/finish": true, "/auth/github/callback": true,
		"/u/1/dexa": false, "/admin/users": false, "/logout": false, "/": false,
	} {
		if got := unauthenticatedPost(path); got != want {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
}

func TestReadinessFollowsTheBackend(t *testing.T) {
	fs := newAnonymousStack(t)
	if res := fs.get("/readyz"); res.code != http.StatusOK || res.body != "ok" {
		t.Fatalf("healthy: %d %q", res.code, res.body)
	}
	fs.fail(serverError, "GET", "/healthz")
	if res := fs.get("/readyz"); res.code != http.StatusServiceUnavailable {
		t.Fatalf("with the backend down: %d", res.code)
	}
}

func TestWithoutAnAPIBehindItTheFrontendSaysWhereTheAPILives(t *testing.T) {
	fs := newAnonymousStack(t)
	res := fs.get("/api/v1/anything")
	if res.code != http.StatusNotFound || !strings.Contains(res.body, "served by the api process") {
		t.Fatalf("%d %q", res.code, res.body)
	}
}
