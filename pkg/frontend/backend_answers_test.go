package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

func TestPagesBeforeSigningInSayTheBackendIsUnavailable(t *testing.T) {
	fs := newFaultStack(t)
	fs.failAll(serverError)
	for _, c := range []pageCall{
		{"the sign-in page", "GET", "/", nil},
		{"signing in", "POST", "/login", v("email", "a@b.c", "password", "x")},
		{"signing up", "POST", "/signup", v("name", "a", "email", "a@b.c", "password", "x", "confirm", "x")},
		{"asking for a reset link", "GET", "/password/forgot", nil},
		{"sending the e-mail address", "POST", "/password/forgot", v("email", "a@b.c")},
		{"the reset form", "GET", "/password/reset?t=abc", nil},
		{"setting the new password", "POST", "/password/reset", v("t", "abc", "password", "x", "confirm", "x")},
		{"the second step", "GET", "/login/2fa", nil},
		{"sending the code", "POST", "/login/2fa", v("code", "123456")},
		{"the required setup", "GET", "/login/2fa/setup", nil},
		{"sending the setup code", "POST", "/login/2fa/setup", v("code", "123456")},
		{"a provider sign-in", "GET", "/auth/github/login", nil},
		{"a provider callback", "GET", "/auth/github/callback?code=x", nil},
	} {
		res := fs.anonymous(c.method, c.path, c.form)
		if res.code != http.StatusServiceUnavailable || res.header.Get("Retry-After") == "" {
			t.Errorf("%s: %d %q", c.name, res.code, res.body)
		}
	}
}

func TestThePasskeyScriptGetsAJSONErrorWhenTheBackendFails(t *testing.T) {
	fs := newFaultStack(t)
	fs.failAll(serverError, guards...)
	for _, c := range []pageCall{
		{"starting a sign-in", "POST", "/passkey/login/begin", nil},
		{"finishing a sign-in", "POST", "/passkey/login/finish", nil},
		{"starting a registration", "POST", "/u/1/passkeys/begin", nil},
		{"finishing a registration", "POST", "/u/1/passkeys/finish", nil},
		{"deleting a passkey", "POST", "/u/1/passkeys/abc/delete", nil},
		{"renaming a passkey", "POST", "/u/1/passkeys/abc/rename", v("name", "phone")},
	} {
		res := fs.post(c.path, c.form)
		if c.name == "deleting a passkey" || c.name == "renaming a passkey" { // settings forms, not the script
			if res.code != http.StatusServiceUnavailable {
				t.Errorf("%s: %d %q", c.name, res.code, res.body)
			}
			continue
		}
		if res.code != http.StatusInternalServerError || !strings.Contains(res.body, `"error":"Something went wrong."`) {
			t.Errorf("%s: %d %q", c.name, res.code, res.body)
		}
	}
}

func TestThePasskeyScriptSeesTheBackendsRefusal(t *testing.T) {
	fs := newFaultStack(t)
	fs.fail(answering(http.StatusConflict, `{"code":"exists","message":"That passkey is already registered."}`), "POST", "/users/1/passkeys/begin")
	res := fs.post("/u/1/passkeys/begin", nil)
	if res.code != http.StatusConflict || !strings.Contains(res.body, "That passkey is already registered.") {
		t.Fatalf("%d %q", res.code, res.body)
	}
}

func TestARenamedPasskeyShowsTheBackendsComplaint(t *testing.T) {
	fs := newFaultStack(t)
	fs.fail(answering(http.StatusBadRequest, `{"code":"invalid","message":"Give the passkey a name."}`), "POST", "/users/1/passkeys/abc/rename")
	res := fs.post("/u/1/passkeys/abc/rename", v("name", " "))
	if res.code != http.StatusSeeOther || !strings.Contains(res.header.Get("Location"), url.QueryEscape("Give the passkey a name.")) {
		t.Fatalf("%d %v", res.code, res.header)
	}
}

func TestRefusedFormsShowTheBackendsMessageAndStatus(t *testing.T) {
	fs := newFaultStack(t)
	refuse := func(status int, code, msg string) faultFn {
		return answering(status, `{"code":"`+code+`","message":"`+msg+`"}`)
	}
	t.Run("a DEXA scan the backend finds invalid", func(t *testing.T) {
		fs.fail(refuse(400, apiclient.CodeInvalid, "Body fat must be below 100."), "POST", "/users/1/dexa")
		res := fs.post("/u/1/dexa", v("measured_at", "2026-01-02", "weight_kg", "80"))
		if res.code != http.StatusUnprocessableEntity || !strings.Contains(res.body, "Body fat must be below 100.") {
			t.Fatalf("%d", res.code)
		}
	})
	t.Run("a reading the backend finds invalid", func(t *testing.T) {
		fs.fail(refuse(400, apiclient.CodeInvalid, "Weight must be positive."), "POST", "/users/1/scale")
		res := fs.post("/u/1/scale", v("measured_at", "2026-01-02T08:00", "weight_kg", "80"))
		if res.code != http.StatusUnprocessableEntity || !strings.Contains(res.body, "Weight must be positive.") {
			t.Fatalf("%d", res.code)
		}
	})
	t.Run("a link to a provider that asks for the current password", func(t *testing.T) {
		fs.fail(refuse(403, apiclient.CodeForbidden, "Enter your current password."), "POST", "/auth/providers/github/start")
		res := fs.post("/auth/github/login", nil)
		if res.code != http.StatusForbidden || !strings.Contains(res.body, "Enter your current password.") {
			t.Fatalf("%d", res.code)
		}
	})
	t.Run("a sign-in the backend turns away", func(t *testing.T) {
		fs.heal()
		fs.fail(refuse(404, apiclient.CodeNotFound, "No such page."), "GET", "/users/1/export/data")
		if res := fs.get("/u/1/export/data"); res.code != http.StatusNotFound {
			t.Fatalf("%d %q", res.code, res.body)
		}
	})
}

func TestStatusCodesOfTheBackendMapToTheVisitorsPage(t *testing.T) {
	f := &Frontend{}
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"not found":               {&apiclient.Error{Status: 404, Message: "gone"}, 404},
		"too many requests":       {&apiclient.Error{Status: 429, Message: "slow down"}, 429},
		"an error nobody expects": {&apiclient.Error{Status: 500}, 500},
		"another client error":    {&apiclient.Error{Status: 418, Message: "teapot"}, 400},
		"a plain error":           {errors.New("something"), 500},
	} {
		rec := httptest.NewRecorder()
		f.fail(rec, tc.err)
		if rec.Code != tc.want {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

func TestAnUnknownStaticFileKeepsItsPlainURL(t *testing.T) {
	f := &Frontend{assetVer: map[string]string{"app.css": "abc123"}}
	if got := f.assetURL("app.css"); got != "/static/app.css?v=abc123" {
		t.Errorf("known file: %q", got)
	}
	if got := f.assetURL("other.css"); got != "/static/other.css" {
		t.Errorf("unknown file: %q", got)
	}
}

func TestAPageThatCannotBeRenderedIsAnInternalError(t *testing.T) {
	fs := newFaultStack(t)
	rec := httptest.NewRecorder()
	fs.f.render(rec, "dexa.html", http.StatusOK, pageData{Rows: []row{{ID: 1}}}) // a row without the user it belongs to cannot be shown
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "internal error") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestMessagesInRedirectsAreCutToAReasonableLength(t *testing.T) {
	long := strings.Repeat("x", 2*maxFlashLen)
	for name, redirect := range map[string]func(w http.ResponseWriter, r *http.Request){
		"to the Admin page":    func(w http.ResponseWriter, r *http.Request) { adminRedirect(w, r, long) },
		"to the estimates":     func(w http.ResponseWriter, r *http.Request) { flashRedirect(w, r, 1, long) },
		"to the settings page": func(w http.ResponseWriter, r *http.Request) { settingsRedirect(w, r, 1, long) },
	} {
		rec := httptest.NewRecorder()
		redirect(rec, httptest.NewRequest("POST", "/", nil))
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil || len(loc.Query().Get("msg")) != maxFlashLen {
			t.Errorf("%s: %q", name, rec.Header().Get("Location"))
		}
	}
	r := httptest.NewRequest("GET", "/?msg="+long, nil)
	if got := flashParam(r); len(got) != maxFlashLen {
		t.Errorf("flashParam kept %d characters", len(got))
	}
}

func TestInvalidFormsAreRefusedBeforeTheBackendIsAsked(t *testing.T) {
	fs := newFaultStack(t)
	fs.failAll(serverError, guards...)
	if res := fs.postRaw("/u/1/estimates/columns", "column=%zz"); res.code != http.StatusBadRequest || !strings.Contains(res.body, "invalid form") {
		t.Errorf("columns: %d %q", res.code, res.body)
	}
	if res := fs.get("/auth/github/callback?code=%zz"); res.code != http.StatusBadRequest || !strings.Contains(res.body, "start again") {
		t.Errorf("callback: %d %q", res.code, res.body)
	}
}

func TestPathsWithAnItemThatIsNotANumberAreNotFound(t *testing.T) {
	fs := newFaultStack(t)
	fs.failAll(serverError, guards...) // nothing may reach the backend
	for _, c := range []pageCall{
		{"deleting a reading", "POST", "/u/1/scale/abc/delete", nil},
		{"revoking a token", "POST", "/u/1/tokens/abc/delete", nil},
		{"a role", "POST", "/admin/users/abc/role", v("role", "admin")},
		{"Strength Level for a user", "POST", "/admin/users/abc/strengthlevel", nil},
		{"Strength Level for everyone", "POST", "/admin/users/abc/strengthlevel-bulk", nil},
		{"a password", "POST", "/admin/users/abc/password", nil},
		{"deleting a user", "POST", "/admin/users/abc/delete", nil},
		{"a reset link", "POST", "/admin/users/abc/reset-link", nil},
		{"removing the second step", "POST", "/admin/users/abc/2fa/delete", nil},
		{"a priority", "POST", "/admin/users/abc/priority", v("priority", "1")},
		{"someone else's page", "GET", "/u/2/estimates", nil},
		{"a page of an id that is no number", "GET", "/u/abc/estimates", nil},
	} {
		if res := fs.post(c.path, c.form); c.method == "POST" && res.code != http.StatusNotFound {
			t.Errorf("%s: %d", c.name, res.code)
		}
		if c.method == "GET" {
			if res := fs.get(c.path); res.code != http.StatusNotFound {
				t.Errorf("%s: %d", c.name, res.code)
			}
		}
	}
}
