package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// tfRequest sends one request with the given cookies and returns the response and its body. A nil
// form sends no body; a non-nil one is posted as a form.
func tfRequest(t *testing.T, c *http.Client, method, u string, form url.Values, cookies ...*http.Cookie) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if form != nil {
		rd = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, ck := range cookies {
		if ck != nil {
			req.AddCookie(ck)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// tfPasswordLogin is the first step of a sign-in with a password, from a client that holds no cookies.
func tfPasswordLogin(t *testing.T, c *http.Client, base, email string) *http.Response {
	t.Helper()
	resp, _ := tfRequest(t, c, "POST", base+"/login", url.Values{"email": {email}, "password": {testPassword}})
	return resp
}

// tfMFACookie returns the cookie that carries a password sign-in to the second step.
func tfMFACookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, ck := range resp.Cookies() {
		if ck.Name == mfaCookie && ck.MaxAge > 0 {
			return ck
		}
	}
	t.Fatalf("no second-step cookie in %v", resp.Cookies())
	return nil
}

// tfHasSession reports whether the response starts a session.
func tfHasSession(resp *http.Response) bool {
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie && ck.MaxAge > 0 {
			return true
		}
	}
	return false
}

// tfSecondStep submits a code (or a recovery code) to the second step of a sign-in.
func tfSecondStep(t *testing.T, c *http.Client, base string, mfa *http.Cookie, code string) (*http.Response, string) {
	t.Helper()
	return tfRequest(t, c, "POST", base+"/login/2fa", url.Values{"code": {code}}, mfa)
}

// tfEnable turns two-step verification on for a user directly in the store, the way a finished
// setup leaves it. The confirmed step is set well in the past, so the code for the current step is
// still unused and the test can sign in with it. It returns the secret and the recovery codes.
func tfEnable(t *testing.T, srv *Server, uid int64) (string, []string) {
	t.Helper()
	ctx := context.Background()
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.PutPendingTOTP(ctx, uid, secret); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.ConfirmTOTP(ctx, uid, totpStep(time.Now())-100); err != nil {
		t.Fatal(err)
	}
	codes, err := srv.issueRecoveryCodes(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	return secret, codes
}

// tfCode is the code an authenticator app shows now, for the secret the server stored.
func tfCode(t *testing.T, srv *Server, uid int64) string {
	t.Helper()
	tot, err := srv.store.GetTOTP(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	code, err := totpCode(tot.Secret, totpStep(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// tfWrong returns a six-digit code that is not accepted now.
func tfWrong(t *testing.T, secret string) string {
	t.Helper()
	for i := 0; i < 1000000; i++ {
		c := fmt.Sprintf("%06d", i)
		if _, ok := checkTOTP(secret, c, time.Now()); !ok {
			return c
		}
	}
	t.Fatal("no wrong code found")
	return ""
}

// tfRecoveryCodes reads the recovery codes shown on a page.
func tfRecoveryCodes(t *testing.T, page string) []string {
	t.Helper()
	start := strings.Index(page, `<ul class="recovery-codes">`)
	if start < 0 {
		t.Fatalf("no recovery codes on the page:\n%s", page)
	}
	list := page[start:]
	end := strings.Index(list, "</ul>")
	if end < 0 {
		t.Fatalf("recovery code list is not closed:\n%s", page)
	}
	var codes []string
	for _, part := range strings.Split(list[:end], "<code>")[1:] {
		codes = append(codes, part[:strings.Index(part, "</code>")])
	}
	return codes
}

func TestEnableTwoFactorFromSettings(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	srv := serverOf(ts)
	ctx := context.Background()
	signup(t, ts, c, "alice") // user 1, signed in with a password
	begin := ts.URL + "/u/1/2fa/begin"

	if resp, _ := tfRequest(t, c, "POST", begin, url.Values{}); resp.StatusCode != 422 {
		t.Fatalf("setup without the current password: %d", resp.StatusCode)
	}
	if _, err := srv.store.GetTOTP(ctx, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("setup started without the current password: %v", err)
	}

	resp, page := tfRequest(t, c, "POST", begin, url.Values{"current": {testPassword}})
	if resp.StatusCode != 200 || !strings.Contains(page, "<svg") || !strings.Contains(page, "Enter this key") {
		t.Fatalf("begin: %d\n%s", resp.StatusCode, page)
	}
	pending, err := srv.store.GetTOTP(ctx, 1)
	if err != nil || pending.Confirmed {
		t.Fatalf("pending setup: %+v %v", pending, err)
	}

	confirm := ts.URL + "/u/1/2fa/confirm"
	if resp, _ := tfRequest(t, c, "POST", confirm, url.Values{"code": {tfWrong(t, pending.Secret)}}); resp.StatusCode != 422 {
		t.Fatalf("wrong code: %d", resp.StatusCode)
	}
	if still, err := srv.store.GetTOTP(ctx, 1); err != nil || still.Confirmed {
		t.Fatalf("a wrong code confirmed the setup: %+v %v", still, err)
	}

	resp, page = tfRequest(t, c, "POST", confirm, url.Values{"code": {tfCode(t, srv, 1)}})
	if resp.StatusCode != 200 {
		t.Fatalf("confirm: %d\n%s", resp.StatusCode, page)
	}
	if codes := tfRecoveryCodes(t, page); len(codes) != 10 {
		t.Fatalf("got %d recovery codes on the page, want 10", len(codes))
	}
	if done, err := srv.store.GetTOTP(ctx, 1); err != nil || !done.Confirmed {
		t.Fatalf("setup not confirmed: %+v %v", done, err)
	}
}

func TestPasswordSignInAsksForSecondStep(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, c, "alice")
	secret, _ := tfEnable(t, srv, 1)

	fresh := noRedirectClient()
	resp := tfPasswordLogin(t, fresh, ts.URL, "alice@example.com")
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/login/2fa" {
		t.Fatalf("password sign-in: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if tfHasSession(resp) {
		t.Fatal("a password alone started a session")
	}
	mfa := tfMFACookie(t, resp)
	if r, _ := tfRequest(t, fresh, "GET", ts.URL+"/u/1", nil); r.StatusCode != 303 || r.Header.Get("Location") != "/" {
		t.Fatalf("signed in before the second step: %d %s", r.StatusCode, r.Header.Get("Location"))
	}

	if r, _ := tfSecondStep(t, fresh, ts.URL, mfa, tfWrong(t, secret)); r.StatusCode != 401 || tfHasSession(r) {
		t.Fatalf("wrong code: %d, session %v", r.StatusCode, tfHasSession(r))
	}

	code := tfCode(t, srv, 1)
	r, _ := tfSecondStep(t, fresh, ts.URL, mfa, code)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/u/1" || !tfHasSession(r) {
		t.Fatalf("right code: %d %s, session %v", r.StatusCode, r.Header.Get("Location"), tfHasSession(r))
	}
	useSession(t, fresh, r)
	if status, _ := body(t, fresh, ts.URL+"/u/1/settings"); status != 200 {
		t.Fatalf("page after the second step: %d", status)
	}

	// the same code cannot be used for a second sign-in: its step is recorded
	again := noRedirectClient()
	resp = tfPasswordLogin(t, again, ts.URL, "alice@example.com")
	if resp.StatusCode != 303 {
		t.Fatalf("second password sign-in: %d", resp.StatusCode)
	}
	if r, _ := tfSecondStep(t, again, ts.URL, tfMFACookie(t, resp), code); r.StatusCode != 401 || tfHasSession(r) {
		t.Fatalf("replayed code: %d, session %v", r.StatusCode, tfHasSession(r))
	}
}

func TestRecoveryCodeSignIn(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	srv := serverOf(ts)
	ctx := context.Background()
	signup(t, ts, c, "alice")
	_, codes := tfEnable(t, srv, 1)

	fresh := noRedirectClient()
	mfa := tfMFACookie(t, tfPasswordLogin(t, fresh, ts.URL, "alice@example.com"))
	r, _ := tfSecondStep(t, fresh, ts.URL, mfa, codes[0])
	if r.StatusCode != 303 || r.Header.Get("Location") != "/u/1" || !tfHasSession(r) {
		t.Fatalf("recovery code: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	useSession(t, fresh, r)
	if code, _ := body(t, fresh, ts.URL+"/u/1/settings"); code != 200 {
		t.Fatalf("page after a recovery code: %d", code)
	}
	if left, err := srv.store.CountRecoveryCodes(ctx, 1); err != nil || left != 9 {
		t.Fatalf("codes left: %d %v, want 9", left, err)
	}

	// a used code is gone
	again := noRedirectClient()
	mfa = tfMFACookie(t, tfPasswordLogin(t, again, ts.URL, "alice@example.com"))
	if r, _ := tfSecondStep(t, again, ts.URL, mfa, codes[0]); r.StatusCode != 401 || tfHasSession(r) {
		t.Fatalf("used code: %d, session %v", r.StatusCode, tfHasSession(r))
	}
	if left, _ := srv.store.CountRecoveryCodes(ctx, 1); left != 9 {
		t.Fatalf("codes left after a refused code: %d, want 9", left)
	}

	// upper case, without dashes, also works
	typed := strings.ToUpper(strings.ReplaceAll(codes[1], "-", ""))
	third := noRedirectClient()
	mfa = tfMFACookie(t, tfPasswordLogin(t, third, ts.URL, "alice@example.com"))
	if r, _ := tfSecondStep(t, third, ts.URL, mfa, typed); r.StatusCode != 303 || r.Header.Get("Location") != "/u/1" {
		t.Fatalf("typed recovery code %q: %d %s", typed, r.StatusCode, r.Header.Get("Location"))
	}
	if left, _ := srv.store.CountRecoveryCodes(ctx, 1); left != 8 {
		t.Fatalf("codes left: %d, want 8", left)
	}
}

func TestSecondStepCookieIsNotReplayable(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, c, "alice")
	_, codes := tfEnable(t, srv, 1)

	first := noRedirectClient()
	mfa := tfMFACookie(t, tfPasswordLogin(t, first, ts.URL, "alice@example.com"))
	if r, _ := tfSecondStep(t, first, ts.URL, mfa, tfCode(t, srv, 1)); r.StatusCode != 303 || r.Header.Get("Location") != "/u/1" {
		t.Fatalf("first completion: %d %s", r.StatusCode, r.Header.Get("Location"))
	}

	// the same cookie again, with a valid recovery code: the cookie was already used
	other := noRedirectClient()
	r, _ := tfSecondStep(t, other, ts.URL, mfa, codes[0])
	if r.StatusCode != 303 || r.Header.Get("Location") != "/" || tfHasSession(r) {
		t.Fatalf("replayed cookie: %d %s, session %v", r.StatusCode, r.Header.Get("Location"), tfHasSession(r))
	}
	if r, _ := tfRequest(t, other, "GET", ts.URL+"/u/1/settings", nil); r.StatusCode != 303 {
		t.Fatalf("signed in through a replayed cookie: %d", r.StatusCode)
	}
}

func TestSecondStepThrottle(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, c, "alice")
	secret, _ := tfEnable(t, srv, 1)

	fresh := noRedirectClient()
	mfa := tfMFACookie(t, tfPasswordLogin(t, fresh, ts.URL, "alice@example.com"))
	for i := 0; i < 5; i++ {
		if r, _ := tfSecondStep(t, fresh, ts.URL, mfa, tfWrong(t, secret)); r.StatusCode != 401 {
			t.Fatalf("wrong code %d: %d, want 401", i+1, r.StatusCode)
		}
	}
	if r, _ := tfSecondStep(t, fresh, ts.URL, mfa, tfCode(t, srv, 1)); r.StatusCode != 429 || tfHasSession(r) {
		t.Fatalf("right code after 5 wrong ones: %d, session %v, want 429 and no session", r.StatusCode, tfHasSession(r))
	}
}

func TestMFACookieNeededForSecondStep(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, c, "alice")
	tfEnable(t, srv, 1)
	fresh := noRedirectClient()

	if r, _ := tfRequest(t, fresh, "GET", ts.URL+"/login/2fa", nil); r.StatusCode != 303 || r.Header.Get("Location") != "/" {
		t.Fatalf("second step without a cookie: %d %s", r.StatusCode, r.Header.Get("Location"))
	}

	// a valid signature for another purpose must not open the second step
	sealed, _, err := srv.sealValue("provider", mfaState{UID: 1}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	foreign := &http.Cookie{Name: mfaCookie, Value: sealed}
	if r, _ := tfRequest(t, fresh, "GET", ts.URL+"/login/2fa", nil, foreign); r.StatusCode != 303 || r.Header.Get("Location") != "/" {
		t.Fatalf("GET with another purpose: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := tfSecondStep(t, fresh, ts.URL, foreign, tfCode(t, srv, 1)); r.StatusCode != 303 || r.Header.Get("Location") != "/" || tfHasSession(r) {
		t.Fatalf("POST with another purpose: %d %s, session %v", r.StatusCode, r.Header.Get("Location"), tfHasSession(r))
	}
}

func TestDisableAndRecoveryRegenerate(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	srv := serverOf(ts)
	ctx := context.Background()
	signup(t, ts, c, "alice")
	secret, oldCodes := tfEnable(t, srv, 1)
	disable := ts.URL + "/u/1/2fa/disable"
	stillOn := func(step string) {
		t.Helper()
		if tot, err := srv.store.GetTOTP(ctx, 1); err != nil || !tot.Confirmed {
			t.Fatalf("%s: two-step verification is off: %+v %v", step, tot, err)
		}
	}

	if r, _ := tfRequest(t, c, "POST", disable, url.Values{"current": {"not the password"}, "code": {tfCode(t, srv, 1)}}); r.StatusCode != 422 {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	stillOn("wrong password")
	if r, _ := tfRequest(t, c, "POST", disable, url.Values{"current": {testPassword}, "code": {tfWrong(t, secret)}}); r.StatusCode != 422 {
		t.Fatalf("wrong code: %d", r.StatusCode)
	}
	stillOn("wrong code")

	// regenerating the recovery codes takes the current code; it replaces the set
	r, page := tfRequest(t, c, "POST", ts.URL+"/u/1/2fa/recovery", url.Values{"current": {testPassword}, "code": {tfCode(t, srv, 1)}})
	if r.StatusCode != 200 {
		t.Fatalf("regenerate: %d", r.StatusCode)
	}
	newCodes := tfRecoveryCodes(t, page)
	if len(newCodes) != 10 {
		t.Fatalf("got %d new codes, want 10", len(newCodes))
	}
	for _, n := range newCodes {
		for _, o := range oldCodes {
			if n == o {
				t.Fatalf("a new recovery code is an old one: %s", n)
			}
		}
	}
	stillOn("regenerate")

	old := noRedirectClient()
	if r, _ := tfSecondStep(t, old, ts.URL, tfMFACookie(t, tfPasswordLogin(t, old, ts.URL, "alice@example.com")), oldCodes[0]); r.StatusCode != 401 {
		t.Fatalf("old recovery code after regenerating: %d", r.StatusCode)
	}
	fresh := noRedirectClient()
	if r, _ := tfSecondStep(t, fresh, ts.URL, tfMFACookie(t, tfPasswordLogin(t, fresh, ts.URL, "alice@example.com")), newCodes[0]); r.StatusCode != 303 {
		t.Fatalf("new recovery code: %d", r.StatusCode)
	}

	// turning it off takes the password and a valid code; a recovery code will do
	r, _ = tfRequest(t, c, "POST", disable, url.Values{"current": {testPassword}, "code": {newCodes[1]}})
	if r.StatusCode != 303 {
		t.Fatalf("disable: %d", r.StatusCode)
	}
	if _, err := srv.store.GetTOTP(ctx, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("setup still there after disabling: %v", err)
	}
}

func TestRequirePolicy(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	srv := serverOf(ts)
	ctx := context.Background()
	base := ts.URL
	signup(t, ts, c, "root") // user 1, administrator
	bob := noRedirectClient()
	signup(t, ts, bob, "bob") // user 2, signed up before the policy
	carol := noRedirectClient()
	signup(t, ts, carol, "carol") // user 3, signed up before the policy

	// everyone: a sign-in without two-step verification is sent to set it up
	if r, _ := tfRequest(t, c, "POST", base+"/admin/two-factor", url.Values{"require": {"everyone"}}); r.StatusCode != 303 {
		t.Fatalf("set policy: %d", r.StatusCode)
	}
	if v, err := srv.store.GetSetting(ctx, settingRequire2FA); err != nil || v != require2FAEveryone {
		t.Fatalf("policy stored as %q %v", v, err)
	}

	fresh := noRedirectClient()
	resp := tfPasswordLogin(t, fresh, base, "bob@example.com")
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/login/2fa/setup" || tfHasSession(resp) {
		t.Fatalf("required sign-in: %d %s, session %v", resp.StatusCode, resp.Header.Get("Location"), tfHasSession(resp))
	}
	mfa := tfMFACookie(t, resp)
	if r, page := tfRequest(t, fresh, "GET", base+"/login/2fa/setup", nil, mfa); r.StatusCode != 200 || !strings.Contains(page, "<svg") {
		t.Fatalf("setup page: %d", r.StatusCode)
	}
	if r, _ := tfRequest(t, fresh, "POST", base+"/login/2fa/setup", url.Values{"code": {tfWrong(t, pendingSecret(t, srv, 2))}}, mfa); r.StatusCode != 422 {
		t.Fatalf("wrong setup code: %d", r.StatusCode)
	}
	r, page := tfRequest(t, fresh, "POST", base+"/login/2fa/setup", url.Values{"code": {tfCode(t, srv, 2)}}, mfa)
	if r.StatusCode != 200 || len(tfRecoveryCodes(t, page)) != 10 {
		t.Fatalf("setup: %d", r.StatusCode)
	}
	if !tfHasSession(r) {
		t.Fatal("finishing the setup did not start a session")
	}
	useSession(t, fresh, r)
	if r, _ := tfRequest(t, fresh, "POST", base+"/u/2/2fa/disable", url.Values{"current": {testPassword}}); r.StatusCode != 403 {
		t.Fatalf("disable under a required policy: %d", r.StatusCode)
	}

	// admins: only administrators are asked; bob keeps his, carol has none and signs in normally
	if r, _ := tfRequest(t, c, "POST", base+"/admin/two-factor", url.Values{"require": {"admins"}}); r.StatusCode != 303 {
		t.Fatalf("set admins: %d", r.StatusCode)
	}
	carolFresh := noRedirectClient()
	if r := tfPasswordLogin(t, carolFresh, base, "carol@example.com"); r.StatusCode != 303 || r.Header.Get("Location") != "/u/3" || !tfHasSession(r) {
		t.Fatalf("non-admin sign-in: %d %s, session %v", r.StatusCode, r.Header.Get("Location"), tfHasSession(r))
	}
	rootFresh := noRedirectClient()
	if r := tfPasswordLogin(t, rootFresh, base, "root@example.com"); r.Header.Get("Location") != "/login/2fa/setup" {
		t.Fatalf("administrator sign-in under admins: %s", r.Header.Get("Location"))
	}

	// an unknown value changes nothing
	if r, _ := tfRequest(t, c, "POST", base+"/admin/two-factor", url.Values{"require": {"bogus"}}); !strings.Contains(r.Header.Get("Location"), "Choose+who") {
		t.Fatalf("invalid value: %s", r.Header.Get("Location"))
	}
	if v, _ := srv.store.GetSetting(ctx, settingRequire2FA); v != require2FAAdmins {
		t.Fatalf("invalid value changed the policy to %q", v)
	}
}

// pendingSecret returns the secret of a setup that is not confirmed yet.
func pendingSecret(t *testing.T, srv *Server, uid int64) string {
	t.Helper()
	tot, err := srv.store.GetTOTP(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return tot.Secret
}

func TestOIDCSignInSkipsSecondStep(t *testing.T) {
	idp := newFakeIdP(t)
	ts, admin, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, admin, "root") // user 1
	addProvider(t, ts, admin, idp, false)
	if r := idp.signInVia(t, ts, admin, "idp"); r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), "linked") {
		t.Fatalf("link: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	tfEnable(t, srv, 1)

	// the password still asks for the second step
	if r := tfPasswordLogin(t, noRedirectClient(), ts.URL, "root@example.com"); r.Header.Get("Location") != "/login/2fa" {
		t.Fatalf("password sign-in: %s", r.Header.Get("Location"))
	}

	// the provider signs the user in directly
	c := noRedirectClient()
	r := idp.signInVia(t, ts, c, "idp")
	if r.StatusCode != 303 || r.Header.Get("Location") != "/u/1" || !tfHasSession(r) {
		t.Fatalf("provider sign-in: %d %s, session %v", r.StatusCode, r.Header.Get("Location"), tfHasSession(r))
	}
	useSession(t, c, r)
	if code, _ := body(t, c, ts.URL+"/u/1/settings"); code != 200 {
		t.Fatalf("page after provider sign-in: %d", code)
	}
}

func TestBasicAuthRefusedWithTwoFactor(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, c, "alice")
	token := createPAT(t, ts, c, 1, url.Values{"name": {"script"}, "expires": {"never"}})

	basic := http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte("alice@example.com:"+testPassword))}}
	api := ts.URL + "/api/v1/users/1/settings"
	if code, _ := apiCall(t, noRedirectClient(), "GET", api, "", basic); code != 200 {
		t.Fatalf("basic before two-step verification: %d", code)
	}

	tfEnable(t, srv, 1)
	if code, _ := apiCall(t, noRedirectClient(), "GET", api, "", basic); code != 401 {
		t.Fatalf("basic with two-step verification: %d", code)
	}
	if code, _ := apiCall(t, noRedirectClient(), "GET", api, "", bearer(token)); code != 200 {
		t.Fatalf("access token with two-step verification: %d", code)
	}
}

func TestAdminRemovesTwoFactor(t *testing.T) {
	ts, admin, st := newAuthServer(t)
	srv := serverOf(ts)
	ctx := context.Background()
	signup(t, ts, admin, "root") // user 1, administrator
	bob := noRedirectClient()
	signup(t, ts, bob, "bob") // user 2
	tfEnable(t, srv, 2)
	remove := ts.URL + "/admin/users/2/2fa/delete"

	if r, _ := tfRequest(t, bob, "POST", remove, url.Values{}); r.StatusCode != 404 {
		t.Fatalf("non-admin: %d", r.StatusCode)
	}
	if _, err := srv.store.GetTOTP(ctx, 2); err != nil {
		t.Fatalf("non-admin removed the setup: %v", err)
	}

	if r, _ := tfRequest(t, admin, "POST", remove, url.Values{}); r.StatusCode != 303 {
		t.Fatalf("admin remove: %d", r.StatusCode)
	}
	if _, err := srv.store.GetTOTP(ctx, 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("setup still there: %v", err)
	}
	events, err := st.ListAuditEvents(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Action == auditTOTPRemovedByAdmin && e.TargetID != nil && *e.TargetID == 2 && e.ActorID != nil && *e.ActorID == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s event for user 2 by user 1 in %+v", auditTOTPRemovedByAdmin, events)
	}
}

func TestTwoFactorNeedsPasswordAccount(t *testing.T) {
	idp := newFakeIdP(t)
	ts, admin, _ := newAuthServer(t)
	signup(t, ts, admin, "root")
	addProvider(t, ts, admin, idp, true)

	// the account is made by the provider, so it has no password to protect
	c := noRedirectClient()
	r := idp.signInVia(t, ts, c, "idp")
	if r.StatusCode != 303 || r.Header.Get("Location") != "/u/2" {
		t.Fatalf("provider sign-up: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	useSession(t, c, r)

	if resp, _ := tfRequest(t, c, "POST", ts.URL+"/u/2/2fa/begin", url.Values{}); resp.StatusCode != 422 {
		t.Fatalf("setup for a provider-only account: %d", resp.StatusCode)
	}
}
