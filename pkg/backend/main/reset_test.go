package mainbackend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// sentMail is one e-mail the fake mailer was asked to send.
type sentMail struct{ to, subject, body string }

// fakeMailer records the e-mails instead of sending them.
type fakeMailer struct {
	mu   sync.Mutex
	sent []sentMail
}

func (f *fakeMailer) Send(_ context.Context, to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMail{to, subject, body})
	return nil
}

func (f *fakeMailer) all() []sentMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMail(nil), f.sent...)
}

// waitFor waits up to two seconds for n e-mails (the server sends them in the background) and
// returns what has arrived.
func (f *fakeMailer) waitFor(n int) []sentMail {
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := f.all()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var resetLinkRe = regexp.MustCompile(`/password/reset\?t=([0-9a-f]{64})`)

// resetTokenIn returns the token of the reset link found in a page or an e-mail.
func resetTokenIn(t *testing.T, text string) string {
	t.Helper()
	m := resetLinkRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no reset link in:\n%s", text)
	}
	return m[1]
}

// enableSelfService turns on e-mailed reset links: a mailer and a public base URL. The current
// settings are copied, so everything else stays as it was.
func enableSelfService(t *testing.T, srv *Server, fm *fakeMailer) {
	t.Helper()
	base, err := url.Parse("https://dexa.example.com")
	if err != nil {
		t.Fatal(err)
	}
	l := *srv.cfg()
	l.mailer = fm
	l.baseURL = base
	srv.swapSettings(&l)
}

// makeResetLink makes a reset link for a user the way the administrator does on the Admin page. It
// returns the token and the page that shows the link.
func makeResetLink(t *testing.T, ts *httptest.Server, admin *http.Client, uid int64) (string, string) {
	t.Helper()
	resp, page := tfRequest(t, admin, "POST", fmt.Sprintf("%s/admin/users/%d/reset-link", ts.URL, uid), url.Values{})
	if resp.StatusCode != 200 {
		t.Fatalf("reset link for user %d: %d\n%s", uid, resp.StatusCode, page)
	}
	return resetTokenIn(t, page), page
}

// signInWith is a password sign-in with the given password.
func signInWith(t *testing.T, c *http.Client, base, email, password string) *http.Response {
	t.Helper()
	resp, _ := tfRequest(t, c, "POST", base+"/login", url.Values{"email": {email}, "password": {password}})
	return resp
}

// resetForm is the form a person submits on the reset page, without a code.
func resetForm(token, password string) url.Values {
	return url.Values{"t": {token}, "password": {password}, "confirm": {password}}
}

// resetPOST submits the reset form.
func resetPOST(t *testing.T, c *http.Client, base string, form url.Values) (*http.Response, string) {
	t.Helper()
	return tfRequest(t, c, "POST", base+"/password/reset", form)
}

func TestAdminResetLinkLifecycle(t *testing.T) {
	ts, admin, st := newAuthServer(t)
	ctx := context.Background()
	signup(t, ts, admin, "root") // user 1, administrator
	bob := noRedirectClient()
	signup(t, ts, bob, "bob") // user 2
	carol := noRedirectClient()
	signup(t, ts, carol, "carol") // user 3

	// Without a base URL the link is built from the address the administrator uses, and the page says so.
	resp, page := tfRequest(t, admin, "POST", ts.URL+"/admin/users/2/reset-link", url.Values{})
	if resp.StatusCode != 200 || !strings.Contains(page, "Copy this link now") || !strings.Contains(page, "/password/reset?t=") {
		t.Fatalf("reset link: %d\n%s", resp.StatusCode, page)
	}
	if !strings.Contains(page, "built from the address you are using now") {
		t.Fatal("no warning on a link built from the request")
	}
	token := resetTokenIn(t, page)

	// only administrators make links
	if r, _ := tfRequest(t, carol, "POST", ts.URL+"/admin/users/2/reset-link", url.Values{}); r.StatusCode != 404 {
		t.Fatalf("non-admin reset link: %d", r.StatusCode)
	}

	// before the reset, bob has a session and an access token
	pat := createPAT(t, ts, bob, 2, url.Values{"name": {"script"}, "expires": {"never"}})
	if code, _ := apiCall(t, noRedirectClient(), "GET", ts.URL+"/api/v1/users/2/settings", "", bearer(pat)); code != 200 {
		t.Fatalf("token before the reset: %d", code)
	}

	// the form opens with the link, and carries it on
	r, page := tfRequest(t, noRedirectClient(), "GET", ts.URL+"/password/reset?t="+token, nil)
	if r.StatusCode != 200 || !strings.Contains(page, `name="t" value="`+token+`"`) || strings.Contains(page, `name="code"`) {
		t.Fatalf("reset form: %d\n%s", r.StatusCode, page)
	}
	if r.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("Referrer-Policy: %q", r.Header.Get("Referrer-Policy"))
	}

	// bad submissions are refused and do not use the link up
	const next = "a new password for bob"
	fresh := noRedirectClient()
	if r, _ := resetPOST(t, fresh, ts.URL, url.Values{"t": {token}, "password": {next}, "confirm": {"something else"}}); r.StatusCode != 422 {
		t.Fatalf("mismatch: %d", r.StatusCode)
	}
	if r, _ := resetPOST(t, fresh, ts.URL, resetForm(token, "short")); r.StatusCode != 422 {
		t.Fatalf("short password: %d", r.StatusCode)
	}

	// the reset: signed in, the user is sent to the account
	r, _ = resetPOST(t, fresh, ts.URL, resetForm(token, next))
	if r.StatusCode != 303 || !tfHasSession(r) || r.Header.Get("Location") != "/u/2" {
		t.Fatalf("reset: %d %s, session %v", r.StatusCode, r.Header.Get("Location"), tfHasSession(r))
	}
	signedIn := noRedirectClient()
	useSession(t, signedIn, r)
	if code, _ := body(t, signedIn, ts.URL+"/u/2/settings"); code != 200 {
		t.Fatalf("session from the reset: %d", code)
	}

	// the new password works, the old one does not
	if r := signInWith(t, noRedirectClient(), ts.URL, "bob@example.com", next); r.StatusCode != 303 {
		t.Fatalf("new password: %d", r.StatusCode)
	}
	if r := signInWith(t, noRedirectClient(), ts.URL, "bob@example.com", testPassword); r.StatusCode != 401 {
		t.Fatalf("old password: %d", r.StatusCode)
	}

	// everything bob had before is signed out
	if code, _ := body(t, bob, ts.URL+"/u/2/settings"); code != 303 {
		t.Fatalf("old session after the reset: %d", code)
	}
	if code, _ := apiCall(t, noRedirectClient(), "GET", ts.URL+"/api/v1/users/2/settings", "", bearer(pat)); code != 401 {
		t.Fatalf("access token after the reset: %d", code)
	}

	// the link is used up
	if r, page := tfRequest(t, noRedirectClient(), "GET", ts.URL+"/password/reset?t="+token, nil); r.StatusCode != 404 || !strings.Contains(page, "does not work") {
		t.Fatalf("used link: %d", r.StatusCode)
	}
	if r, _ := resetPOST(t, noRedirectClient(), ts.URL, resetForm(token, "yet another password")); r.StatusCode != 404 {
		t.Fatalf("reused link: %d", r.StatusCode)
	}
	if r := signInWith(t, noRedirectClient(), ts.URL, "bob@example.com", "yet another password"); r.StatusCode != 401 {
		t.Fatalf("password set by a reused link: %d", r.StatusCode)
	}

	// with password sign-in off nothing can be reset, and the reset pages refuse
	if err := st.PutSetting(ctx, settingPasswordOn, valueDisabled); err != nil {
		t.Fatal(err)
	}
	r, _ = tfRequest(t, admin, "POST", ts.URL+"/admin/users/3/reset-link", url.Values{})
	loc, _ := url.Parse(r.Header.Get("Location"))
	if r.StatusCode != 303 || loc.Path != "/admin" || !strings.Contains(loc.Query().Get("msg"), "nothing to reset") {
		t.Fatalf("admin link with password sign-in off: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := tfRequest(t, noRedirectClient(), "GET", ts.URL+"/password/reset?t="+token, nil); r.StatusCode != 403 {
		t.Fatalf("reset page with password sign-in off: %d", r.StatusCode)
	}
	if r, _ := resetPOST(t, noRedirectClient(), ts.URL, resetForm(token, next)); r.StatusCode != 403 {
		t.Fatalf("reset with password sign-in off: %d", r.StatusCode)
	}
}

// forgotPOST asks for a reset link by e-mail. The host is what the request claims to be (empty
// keeps the test server's own).
func forgotPOST(t *testing.T, ts *httptest.Server, host, email string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/password/forgot", strings.NewReader(url.Values{"email": {email}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if host != "" {
		req.Host = host
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestForgotPassword(t *testing.T) {
	ts, admin, st := newAuthServer(t)
	srv := serverOf(ts)
	ctx := context.Background()
	signup(t, ts, admin, "root") // user 1
	alice := noRedirectClient()
	signup(t, ts, alice, "alice") // user 2, has a password
	dana := noRedirectClient()
	signup(t, ts, dana, "dana") // user 3, for the rate limit
	if _, err := st.CreateUserWithIdentity(ctx, "pat", "pat@example.com", models.Identity{Provider: "idp", Subject: "pat-1"}); err != nil {
		t.Fatal(err) // signs in with a provider only: no password to reset
	}
	fm := &fakeMailer{}
	enableSelfService(t, srv, fm)

	if r, page := tfRequest(t, noRedirectClient(), "GET", ts.URL+"/password/forgot", nil); r.StatusCode != 200 || !strings.Contains(page, `name="email"`) {
		t.Fatalf("forgot page: %d", r.StatusCode)
	}

	// the request claims another host: the e-mailed link still uses the base URL
	code, sent := forgotPOST(t, ts, "evil.example", "alice@example.com")
	if code != 200 || !strings.Contains(sent, "Check your e-mail") {
		t.Fatalf("forgot: %d\n%s", code, sent)
	}
	mails := fm.waitFor(1)
	if len(mails) != 1 || mails[0].to != "alice@example.com" {
		t.Fatalf("mails after the request: %+v", mails)
	}
	if !strings.Contains(mails[0].body, "https://dexa.example.com/password/reset?t=") || strings.Contains(mails[0].body, "evil.example") {
		t.Fatalf("link in the e-mail:\n%s", mails[0].body)
	}
	token := resetTokenIn(t, mails[0].body)
	if r, _ := tfRequest(t, noRedirectClient(), "GET", ts.URL+"/password/reset?t="+token, nil); r.StatusCode != 200 {
		t.Fatalf("link from the e-mail: %d", r.StatusCode)
	}

	// an unknown address, a provider-only account and a malformed address get the same answer, and no mail
	for _, email := range []string{"nobody@example.com", "pat@example.com", "not an address"} {
		if c, page := forgotPOST(t, ts, "", email); c != 200 || page != sent {
			t.Fatalf("%q answered differently: %d", email, c)
		}
	}

	// the fourth request for one address within the window sends nothing
	for i := range 4 {
		if c, page := forgotPOST(t, ts, "", "dana@example.com"); c != 200 || page != sent {
			t.Fatalf("request %d for dana: %d", i+1, c)
		}
	}
	fm.waitFor(4)
	time.Sleep(200 * time.Millisecond) // let the background jobs of the last requests finish
	mails = fm.all()
	toDana := 0
	for _, m := range mails {
		if m.to == "dana@example.com" {
			toDana++
		}
	}
	if toDana != 3 || len(mails) != 4 { // alice's mail, then three for dana; the fourth request sent nothing
		t.Fatalf("%d mails in all, %d to dana; want 4 and 3", len(mails), toDana)
	}
}

func TestForgotPasswordNeedsSettings(t *testing.T) {
	ts, admin, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, admin, "root")
	fm := &fakeMailer{}
	forgot := ts.URL + "/password/forgot"

	off := func(step string) {
		t.Helper()
		if r, _ := tfRequest(t, noRedirectClient(), "GET", forgot, nil); r.StatusCode != 404 {
			t.Fatalf("%s: GET forgot %d", step, r.StatusCode)
		}
		if r, _ := tfRequest(t, noRedirectClient(), "POST", forgot, url.Values{"email": {"root@example.com"}}); r.StatusCode != 404 {
			t.Fatalf("%s: POST forgot %d", step, r.StatusCode)
		}
		if _, page := body(t, noRedirectClient(), ts.URL+"/"); strings.Contains(page, "Forgot your password?") {
			t.Fatalf("%s: the sign-in page links to the reset", step)
		}
	}
	on := func(step string) {
		t.Helper()
		if r, _ := tfRequest(t, noRedirectClient(), "GET", forgot, nil); r.StatusCode != 200 {
			t.Fatalf("%s: GET forgot %d", step, r.StatusCode)
		}
		if _, page := body(t, noRedirectClient(), ts.URL+"/"); !strings.Contains(page, "Forgot your password?") {
			t.Fatalf("%s: the sign-in page has no reset link", step)
		}
	}

	off("nothing set")

	l := *srv.cfg() // a mailer without a base URL
	l.mailer = fm
	srv.swapSettings(&l)
	off("no base URL")

	enableSelfService(t, srv, fm) // both
	on("both set")

	// Each swap gets a variable of its own: swapSettings publishes the pointer, so reusing one
	// variable would write to a config that a worker request may be reading.
	noMailer := *srv.cfg() // a base URL without a mailer
	noMailer.mailer = nil
	srv.swapSettings(&noMailer)
	off("no mailer")

	again := *srv.cfg()
	again.mailer = fm
	srv.swapSettings(&again)
	on("both set again")
}

func TestResetRequiresTwoFactor(t *testing.T) {
	ts, admin, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, admin, "root")
	bob := noRedirectClient()
	signup(t, ts, bob, "bob") // user 2
	secret, codes := tfEnable(t, srv, 2)
	token, _ := makeResetLink(t, ts, admin, 2)
	fresh := noRedirectClient()

	if _, page := tfRequest(t, fresh, "GET", ts.URL+"/password/reset?t="+token, nil); !strings.Contains(page, `name="code"`) {
		t.Fatalf("form without the code field:\n%s", page)
	}

	const next = "a new password for bob"
	form := resetForm(token, next)
	if r, _ := resetPOST(t, fresh, ts.URL, form); r.StatusCode != 401 || tfHasSession(r) {
		t.Fatalf("no code: %d, session %v", r.StatusCode, tfHasSession(r))
	}
	form.Set("code", tfWrong(t, secret))
	if r, _ := resetPOST(t, fresh, ts.URL, form); r.StatusCode != 401 || tfHasSession(r) {
		t.Fatalf("wrong code: %d, session %v", r.StatusCode, tfHasSession(r))
	}

	// the refused resets changed nothing: the old password still asks for the code, the new one does not work
	if r := signInWith(t, noRedirectClient(), ts.URL, "bob@example.com", testPassword); r.StatusCode != 303 || r.Header.Get("Location") != "/login/2fa" {
		t.Fatalf("old password after refused resets: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := signInWith(t, noRedirectClient(), ts.URL, "bob@example.com", next); r.StatusCode != 401 {
		t.Fatalf("new password after refused resets: %d", r.StatusCode)
	}
	if r, _ := tfRequest(t, noRedirectClient(), "GET", ts.URL+"/password/reset?t="+token, nil); r.StatusCode != 200 {
		t.Fatalf("link after refused resets: %d", r.StatusCode)
	}

	// a recovery code completes the reset and signs in without a second prompt
	form.Set("code", codes[0])
	r, _ := resetPOST(t, fresh, ts.URL, form)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/u/2" || !tfHasSession(r) {
		t.Fatalf("right recovery code: %d %s, session %v", r.StatusCode, r.Header.Get("Location"), tfHasSession(r))
	}
	signedIn := noRedirectClient()
	useSession(t, signedIn, r)
	if code, _ := body(t, signedIn, ts.URL+"/u/2/settings"); code != 200 {
		t.Fatalf("page after the reset: %d", code)
	}

	// the password is the new one, it still asks for a code, and the link is used up
	if r := signInWith(t, noRedirectClient(), ts.URL, "bob@example.com", next); r.StatusCode != 303 || r.Header.Get("Location") != "/login/2fa" {
		t.Fatalf("new password after the reset: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := tfRequest(t, noRedirectClient(), "GET", ts.URL+"/password/reset?t="+token, nil); r.StatusCode != 404 {
		t.Fatalf("used link: %d", r.StatusCode)
	}
}

func TestResetLinkDiesWhenPasswordOrEmailChanges(t *testing.T) {
	ts, admin, st := newAuthServer(t)
	ctx := context.Background()
	signup(t, ts, admin, "root")
	bob := noRedirectClient()
	signup(t, ts, bob, "bob") // user 2

	// the user changes the password: a link made before does not set it again
	old, _ := makeResetLink(t, ts, admin, 2)
	const own = "bob's own password"
	r, _ := tfRequest(t, bob, "POST", ts.URL+"/u/2/password", url.Values{"current": {testPassword}, "password": {own}, "confirm": {own}})
	if r.StatusCode != 200 {
		t.Fatalf("change password: %d", r.StatusCode)
	}
	useSession(t, bob, r) // the change ended the old session and started a new one here
	if _, err := st.PasswordResetUser(ctx, hashToken(old)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("link survived a password change: %v", err)
	}
	if r, _ := resetPOST(t, noRedirectClient(), ts.URL, resetForm(old, "the link's password")); r.StatusCode != 404 {
		t.Fatalf("link used after a password change: %d", r.StatusCode)
	}
	if r := signInWith(t, noRedirectClient(), ts.URL, "bob@example.com", "the link's password"); r.StatusCode != 401 {
		t.Fatalf("password set by a dead link: %d", r.StatusCode)
	}

	// a new e-mail address: a link made for the old one stops working
	fresh, _ := makeResetLink(t, ts, admin, 2)
	r, _ = tfRequest(t, bob, "POST", ts.URL+"/u/2/profile", url.Values{"username": {"bob"}, "email": {"bob.new@example.com"}, "current": {own}})
	if r.StatusCode != 303 {
		t.Fatalf("change e-mail address: %d", r.StatusCode)
	}
	if _, err := st.PasswordResetUser(ctx, hashToken(fresh)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("link survived an e-mail change: %v", err)
	}
}

func TestResetForcedEnrolment(t *testing.T) {
	ts, admin, _ := newAuthServer(t)
	signup(t, ts, admin, "root")
	bob := noRedirectClient()
	signup(t, ts, bob, "bob") // user 2, no two-step verification yet
	if r, _ := tfRequest(t, admin, "POST", ts.URL+"/admin/two-factor", url.Values{"require": {"everyone"}}); r.StatusCode != 303 {
		t.Fatalf("set policy: %d", r.StatusCode)
	}
	token, _ := makeResetLink(t, ts, admin, 2)

	const next = "a new password for bob"
	fresh := noRedirectClient()
	r, _ := resetPOST(t, fresh, ts.URL, resetForm(token, next))
	if r.StatusCode != 303 || r.Header.Get("Location") != "/login/2fa/setup" || tfHasSession(r) {
		t.Fatalf("reset under a required policy: %d %s, session %v", r.StatusCode, r.Header.Get("Location"), tfHasSession(r))
	}
	mfa := tfMFACookie(t, r)
	if r, page := tfRequest(t, fresh, "GET", ts.URL+"/login/2fa/setup", nil, mfa); r.StatusCode != 200 || !strings.Contains(page, "<svg") {
		t.Fatalf("setup page: %d", r.StatusCode)
	}

	// the password was set all the same, and the next sign-in is sent to the setup too
	if r := signInWith(t, noRedirectClient(), ts.URL, "bob@example.com", next); r.StatusCode != 303 || r.Header.Get("Location") != "/login/2fa/setup" {
		t.Fatalf("sign-in after the reset: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
}
