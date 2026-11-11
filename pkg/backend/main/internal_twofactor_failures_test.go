package mainbackend

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// twoFactorPath is the settings endpoint of a user for the two-step verification.
func twoFactorPath(uid int64, action string) string {
	return fmt.Sprintf("/users/%d/2fa/%s", uid, action)
}

func TestTheTwoFactorPolicyIgnoresAValueItDoesNotKnow(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	if err := g.st.PutSetting(context.Background(), settingRequire2FA, "sometimes"); err != nil {
		t.Fatal(err)
	}
	if got := g.b.twoFactorPolicy(context.Background()); got != require2FAOff {
		t.Fatalf("policy %q, want %q", got, require2FAOff)
	}
	u, _ := g.st.GetUser(context.Background(), uid)
	if g.b.twoFactorRequired(context.Background(), u) {
		t.Fatal("an unknown policy required two-step verification")
	}
}

func TestACodeIsNotAcceptedForAnAccountWithoutTwoFactor(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	ctx := context.Background()
	check := func(what string) {
		t.Helper()
		ok, recovery, err := g.b.verifySecondFactor(ctx, uid, "123456")
		if err != nil || ok || recovery {
			t.Fatalf("%s: ok %v recovery %v err %v", what, ok, recovery, err)
		}
	}
	check("no setup at all")
	if err := g.st.PutPendingTOTP(ctx, uid, newTOTPSecret()); err != nil {
		t.Fatal(err)
	}
	check("a setup that was never confirmed")
	if ok, _, _ := g.b.verifySecondFactor(ctx, uid, "   "); ok {
		t.Fatal("a blank code was accepted")
	}
}

func TestTheSetupQRCodeNamesTheSiteAndTheAccount(t *testing.T) {
	g := newRig(t)
	secret := newTOTPSecret()
	want := func(issuer, account string) string {
		svg, err := qrSVG(otpauthURI(issuer, account, secret))
		if err != nil {
			t.Fatal(err)
		}
		return string(svg)
	}
	if got := string(g.b.twoFactorSetupFor(models.User{Username: "Pat"}, secret).QR); got != want(defaultTOTPIssuer, "Pat") {
		t.Error("an account without an address is not named after the user")
	}
	if got := string(g.b.twoFactorSetupFor(models.User{Username: "Pat", Email: "pat@example.com"}, secret).QR); got != want(defaultTOTPIssuer, "pat@example.com") {
		t.Error("an account with an address is not named after it")
	}
	enableSelfService(t, g.srv, &fakeMailer{}) // sets the base URL https://dexa.example.com
	if got := string(g.b.twoFactorSetupFor(models.User{Username: "Pat"}, secret).QR); got != want("dexa.example.com", "Pat") {
		t.Error("the site is not named after the base URL")
	}
}

func TestTwoFactorSettingsRefuseAGarbledBody(t *testing.T) {
	g := newRig(t)
	uid, session := g.signUp("alice")
	for _, action := range []string{"begin", "confirm", "disable", "recovery"} {
		t.Run(action, func(t *testing.T) {
			wantStatus(t, g.do("POST", twoFactorPath(uid, action), "nonsense", session), http.StatusBadRequest, action)
		})
	}
	wantStatus(t, g.do("POST", "/admin/step-up", "nonsense", session), http.StatusBadRequest, "admin step-up")
}

func TestTwoFactorSettingsRefuseAnotherUsersAccountAndAStrangersRequest(t *testing.T) {
	g := newRig(t)
	g.signUp("alice")
	bob, bobSession := g.signUp("bob")
	aliceID, _ := g.st.GetUserByEmail(context.Background(), "alice@example.com")
	for _, action := range []string{"begin", "confirm", "disable", "recovery"} {
		wantStatus(t, g.do("POST", twoFactorPath(aliceID.ID, action), map[string]string{}, bobSession), http.StatusNotFound, "another user's "+action)
		wantStatus(t, g.do("POST", twoFactorPath(bob, action), map[string]string{}), http.StatusUnauthorized, "anonymous "+action)
	}
}

func TestTheSetupFromTheSettingsPageRefusesWhatItShould(t *testing.T) {
	g := newRig(t)
	uid, session := g.signUp("alice")
	ctx := context.Background()

	t.Run("an account without a password has nothing to protect", func(t *testing.T) {
		g2 := newRig(t)
		id, ses := g2.signUp("pat")
		if err := g2.st.DeleteIdentity(ctx, id, models.ProviderLocal); err != nil {
			t.Fatal(err)
		}
		rep := g2.do("POST", twoFactorPath(id, "begin"), map[string]string{}, ses)
		wantStatus(t, rep, http.StatusUnprocessableEntity, "no password")
	})
	t.Run("a wrong password is refused", func(t *testing.T) {
		rep := g.do("POST", twoFactorPath(uid, "begin"), map[string]string{"Current": "wrong password"}, session)
		wantStatus(t, rep, http.StatusUnprocessableEntity, "wrong password")
	})
	t.Run("confirming before beginning is refused", func(t *testing.T) {
		rep := g.do("POST", twoFactorPath(uid, "confirm"), map[string]string{"Code": "123456"}, session)
		wantStatus(t, rep, http.StatusUnprocessableEntity, "confirm first")
		if !strings.Contains(rep.body(), "Start the setup first.") {
			t.Fatal(rep.body())
		}
	})
	t.Run("beginning twice shows the same secret", func(t *testing.T) {
		var first, second apiclient.TwoFactorChange
		g.do("POST", twoFactorPath(uid, "begin"), map[string]string{"Current": testPassword}, session).decode(t, &first)
		g.do("POST", twoFactorPath(uid, "begin"), map[string]string{"Current": testPassword}, session).decode(t, &second)
		if first.Setup == nil || second.Setup == nil || first.Setup.Secret != second.Setup.Secret {
			t.Fatalf("secrets %+v %+v", first.Setup, second.Setup)
		}
	})
	t.Run("a wrong code shows the setup again, too many lock it", func(t *testing.T) {
		secret := mustSecret(t, g, uid)
		var out apiclient.TwoFactorChange
		g.do("POST", twoFactorPath(uid, "confirm"), map[string]string{"Code": tfWrong(t, secret)}, session).decode(t, &out)
		if out.Status != http.StatusUnprocessableEntity || out.Setup == nil || out.Error == "" {
			t.Fatalf("wrong code: %+v", out)
		}
		for i := 0; i < maxMFAFail; i++ {
			g.b.limiter.Fail(ctx, mfaKey(uid))
		}
		code, _ := totpCode(secret, totpStep(time.Now()))
		g.do("POST", twoFactorPath(uid, "confirm"), map[string]string{"Code": code}, session).decode(t, &out)
		if out.Status != http.StatusTooManyRequests || out.Setup == nil {
			t.Fatalf("locked out: %+v", out)
		}
		if has, _ := g.b.hasTwoFactor(ctx, uid); has {
			t.Fatal("two-step verification was switched on while locked out")
		}
		g.b.limiter.Reset(ctx, mfaKey(uid))
	})
	t.Run("beginning when it is already on is refused", func(t *testing.T) {
		if err := g.st.ConfirmTOTP(ctx, uid, totpStep(time.Now())-100); err != nil {
			t.Fatal(err)
		}
		rep := g.do("POST", twoFactorPath(uid, "begin"), map[string]string{"Current": testPassword}, session)
		wantStatus(t, rep, http.StatusUnprocessableEntity, "already on")
		if !strings.Contains(rep.body(), "already on") {
			t.Fatal(rep.body())
		}
	})
}

func TestChangingTwoFactorNeedsThePasswordAndACode(t *testing.T) {
	for _, action := range []string{"disable", "recovery"} {
		t.Run(action, func(t *testing.T) {
			g := newRig(t)
			uid, session := g.signUp("alice")
			ctx := context.Background()
			if action == "recovery" {
				wantStatus(t, g.do("POST", twoFactorPath(uid, action), map[string]string{"Current": testPassword}, session), http.StatusUnprocessableEntity, "not on")
			}
			tfEnable(t, g.srv, uid)
			good := func() map[string]string {
				return map[string]string{"Current": testPassword, "Code": tfCode(t, g.srv, uid)}
			}

			rep := g.do("POST", twoFactorPath(uid, action), map[string]string{"Current": "wrong", "Code": tfCode(t, g.srv, uid)}, session)
			wantStatus(t, rep, http.StatusUnprocessableEntity, "wrong password")
			rep = g.do("POST", twoFactorPath(uid, action), map[string]string{"Current": testPassword, "Code": tfWrong(t, mustSecret(t, g, uid))}, session)
			wantStatus(t, rep, http.StatusUnprocessableEntity, "wrong code")
			if !g.auditedWith(auditSecondFactorFailed, "settings") {
				t.Fatal("the wrong code was not audited")
			}
			for i := 0; i < maxMFAFail; i++ {
				g.b.limiter.Fail(ctx, mfaKey(uid))
			}
			rep = g.do("POST", twoFactorPath(uid, action), good(), session)
			wantStatus(t, rep, http.StatusTooManyRequests, "locked out")
			if !g.audited(auditSecondFactorThrottled) {
				t.Fatal("the lock-out was not audited")
			}
			if has, _ := g.b.hasTwoFactor(ctx, uid); !has {
				t.Fatal("two-step verification was changed while locked out")
			}
		})
	}
}

func TestTwoFactorSettingsFailCleanlyWhenTheStoreLetsThemDown(t *testing.T) {
	t.Run("begin: the account's password cannot be read", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		breakTable(t, g.st, "user_identities")
		wantStatus(t, g.do("POST", twoFactorPath(uid, "begin"), map[string]string{"Current": testPassword}, session), http.StatusInternalServerError, "identities")
	})
	t.Run("begin: the secret cannot be kept", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		breakTable(t, g.st, "user_totp")
		wantStatus(t, g.do("POST", twoFactorPath(uid, "begin"), map[string]string{"Current": testPassword}, session), http.StatusInternalServerError, "totp")
	})
	t.Run("confirm: the setup cannot be read", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		breakTable(t, g.st, "user_totp")
		wantStatus(t, g.do("POST", twoFactorPath(uid, "confirm"), map[string]string{"Code": "123456"}, session), http.StatusInternalServerError, "totp")
	})
	t.Run("confirm: the confirmation cannot be saved", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		g.do("POST", twoFactorPath(uid, "begin"), map[string]string{"Current": testPassword}, session)
		code, _ := totpCode(mustSecret(t, g, uid), totpStep(time.Now()))
		rejectWritesOn(t, g.st, "user_totp", "UPDATE")
		wantStatus(t, g.do("POST", twoFactorPath(uid, "confirm"), map[string]string{"Code": code}, session), http.StatusInternalServerError, "confirm")
	})
	t.Run("confirm: the recovery codes cannot be saved", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		g.do("POST", twoFactorPath(uid, "begin"), map[string]string{"Current": testPassword}, session)
		code, _ := totpCode(mustSecret(t, g, uid), totpStep(time.Now()))
		breakTable(t, g.st, "user_recovery_codes")
		wantStatus(t, g.do("POST", twoFactorPath(uid, "confirm"), map[string]string{"Code": code}, session), http.StatusInternalServerError, "codes")
	})
	t.Run("disable: the setting cannot be removed", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		tfEnable(t, g.srv, uid)
		code := tfCode(t, g.srv, uid)
		rejectWritesOn(t, g.st, "user_totp", "DELETE")
		wantStatus(t, g.do("POST", twoFactorPath(uid, "disable"), map[string]string{"Current": testPassword, "Code": code}, session), http.StatusInternalServerError, "delete")
	})
	t.Run("disable: the code cannot be checked", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		tfEnable(t, g.srv, uid)
		rejectWritesOn(t, g.st, "user_totp", "UPDATE")
		code := tfCode(t, g.srv, uid)
		wantStatus(t, g.do("POST", twoFactorPath(uid, "disable"), map[string]string{"Current": testPassword, "Code": code}, session), http.StatusInternalServerError, "check")
	})
	t.Run("recovery: the state cannot be read", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		breakTable(t, g.st, "user_totp")
		wantStatus(t, g.do("POST", twoFactorPath(uid, "recovery"), map[string]string{}, session), http.StatusInternalServerError, "totp")
	})
	t.Run("recovery: the new codes cannot be saved", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		tfEnable(t, g.srv, uid)
		code := tfCode(t, g.srv, uid)
		breakTable(t, g.st, "user_recovery_codes")
		wantStatus(t, g.do("POST", twoFactorPath(uid, "recovery"), map[string]string{"Current": testPassword, "Code": code}, session), http.StatusInternalServerError, "codes")
	})
	t.Run("sign-in: the state of the account cannot be read", func(t *testing.T) {
		g := newRig(t)
		g.signUp("alice")
		breakTable(t, g.st, "user_totp")
		wantStatus(t, g.signIn("alice"), http.StatusInternalServerError, "sign in")
	})
}

func TestTheAdminStepUpAsksForACodeAndRefusesWrongOnes(t *testing.T) {
	g := newRig(t)
	uid, session := g.signUp("root") // the first account is the administrator
	other, otherSession := g.signUp("bob")
	tfEnable(t, g.srv, uid)
	ctx := context.Background()

	t.Run("an administrator without the proof is sent to the step-up", func(t *testing.T) {
		rep := g.do("GET", "/admin/page", nil, session)
		if rep.Code != http.StatusForbidden || rep.code(t) != apiclient.CodeStepUp {
			t.Fatalf("%d %s", rep.Code, rep.body())
		}
		var out map[string]bool
		g.do("GET", "/admin/step-up", nil, session).decode(t, &out)
		if !out["needed"] {
			t.Fatalf("step-up %v", out)
		}
	})
	t.Run("a user who is not an administrator is told nothing", func(t *testing.T) {
		wantStatus(t, g.do("GET", "/admin/step-up", nil, otherSession), http.StatusForbidden, "bob")
		_ = other
	})
	t.Run("a wrong code is refused and audited", func(t *testing.T) {
		rep := g.do("POST", "/admin/step-up", map[string]string{"Code": tfWrong(t, mustSecret(t, g, uid))}, session)
		var out apiclient.SecondStep
		rep.decode(t, &out)
		if out.Status != http.StatusUnauthorized || rep.cookie(adminCookie) != nil || !g.auditedWith(auditSecondFactorFailed, "admin") {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("a locked-out account is refused even with the right code", func(t *testing.T) {
		for i := 0; i < maxMFAFail; i++ {
			g.b.limiter.Fail(ctx, mfaKey(uid))
		}
		defer g.b.limiter.Reset(ctx, mfaKey(uid))
		rep := g.do("POST", "/admin/step-up", map[string]string{"Code": tfCode(t, g.srv, uid)}, session)
		var out apiclient.SecondStep
		rep.decode(t, &out)
		if out.Status != http.StatusTooManyRequests || rep.cookie(adminCookie) != nil || !g.auditedWith(auditSecondFactorThrottled, "admin") {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("the right code gives the proof, which opens the admin page", func(t *testing.T) {
		rep := g.do("POST", "/admin/step-up", map[string]string{"Code": tfCode(t, g.srv, uid)}, session)
		proof := rep.cookie(adminCookie)
		if proof == nil {
			t.Fatalf("no proof: %s", rep.body())
		}
		wantStatus(t, g.do("GET", "/admin/page", nil, session, proof), http.StatusOK, "with the proof")
	})
	t.Run("a proof made for another administrator does not open it", func(t *testing.T) {
		sealed, _, _ := g.b.sealValue(sealAdminStep, adminStepUp{UID: other}, time.Minute)
		rep := g.do("GET", "/admin/page", nil, session, &http.Cookie{Name: adminCookie, Value: sealed})
		if rep.Code != http.StatusForbidden || rep.code(t) != apiclient.CodeStepUp {
			t.Fatalf("%d %s", rep.Code, rep.body())
		}
	})
	t.Run("the code cannot be checked", func(t *testing.T) {
		rejectWritesOn(t, g.st, "user_totp", "UPDATE")
		next, _ := totpCode(mustSecret(t, g, uid), totpStep(time.Now())+1) // the current step was used by the proof above
		wantStatus(t, g.do("POST", "/admin/step-up", map[string]string{"Code": next}, session), http.StatusInternalServerError, "check")
	})
	t.Run("the state of the account cannot be read", func(t *testing.T) {
		breakTable(t, g.st, "user_totp")
		wantStatus(t, g.do("GET", "/admin/step-up", nil, session), http.StatusInternalServerError, "step-up page")
		wantStatus(t, g.do("GET", "/admin/page", nil, session), http.StatusInternalServerError, "admin page")
	})
}

var _ = url.Values{}
