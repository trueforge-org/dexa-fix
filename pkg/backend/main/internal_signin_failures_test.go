package mainbackend

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

func TestTheLoginPageFailsCleanlyWhenTheStoreLetsItDown(t *testing.T) {
	for _, table := range []string{"users", "auth_providers", "app_settings"} {
		t.Run("without the "+table+" table", func(t *testing.T) {
			g := newRig(t)
			g.signUp("alice")
			breakTable(t, g.st, table)
			wantStatus(t, g.do("GET", "/pages/login", nil), http.StatusInternalServerError, "login page")
		})
	}
}

func TestSigningUpIsRefusedWhenItShouldBe(t *testing.T) {
	type outcome = apiclient.SignInOutcome
	form := func(name, email, password string) map[string]string {
		return map[string]string{"Name": name, "Email": email, "Password": password, "Confirm": password}
	}
	signUp := func(g *rig, body map[string]string) outcome {
		var out outcome
		g.do("POST", "/auth/signup", body).decode(t, &out)
		return out
	}

	t.Run("an address that is not one", func(t *testing.T) {
		g := newRig(t)
		if out := signUp(g, form("Pat", "not an address", testPassword)); out.Status != http.StatusUnprocessableEntity || !strings.Contains(out.Error, "valid e-mail") {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("a name that is empty", func(t *testing.T) {
		g := newRig(t)
		if out := signUp(g, form("", "pat@example.com", testPassword)); out.Status != http.StatusUnprocessableEntity || !strings.Contains(out.Error, "1-64 characters") {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("a client address that already tried too often", func(t *testing.T) {
		g := newRig(t)
		for i := 0; i < maxIPFail; i++ {
			g.b.limiter.Fail(context.Background(), "ip:198.51.100.20")
		}
		var out outcome
		g.doFrom("198.51.100.20", "POST", "/auth/signup", form("Pat", "pat@example.com", testPassword)).decode(t, &out)
		if out.Status != http.StatusTooManyRequests {
			t.Fatalf("%+v", out)
		}
		if users, _ := g.st.ListUsers(context.Background()); len(users) != 0 {
			t.Fatal("an account was made for a locked-out address")
		}
	})
	t.Run("whether sign-up is open cannot be read", func(t *testing.T) {
		g := newRig(t)
		g.b.ForcePasswordLogin = true // so that the unreadable setting is not mistaken for "password sign-in is off"
		breakTable(t, g.st, "app_settings")
		wantStatus(t, g.do("POST", "/auth/signup", form("Pat", "pat@example.com", testPassword)), http.StatusInternalServerError, "settings")
	})
	t.Run("the account cannot be made", func(t *testing.T) {
		g := newRig(t)
		breakTable(t, g.st, "users")
		wantStatus(t, g.do("POST", "/auth/signup", form("Pat", "pat@example.com", testPassword)), http.StatusInternalServerError, "users")
	})
	t.Run("the session of the new account cannot be started", func(t *testing.T) {
		g := newRig(t)
		breakTable(t, g.st, "sessions")
		rep := g.do("POST", "/auth/signup", form("Pat", "pat@example.com", testPassword))
		wantStatus(t, rep, http.StatusInternalServerError, "sessions")
		if rep.cookie(sessionCookie) != nil {
			t.Fatal("a session cookie came with the failure")
		}
	})
}

func TestTheSetupStepFailsWhenTheAccountVanishesBeforeTheSessionStarts(t *testing.T) {
	g := newRig(t)
	uid, mfa := g.needingSetup("alice")
	secret, err := g.b.pendingTOTPSecret(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := totpCode(secret, totpStep(time.Now()))
	breakTable(t, g.st, "users")
	rep := g.do("POST", "/auth/2fa/setup", map[string]string{"Code": code}, mfa)
	wantStatus(t, rep, http.StatusInternalServerError, "no account")
	if rep.cookie(sessionCookie) != nil {
		t.Fatal("a session cookie came with the failure")
	}
}
