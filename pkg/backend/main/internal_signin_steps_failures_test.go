package mainbackend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// withTwoFactor makes an account with two-step verification on and starts a password sign-in, which
// stops at the second step. It returns the cookie that carries the sign-in to that step.
func (g *rig) withTwoFactor(name string) (uid int64, mfa *http.Cookie) {
	g.t.Helper()
	uid, _ = g.signUp(name)
	tfEnable(g.t, g.srv, uid)
	return uid, g.mfaCookieOf(name)
}

// mfaCookieOf signs in with the password and returns the second-step cookie.
func (g *rig) mfaCookieOf(name string) *http.Cookie {
	g.t.Helper()
	rep := g.signIn(name)
	ck := rep.cookie(mfaCookie)
	if ck == nil || rep.cookie(sessionCookie) != nil {
		g.t.Fatalf("the password step should stop at the second step: %s", rep.body())
	}
	return ck
}

// needingSetup makes an account that the policy forces to set up two-step verification and starts
// its sign-in. It returns the cookie that carries it to the setup.
func (g *rig) needingSetup(name string) (uid int64, mfa *http.Cookie) {
	g.t.Helper()
	uid, _ = g.signUp(name)
	if err := g.st.PutSetting(context.Background(), settingRequire2FA, require2FAEveryone); err != nil {
		g.t.Fatal(err)
	}
	return uid, g.mfaCookieOf(name)
}

func wantStatus(t *testing.T, rep reply, status int, what string) {
	t.Helper()
	if rep.Code != status {
		t.Fatalf("%s: status %d, want %d\n%s", what, rep.Code, status, rep.body())
	}
}

// wantRedirect checks an answer that sends the visitor on, without a session.
func wantRedirect(t *testing.T, rep reply, target, what string) {
	t.Helper()
	var out apiclient.SecondStep
	rep.decode(t, &out)
	if out.Redirect != target || rep.cookie(sessionCookie) != nil {
		t.Fatalf("%s: redirect %q (want %q), session cookie %v", what, out.Redirect, target, rep.cookie(sessionCookie))
	}
}

func TestEveryStepOfSigningInRefusesAGarbledBody(t *testing.T) {
	g := newRig(t)
	for _, path := range []string{
		"/auth/2fa", "/auth/2fa/setup", "/auth/password/forgot", "/auth/password/reset",
		"/auth/providers/x/start", "/auth/providers/x/callback", "/auth/password", "/auth/signup",
	} {
		t.Run(path, func(t *testing.T) {
			rep := g.do("POST", path, "this is not JSON")
			wantStatus(t, rep, http.StatusBadRequest, "garbled body")
			if got := rep.code(t); got != apiclient.CodeInvalid {
				t.Fatalf("error code %q, want %q", got, apiclient.CodeInvalid)
			}
		})
	}
}

func TestTheSecondStepRefusesWhatIsNotAFreshCookieWithAGoodCode(t *testing.T) {
	g := newRig(t)
	uid, mfa := g.withTwoFactor("alice")
	good := tfCode(t, g.srv, uid)
	ctx := context.Background()

	t.Run("without the cookie it sends the visitor back to the start", func(t *testing.T) {
		wantRedirect(t, g.do("POST", "/auth/2fa", map[string]string{"Code": good}), "/", "no cookie")
	})
	t.Run("a cookie made for another purpose is not accepted", func(t *testing.T) {
		forged := &http.Cookie{Name: mfaCookie, Value: "a.b"}
		wantRedirect(t, g.do("POST", "/auth/2fa", map[string]string{"Code": good}, forged), "/", "forged cookie")
	})
	t.Run("a cookie for the setup step does not open the verify step", func(t *testing.T) {
		sealed, _, err := g.b.sealValue(sealMFA, mfaState{UID: uid, Method: "local", Enrol: true}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		wantRedirect(t, g.do("POST", "/auth/2fa", map[string]string{"Code": good}, &http.Cookie{Name: mfaCookie, Value: sealed}), "/", "setup cookie")
	})
	t.Run("an expired cookie is not accepted", func(t *testing.T) {
		sealed, _, err := g.b.sealValue(sealMFA, mfaState{UID: uid, Method: "local"}, -time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		wantRedirect(t, g.do("POST", "/auth/2fa", map[string]string{"Code": good}, &http.Cookie{Name: mfaCookie, Value: sealed}), "/", "expired cookie")
	})
	t.Run("a wrong code is refused, counted and audited", func(t *testing.T) {
		rep := g.do("POST", "/auth/2fa", map[string]string{"Code": tfWrong(t, mustSecret(t, g, uid))}, mfa)
		var out apiclient.SecondStep
		rep.decode(t, &out)
		if out.Status != http.StatusUnauthorized || out.Error != secondFactorMessage || rep.cookie(sessionCookie) != nil {
			t.Fatalf("wrong code: %+v", out)
		}
		if !g.audited(auditSecondFactorFailed) {
			t.Fatal("a wrong code was not audited")
		}
	})
	t.Run("too many wrong codes lock the account out even for the right one", func(t *testing.T) {
		for i := 0; i < maxMFAFail; i++ {
			g.b.limiter.Fail(ctx, mfaKey(uid))
		}
		rep := g.do("POST", "/auth/2fa", map[string]string{"Code": good}, mfa)
		var out apiclient.SecondStep
		rep.decode(t, &out)
		if out.Status != http.StatusTooManyRequests || !strings.Contains(out.Error, "Too many wrong codes") || rep.cookie(sessionCookie) != nil {
			t.Fatalf("locked out: %+v", out)
		}
		if !g.audited(auditSecondFactorThrottled) {
			t.Fatal("the lock-out was not audited")
		}
		g.b.limiter.Reset(ctx, mfaKey(uid))
	})
	t.Run("a locked-out address is refused too", func(t *testing.T) {
		for i := 0; i < maxIPFail; i++ {
			g.b.limiter.Fail(ctx, "ip:198.51.100.7")
		}
		rep := g.doFrom("198.51.100.7", "POST", "/auth/2fa", map[string]string{"Code": good}, mfa)
		var out apiclient.SecondStep
		rep.decode(t, &out)
		if out.Status != http.StatusTooManyRequests {
			t.Fatalf("locked-out address: %+v", out)
		}
	})
}

// mustSecret returns the secret of the account's authenticator.
func mustSecret(t *testing.T, g *rig, uid int64) string {
	t.Helper()
	tot, err := g.st.GetTOTP(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return tot.Secret
}

func TestTheSecondStepCookieSignsInOnlyOnceEvenWhenTwoRequestsRace(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	secret, _ := tfEnable(t, g.srv, uid)
	blind := newRigOn(t, blindToNonces{g.st}, g.st)
	// the same database, with a backend that cannot tell that a cookie was already used
	blind.b.cookieKey = g.b.cookieKey
	mfa := g.mfaCookieOf("alice")
	cur := totpStep(time.Now())
	first, _ := totpCode(secret, cur-1)
	second, _ := totpCode(secret, cur)

	rep := blind.do("POST", "/auth/2fa", map[string]string{"Code": first}, mfa)
	if rep.cookie(sessionCookie) == nil {
		t.Fatalf("the first use should sign in: %s", rep.body())
	}
	rep = blind.do("POST", "/auth/2fa", map[string]string{"Code": second}, mfa)
	wantRedirect(t, rep, "/", "the same cookie a second time")
}

func TestTheSecondStepFailsCleanlyWhenTheStoreLetsItDown(t *testing.T) {
	scenarios := []struct{ name, table string }{
		{"the code cannot be checked", "user_totp"},
		{"the account cannot be read", "users"},
		{"the session cannot be started", "sessions"},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			g := newRig(t)
			uid, mfa := g.withTwoFactor("alice")
			code := tfCode(t, g.srv, uid)
			breakTable(t, g.st, sc.table)
			rep := g.do("POST", "/auth/2fa", map[string]string{"Code": code}, mfa)
			wantStatus(t, rep, http.StatusInternalServerError, sc.name)
			if rep.cookie(sessionCookie) != nil {
				t.Fatal("a session was started although the store failed")
			}
		})
	}
}

func TestTheLastRecoveryCodeIsAuditedAsTheLastOne(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	_, codes := tfEnable(t, g.srv, uid)
	if err := g.st.ReplaceRecoveryCodes(context.Background(), uid, []string{recoveryHash(codes[0])}); err != nil {
		t.Fatal(err)
	}
	mfa := g.mfaCookieOf("alice")
	rep := g.do("POST", "/auth/2fa", map[string]string{"Code": codes[0]}, mfa)
	if rep.cookie(sessionCookie) == nil {
		t.Fatalf("a recovery code should sign in: %s", rep.body())
	}
	if !g.auditedWith(auditRecoveryUsed, "last") {
		t.Fatal("using the last recovery code was not audited as such")
	}
}

func TestTheSetupStepRefusesWhatIsNotAFreshCookieWithAGoodCode(t *testing.T) {
	g := newRig(t)
	uid, mfa := g.needingSetup("alice")
	ctx := context.Background()
	secret, err := g.b.pendingTOTPSecret(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	good, _ := totpCode(secret, totpStep(time.Now()))

	t.Run("the page of the setup needs the setup cookie", func(t *testing.T) {
		wantRedirect(t, g.do("GET", "/pages/login/2fa/setup", nil), "/", "no cookie")
	})
	t.Run("the verify page sends a visitor in setup to the setup", func(t *testing.T) {
		wantRedirect(t, g.do("GET", "/pages/login/2fa", nil, mfa), "/", "setup cookie on the verify page")
	})
	t.Run("without the cookie it sends the visitor back to the start", func(t *testing.T) {
		wantRedirect(t, g.do("POST", "/auth/2fa/setup", map[string]string{"Code": good}), "/", "no cookie")
	})
	t.Run("a cookie for the verify step does not open the setup", func(t *testing.T) {
		sealed, _, err := g.b.sealValue(sealMFA, mfaState{UID: uid, Method: "local"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		wantRedirect(t, g.do("POST", "/auth/2fa/setup", map[string]string{"Code": good}, &http.Cookie{Name: mfaCookie, Value: sealed}), "/", "verify cookie")
	})
	t.Run("a wrong code shows the setup again with the same secret", func(t *testing.T) {
		rep := g.do("POST", "/auth/2fa/setup", map[string]string{"Code": tfWrong(t, secret)}, mfa)
		var out apiclient.SecondStep
		rep.decode(t, &out)
		if out.Status != http.StatusUnprocessableEntity || out.Mode != "setup" || out.Secret != groupSecret(secret) {
			t.Fatalf("wrong code: %+v", out)
		}
	})
	t.Run("too many wrong codes lock the setup", func(t *testing.T) {
		for i := 0; i < maxMFAFail; i++ {
			g.b.limiter.Fail(ctx, mfaKey(uid))
		}
		rep := g.do("POST", "/auth/2fa/setup", map[string]string{"Code": good}, mfa)
		var out apiclient.SecondStep
		rep.decode(t, &out)
		if out.Status != http.StatusTooManyRequests || out.Mode != "setup" {
			t.Fatalf("locked out: %+v", out)
		}
		if !g.audited(auditSecondFactorThrottled) {
			t.Fatal("the lock-out was not audited")
		}
		g.b.limiter.Reset(ctx, mfaKey(uid))
	})
	t.Run("a setup that is already confirmed sends the visitor back", func(t *testing.T) {
		if err := g.st.ConfirmTOTP(ctx, uid, totpStep(time.Now())-100); err != nil {
			t.Fatal(err)
		}
		wantRedirect(t, g.do("POST", "/auth/2fa/setup", map[string]string{"Code": good}, mfa), "/", "confirmed")
	})
}

func TestTheSetupCookieSignsInOnlyOnceEvenWhenTwoRequestsRace(t *testing.T) {
	g := newRig(t)
	uid, mfa := g.needingSetup("alice")
	secret, err := g.b.pendingTOTPSecret(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	// a backend that cannot tell that the cookie was spent: the single-use check has to
	blind := newRigOn(t, blindToNonces{g.st}, g.st)
	blind.b.cookieKey = g.b.cookieKey
	if !g.b.nonces.use(context.Background(), mustNonce(t, g, mfa), time.Now().Add(time.Minute)) {
		t.Fatal("could not spend the nonce")
	}
	good, _ := totpCode(secret, totpStep(time.Now()))
	rep := blind.do("POST", "/auth/2fa/setup", map[string]string{"Code": good}, mfa)
	wantRedirect(t, rep, "/", "a spent cookie")
	if has, _ := g.b.hasTwoFactor(context.Background(), uid); has {
		t.Fatal("the setup was confirmed with a spent cookie")
	}
}

// mustNonce returns the nonce inside a sealed second-step cookie.
func mustNonce(t *testing.T, g *rig, c *http.Cookie) string {
	t.Helper()
	var st mfaState
	nonce, _, ok := g.b.openValue(sealMFA, c.Value, &st)
	if !ok {
		t.Fatal("the cookie does not open")
	}
	return nonce
}

func TestTheSetupStepFailsCleanlyWhenTheStoreLetsItDown(t *testing.T) {
	t.Run("the setup page cannot read the account", func(t *testing.T) {
		g := newRig(t)
		_, mfa := g.needingSetup("alice")
		breakTable(t, g.st, "users")
		wantStatus(t, g.do("GET", "/pages/login/2fa/setup", nil, mfa), http.StatusInternalServerError, "no account")
	})
	t.Run("the setup page cannot keep the pending secret", func(t *testing.T) {
		g := newRig(t)
		_, mfa := g.needingSetup("alice")
		breakTable(t, g.st, "user_totp")
		wantStatus(t, g.do("GET", "/pages/login/2fa/setup", nil, mfa), http.StatusInternalServerError, "no secret")
	})
	// each of these breaks one write that comes after the code was found right
	for _, sc := range []struct {
		name   string
		break_ func(*testing.T, *rig)
	}{
		{"the confirmation cannot be saved", func(t *testing.T, g *rig) { rejectWritesOn(t, g.st, "user_totp", "UPDATE") }},
		{"the recovery codes cannot be saved", func(t *testing.T, g *rig) { breakTable(t, g.st, "user_recovery_codes") }},
		{"the session cannot be started", func(t *testing.T, g *rig) { breakTable(t, g.st, "sessions") }},
	} {
		t.Run(sc.name, func(t *testing.T) {
			g := newRig(t)
			uid, mfa := g.needingSetup("alice")
			secret, err := g.b.pendingTOTPSecret(context.Background(), uid)
			if err != nil {
				t.Fatal(err)
			}
			sc.break_(t, g)
			good, _ := totpCode(secret, totpStep(time.Now()))
			rep := g.do("POST", "/auth/2fa/setup", map[string]string{"Code": good}, mfa)
			wantStatus(t, rep, http.StatusInternalServerError, sc.name)
			if rep.cookie(sessionCookie) != nil {
				t.Fatal("a session was started although the store failed")
			}
		})
	}
}

// resetFails is a store whose password reset cannot be completed.
type resetFails struct {
	store.Store
	err error
}

func (r resetFails) CompletePasswordReset(context.Context, string, string) (int64, error) {
	return 0, r.err
}

func TestThePasswordResetPagesRefuseWhatIsNotAValidLink(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	ctx := context.Background()
	token, _, err := g.b.newResetToken(ctx, uid, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reset := func(token string, body any) reply {
		return g.doWithToken("POST", "/auth/password/reset", token, body)
	}

	t.Run("with password sign-in off there is nothing to reset", func(t *testing.T) {
		if err := g.st.PutSetting(ctx, settingPasswordOn, valueDisabled); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { g.st.PutSetting(ctx, settingPasswordOn, valueEnabled) })
		for _, rep := range []reply{
			g.doWithToken("GET", "/pages/password/reset", token, nil),
			reset(token, map[string]string{"Password": "a new password", "Confirm": "a new password"}),
		} {
			wantStatus(t, rep, http.StatusForbidden, "password sign-in off")
		}
	})
	t.Run("a token that does not exist is invalid, counted and audited", func(t *testing.T) {
		rep := g.doWithToken("GET", "/pages/password/reset", "no-such-token", nil)
		wantStatus(t, rep, http.StatusOK, "page of an invalid token")
		var out apiclient.ResetStep
		rep.decode(t, &out)
		if out.Mode != "invalid" || out.Status != http.StatusNotFound {
			t.Fatalf("invalid token page: %+v", out)
		}
		rep = reset("no-such-token", map[string]string{"Password": "a new password", "Confirm": "a new password"})
		rep.decode(t, &out)
		if out.Mode != "invalid" || !g.audited(auditResetInvalid) {
			t.Fatalf("invalid token submit: %+v", out)
		}
	})
	t.Run("an address that tried too often is refused before the token is looked at", func(t *testing.T) {
		for i := 0; i < maxIPFail; i++ {
			g.b.limiter.Fail(ctx, "ip:198.51.100.9")
		}
		rep := g.send("198.51.100.9", "POST", "/auth/password/reset", map[string]string{apiclient.HeaderResetToken: token}, map[string]string{"Password": "a new password", "Confirm": "a new password"})
		var out apiclient.ResetStep
		rep.decode(t, &out)
		if out.Status != http.StatusTooManyRequests {
			t.Fatalf("locked-out address: %+v", out)
		}
	})
	t.Run("a weak password is refused and the link stays good", func(t *testing.T) {
		rep := reset(token, map[string]string{"Password": "short", "Confirm": "short"})
		var out apiclient.ResetStep
		rep.decode(t, &out)
		if out.Status != http.StatusUnprocessableEntity || out.Mode != "form" {
			t.Fatalf("weak password: %+v", out)
		}
		if _, ok, _ := g.b.resetUser(httptest.NewRequest("GET", "/", nil).WithContext(ctx), token); !ok {
			t.Fatal("a refused form used the link up")
		}
	})
	t.Run("a link of absurd length is not even looked up", func(t *testing.T) {
		rep := g.doWithToken("GET", "/pages/password/reset", strings.Repeat("a", 200), nil)
		var out apiclient.ResetStep
		rep.decode(t, &out)
		if out.Mode != "invalid" {
			t.Fatalf("long token: %+v", out)
		}
	})
}

func TestThePasswordResetOfAnAccountWithTwoFactorNeedsAGoodCode(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	tfEnable(t, g.srv, uid)
	ctx := context.Background()
	token, _, err := g.b.newResetToken(ctx, uid, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	form := func(code string) map[string]string {
		return map[string]string{"Password": "a new password", "Confirm": "a new password", "Code": code}
	}
	var out apiclient.ResetStep
	g.doWithToken("GET", "/pages/password/reset", token, nil).decode(t, &out)
	if !out.NeedsCode {
		t.Fatalf("the form does not ask for a code: %+v", out)
	}

	t.Run("a wrong code is refused and audited", func(t *testing.T) {
		rep := g.doWithToken("POST", "/auth/password/reset", token, form(tfWrong(t, mustSecret(t, g, uid))))
		rep.decode(t, &out)
		if out.Status != http.StatusUnauthorized || out.Error != secondFactorMessage || rep.cookie(sessionCookie) != nil {
			t.Fatalf("wrong code: %+v", out)
		}
		if !g.auditedWith(auditSecondFactorFailed, "reset") {
			t.Fatal("not audited")
		}
	})
	t.Run("too many wrong codes lock it", func(t *testing.T) {
		for i := 0; i < maxMFAFail; i++ {
			g.b.limiter.Fail(ctx, mfaKey(uid))
		}
		defer g.b.limiter.Reset(ctx, mfaKey(uid))
		rep := g.doWithToken("POST", "/auth/password/reset", token, form(tfCode(t, g.srv, uid)))
		rep.decode(t, &out)
		if out.Status != http.StatusTooManyRequests || !g.auditedWith(auditSecondFactorThrottled, "reset") {
			t.Fatalf("locked out: %+v", out)
		}
	})
	t.Run("the code is checked against the store, whose failure is reported", func(t *testing.T) {
		g2 := newRig(t)
		uid2, _ := g2.signUp("bob")
		tfEnable(t, g2.srv, uid2)
		token2, _, _ := g2.b.newResetToken(ctx, uid2, nil, time.Hour)
		code := tfCode(t, g2.srv, uid2)
		rejectWritesOn(t, g2.st, "user_totp", "UPDATE")
		rep := g2.doWithToken("POST", "/auth/password/reset", token2, form(code))
		wantStatus(t, rep, http.StatusInternalServerError, "the code cannot be recorded")
	})
	t.Run("a good code resets the password and signs in without asking again", func(t *testing.T) {
		rep := g.doWithToken("POST", "/auth/password/reset", token, form(tfCode(t, g.srv, uid)))
		rep.decode(t, &out)
		if out.Redirect == "" || rep.cookie(sessionCookie) == nil || rep.cookie(adminCookie) == nil {
			t.Fatalf("good code: %+v", out)
		}
	})
}

func TestThePasswordResetFailsCleanlyWhenTheStoreLetsItDown(t *testing.T) {
	newAccount := func(t *testing.T, g *rig, twoFactor bool) (int64, string) {
		uid, _ := g.signUp("alice")
		if twoFactor {
			tfEnable(t, g.srv, uid)
		}
		token, _, err := g.b.newResetToken(context.Background(), uid, nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return uid, token
	}
	good := map[string]string{"Password": "a new password", "Confirm": "a new password"}

	t.Run("the page cannot look the link up", func(t *testing.T) {
		g := newRig(t)
		_, token := newAccount(t, g, false)
		breakTable(t, g.st, "password_resets")
		wantStatus(t, g.doWithToken("GET", "/pages/password/reset", token, nil), http.StatusInternalServerError, "page")
		wantStatus(t, g.doWithToken("POST", "/auth/password/reset", token, good), http.StatusInternalServerError, "submit")
	})
	t.Run("the page cannot tell whether the account has two-step verification", func(t *testing.T) {
		g := newRig(t)
		_, token := newAccount(t, g, false)
		breakTable(t, g.st, "user_totp")
		wantStatus(t, g.doWithToken("GET", "/pages/password/reset", token, nil), http.StatusInternalServerError, "page")
		wantStatus(t, g.doWithToken("POST", "/auth/password/reset", token, good), http.StatusInternalServerError, "submit")
	})
	t.Run("the sessions cannot be ended, so the password stays", func(t *testing.T) {
		g := newRig(t)
		uid, token := newAccount(t, g, false)
		breakTable(t, g.st, "sessions")
		wantStatus(t, g.doWithToken("POST", "/auth/password/reset", token, good), http.StatusInternalServerError, "sessions")
		if _, ok, _ := g.b.resetUser(httptest.NewRequest("GET", "/", nil), token); !ok {
			t.Fatalf("the link of user %d was used up although the reset failed", uid)
		}
	})
	t.Run("the link was used a moment ago", func(t *testing.T) {
		g := newRig(t)
		_, token := newAccount(t, g, false)
		g2 := newRigOn(t, resetFails{g.st, store.ErrNotFound}, g.st)
		g2.b.cookieKey = g.b.cookieKey
		rep := g2.doWithToken("POST", "/auth/password/reset", token, good)
		var out apiclient.ResetStep
		rep.decode(t, &out)
		if out.Mode != "invalid" || out.Status != http.StatusNotFound {
			t.Fatalf("used link: %+v", out)
		}
	})
	t.Run("the reset cannot be completed", func(t *testing.T) {
		g := newRig(t)
		_, token := newAccount(t, g, false)
		g2 := newRigOn(t, resetFails{g.st, errors.New("disk full")}, g.st)
		wantStatus(t, g2.doWithToken("POST", "/auth/password/reset", token, good), http.StatusInternalServerError, "complete")
	})
	for _, twoFactor := range []bool{false, true} {
		name := "the session after the reset cannot be started"
		if twoFactor {
			name += " (with a code)"
		}
		t.Run(name, func(t *testing.T) {
			g := newRig(t)
			uid, token := newAccount(t, g, twoFactor)
			rejectWritesOn(t, g.st, "sessions", "INSERT")
			body := map[string]string{"Password": "a new password", "Confirm": "a new password"}
			if twoFactor {
				body["Code"] = tfCode(t, g.srv, uid)
			}
			rep := g.doWithToken("POST", "/auth/password/reset", token, body)
			wantStatus(t, rep, http.StatusInternalServerError, "start the session")
		})
	}
}

func TestTheForgottenPasswordMailIsSentOnlyWhereItCanWork(t *testing.T) {
	ctx := context.Background()
	base, _ := url.Parse("https://dexa.example.com")

	t.Run("the lookup failing is logged and nothing is sent", func(t *testing.T) {
		g := newRig(t)
		fm := &fakeMailer{}
		g.signUp("alice")
		enableSelfService(t, g.srv, fm)
		breakTable(t, g.st, "users")
		logs := captureLog(t)
		g.b.sendResetFor("alice@example.com", base)
		if !strings.Contains(logs.String(), "password reset lookup") || len(fm.all()) != 0 {
			t.Fatalf("log %q, mails %d", logs.String(), len(fm.all()))
		}
	})
	t.Run("an account that cannot be read for its password gets nothing", func(t *testing.T) {
		g := newRig(t)
		fm := &fakeMailer{}
		g.signUp("alice")
		enableSelfService(t, g.srv, fm)
		breakTable(t, g.st, "user_identities")
		g.b.sendResetFor("alice@example.com", base)
		if len(fm.all()) != 0 {
			t.Fatal("a mail went out although the account could not be checked")
		}
	})
	t.Run("a token that cannot be stored is logged and nothing is sent", func(t *testing.T) {
		g := newRig(t)
		fm := &fakeMailer{}
		g.signUp("alice")
		enableSelfService(t, g.srv, fm)
		breakTable(t, g.st, "password_resets")
		logs := captureLog(t)
		g.b.sendResetFor("alice@example.com", base)
		if !strings.Contains(logs.String(), "password reset token") || len(fm.all()) != 0 {
			t.Fatalf("log %q, mails %d", logs.String(), len(fm.all()))
		}
	})
	t.Run("a mail that cannot be sent is audited", func(t *testing.T) {
		g := newRig(t)
		g.signUp("alice")
		l := *g.b.cfg()
		l.baseURL = base // a base URL but no mailer
		g.b.swapSettings(&l)
		g.b.sendResetFor("alice@example.com", base)
		if !g.audited(auditResetEmailFailed) {
			t.Fatal("the failed mail was not audited")
		}
	})
	_ = ctx
}

func (r resetFails) Unwrap() store.Store { return r.Store }
