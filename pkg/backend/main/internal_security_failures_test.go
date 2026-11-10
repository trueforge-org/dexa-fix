package mainbackend

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

func userPathOf(uid int64, rest string) string { return fmt.Sprintf("/users/%d/%s", uid, rest) }

func TestAccountSettingsRefuseAGarbledBody(t *testing.T) {
	g := newRig(t)
	uid, session := g.signUp("alice")
	for _, rest := range []string{"profile", "password", "tokens"} {
		t.Run(rest, func(t *testing.T) {
			wantStatus(t, g.do("POST", userPathOf(uid, rest), "nonsense", session), http.StatusBadRequest, rest)
		})
	}
}

func TestAccountSettingsRefuseAnotherUsersAccountAndAnAnonymousVisitor(t *testing.T) {
	g := newRig(t)
	alice, aliceSession := g.signUp("alice")
	_, bobSession := g.signUp("bob")
	for _, c := range []struct{ method, rest string }{
		{"POST", "profile"}, {"POST", "password"}, {"POST", "tokens"}, {"DELETE", "tokens/1"}, {"DELETE", "identities/idp"},
	} {
		wantStatus(t, g.do(c.method, userPathOf(alice, c.rest), map[string]string{}, bobSession), http.StatusNotFound, "bob on alice's "+c.rest)
		wantStatus(t, g.do(c.method, userPathOf(alice, c.rest), map[string]string{}), http.StatusUnauthorized, "anonymous "+c.rest)
	}
	_ = aliceSession
}

func TestTheProfileIsSavedOnlyWhenItIsValidAndTheStoreAgrees(t *testing.T) {
	profile := func(uid int64, g *rig, session *http.Cookie, in apiclient.Profile) reply {
		return g.do("POST", userPathOf(uid, "profile"), in, session)
	}
	t.Run("an address another account already uses", func(t *testing.T) {
		g := newRig(t)
		g.signUp("bob")
		uid, session := g.signUp("alice")
		rep := profile(uid, g, session, apiclient.Profile{Name: "Alice", Email: "bob@example.com", Current: testPassword})
		wantStatus(t, rep, http.StatusUnprocessableEntity, "taken address")
		if !strings.Contains(rep.body(), "Another account already uses") {
			t.Fatal(rep.body())
		}
		if u, _ := g.st.GetUser(context.Background(), uid); u.Email != "alice@example.com" {
			t.Fatalf("the address changed to %q", u.Email)
		}
	})
	t.Run("a new address needs the current password", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		rep := profile(uid, g, session, apiclient.Profile{Name: "Alice", Email: "new@example.com", Current: "wrong"})
		wantStatus(t, rep, http.StatusUnprocessableEntity, "wrong password")
		if u, _ := g.st.GetUser(context.Background(), uid); u.Email != "alice@example.com" {
			t.Fatalf("the address changed to %q without the password", u.Email)
		}
	})
	t.Run("the password cannot be checked", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		breakTable(t, g.st, "user_identities")
		rep := profile(uid, g, session, apiclient.Profile{Name: "Alice", Email: "new@example.com", Current: testPassword})
		wantStatus(t, rep, http.StatusInternalServerError, "identities")
	})
	t.Run("the profile cannot be saved", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		rejectWritesOn(t, g.st, "users", "UPDATE")
		rep := profile(uid, g, session, apiclient.Profile{Name: "Alice", Email: "alice@example.com"})
		wantStatus(t, rep, http.StatusInternalServerError, "update")
	})
	t.Run("the time zone cannot be saved", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		if err := g.st.ExecForTests(context.Background(), "CREATE TRIGGER reject_timezone BEFORE UPDATE OF timezone ON users BEGIN SELECT RAISE(ABORT, 'the test broke the time zone'); END"); err != nil {
			t.Fatal(err)
		}
		rep := profile(uid, g, session, apiclient.Profile{Name: "Alice", Email: "alice@example.com", Timezone: "Asia/Tokyo"})
		wantStatus(t, rep, http.StatusInternalServerError, "time zone")
	})
	t.Run("a changed address makes the old reset links useless", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		token, _, err := g.b.newResetToken(context.Background(), uid, nil, 3600e9)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus(t, profile(uid, g, session, apiclient.Profile{Name: "Alice", Email: "new@example.com", Current: testPassword}), http.StatusNoContent, "change")
		rep := g.doWithToken("GET", "/pages/password/reset", token, nil)
		var out apiclient.ResetStep
		rep.decode(t, &out)
		if out.Mode != "invalid" {
			t.Fatalf("an old link still works: %+v", out)
		}
	})
}

func TestChangingThePasswordIsRefusedOrFailsCleanly(t *testing.T) {
	change := func(g *rig, uid int64, session *http.Cookie, current, password string) reply {
		return g.do("POST", userPathOf(uid, "password"), apiclient.PasswordChange{Current: current, Password: password, Confirm: password}, session)
	}
	signsInWith := func(g *rig, password string) bool {
		rep := g.do("POST", "/auth/password", map[string]string{"Email": "alice@example.com", "Password": password})
		return rep.cookie(sessionCookie) != nil
	}
	const next = "another good password"

	t.Run("with password sign-in turned off", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		if err := g.st.PutSetting(context.Background(), settingPasswordOn, valueDisabled); err != nil {
			t.Fatal(err)
		}
		wantStatus(t, change(g, uid, session, testPassword, next), http.StatusForbidden, "turned off")
	})
	t.Run("with the wrong current password", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		wantStatus(t, change(g, uid, session, "wrong", next), http.StatusUnprocessableEntity, "wrong current")
		if !signsInWith(g, testPassword) {
			t.Fatal("the password changed anyway")
		}
	})
	t.Run("to a password that is too short", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		wantStatus(t, change(g, uid, session, testPassword, "short"), http.StatusUnprocessableEntity, "short")
		if !signsInWith(g, testPassword) {
			t.Fatal("the password changed anyway")
		}
	})
	t.Run("when the other sessions cannot be ended the password stays", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		rejectWritesOn(t, g.st, "sessions", "DELETE")
		wantStatus(t, change(g, uid, session, testPassword, next), http.StatusInternalServerError, "sessions")
		if !signsInWith(g, testPassword) || signsInWith(g, next) {
			t.Fatal("the password changed although the sessions could not be ended")
		}
	})
	t.Run("when the new password cannot be saved", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		rejectWritesOn(t, g.st, "user_identities", "INSERT", "UPDATE")
		wantStatus(t, change(g, uid, session, testPassword, next), http.StatusInternalServerError, "identity")
	})
	t.Run("when the access tokens cannot be revoked", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		breakTable(t, g.st, "access_tokens")
		wantStatus(t, change(g, uid, session, testPassword, next), http.StatusInternalServerError, "tokens")
	})
	t.Run("when the new session cannot be started", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		rejectWritesOn(t, g.st, "sessions", "INSERT")
		wantStatus(t, change(g, uid, session, testPassword, next), http.StatusInternalServerError, "new session")
	})
	t.Run("when the settings page cannot be built afterwards", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		breakTable(t, g.st, "auth_providers")
		wantStatus(t, change(g, uid, session, testPassword, next), http.StatusInternalServerError, "page")
	})
}

func TestAccessTokensAreRefusedWhenTheirRequestIsWrong(t *testing.T) {
	g := newRig(t)
	uid, session := g.signUp("alice")
	create := func(in apiclient.NewTokenRequest) reply {
		return g.do("POST", userPathOf(uid, "tokens"), in, session)
	}
	for name, in := range map[string]apiclient.NewTokenRequest{
		"without a name":         {Current: testPassword},
		"with a name too long":   {Name: strings.Repeat("n", maxTokenNameLen+1), Current: testPassword},
		"with an expiry of text": {Name: "script", Expires: "soon", Current: testPassword},
		"with an expiry of zero": {Name: "script", Expires: "0", Current: testPassword},
		"with an expiry too far": {Name: "script", Expires: "3651", Current: testPassword},
	} {
		t.Run(name, func(t *testing.T) {
			wantStatus(t, create(in), http.StatusUnprocessableEntity, name)
		})
	}
	if tokens, _ := g.st.ListAccessTokens(context.Background(), uid); len(tokens) != 0 {
		t.Fatalf("%d tokens were made from refused requests", len(tokens))
	}
	t.Run("a token that cannot be saved", func(t *testing.T) {
		breakTable(t, g.st, "access_tokens")
		wantStatus(t, create(apiclient.NewTokenRequest{Name: "script", Current: testPassword}), http.StatusInternalServerError, "store")
	})
	t.Run("a token that is not a number is not found", func(t *testing.T) {
		wantStatus(t, g.do("DELETE", userPathOf(uid, "tokens/abc"), nil, session), http.StatusNotFound, "bad id")
	})
}

func TestUnlinkingALoginWorksAndFailsCleanly(t *testing.T) {
	setup := func(t *testing.T) (*rig, int64, *http.Cookie) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		if err := g.st.PutIdentity(context.Background(), models.Identity{UserID: uid, Provider: "idp", Subject: "s1"}); err != nil {
			t.Fatal(err)
		}
		return g, uid, session
	}
	unlink := func(g *rig, uid int64, session *http.Cookie) reply {
		return g.do("DELETE", userPathOf(uid, "identities/idp"), nil, session)
	}
	t.Run("a linked login is removed and audited", func(t *testing.T) {
		g, uid, session := setup(t)
		rep := unlink(g, uid, session)
		if !strings.Contains(rep.body(), "Account unlinked.") || !g.auditedWith(auditIdentityUnlinked, "idp") {
			t.Fatalf("%s", rep.body())
		}
		if _, err := g.st.GetIdentity(context.Background(), "idp", "s1"); err == nil {
			t.Fatal("the login is still linked")
		}
	})
	t.Run("a passkey is removed in its own section", func(t *testing.T) {
		g, uid, session := setup(t)
		rep := g.do("DELETE", userPathOf(uid, "identities/"+models.ProviderPasskey), nil, session)
		if !strings.Contains(rep.body(), "Passkeys section") {
			t.Fatal(rep.body())
		}
	})
	t.Run("the ways to sign in cannot be listed", func(t *testing.T) {
		g, uid, session := setup(t)
		w := newRigOn(t, providersFail{g.st}, g.st)
		wantStatus(t, unlink(w, uid, session), http.StatusInternalServerError, "providers")
	})
	t.Run("the login cannot be removed", func(t *testing.T) {
		g, uid, session := setup(t)
		rejectWritesOn(t, g.st, "user_identities", "DELETE")
		wantStatus(t, unlink(g, uid, session), http.StatusInternalServerError, "delete")
	})
	t.Run("the last way to sign in is kept", func(t *testing.T) {
		g, uid, session := setup(t)
		if err := g.st.DeleteIdentity(context.Background(), uid, models.ProviderLocal); err != nil {
			t.Fatal(err)
		}
		if rep := unlink(g, uid, session); !strings.Contains(rep.body(), "Add another way to sign in") {
			t.Fatal(rep.body())
		}
		if _, err := g.st.GetIdentity(context.Background(), "idp", "s1"); err != nil {
			t.Fatal("the last login was removed")
		}
	})
}

func TestSigningOutFailsCleanlyWhenTheSessionCannotBeEnded(t *testing.T) {
	g := newRig(t)
	g.signUp("alice")
	_, session := g.signUp("bob")
	rejectWritesOn(t, g.st, "sessions", "DELETE")
	rep := g.do("DELETE", "/session", nil, session)
	wantStatus(t, rep, http.StatusInternalServerError, "sign out")
	if rep := g.do("GET", "/session", nil, session); rep.Code != http.StatusOK {
		t.Fatalf("the session ended although the store failed: %d", rep.Code)
	}
}
