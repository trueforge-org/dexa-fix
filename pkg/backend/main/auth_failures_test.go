package mainbackend

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
	"golang.org/x/crypto/bcrypt"
)

// publicGet calls the public API of the rig's backend with the given headers.
func (g *rig) publicGet(ctx context.Context, path string, hdr map[string]string) *httptest.ResponseRecorder {
	g.t.Helper()
	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	req.RemoteAddr = "192.0.2.50:1234"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.b.PublicHandler().ServeHTTP(rec, req)
	return rec
}

func basic(login, password string) map[string]string {
	return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(login+":"+password))}
}

func TestBasicCredentialsOnTheAPIAreRefusedWhenTheyShouldBe(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	path := fmt.Sprintf("/api/v1/users/%d/settings", uid)
	ctx := context.Background()
	good := basic("alice@example.com", testPassword)
	if rec := g.publicGet(ctx, path, good); rec.Code != 200 {
		t.Fatalf("good credentials: %d %s", rec.Code, rec.Body)
	}

	t.Run("a login that failed too often is refused with the right password", func(t *testing.T) {
		for i := 0; i < maxLoginFail; i++ {
			g.b.limiter.Fail(ctx, loginKey("alice@example.com"))
		}
		defer g.b.limiter.Reset(ctx, loginKey("alice@example.com"))
		if rec := g.publicGet(ctx, path, good); rec.Code != http.StatusUnauthorized {
			t.Fatalf("locked-out login: %d", rec.Code)
		}
	})
	t.Run("an account with two-step verification cannot use a password alone", func(t *testing.T) {
		tfEnable(t, g.srv, uid)
		if rec := g.publicGet(ctx, path, good); rec.Code != http.StatusUnauthorized {
			t.Fatalf("password alone: %d", rec.Code)
		}
		if !g.auditedWith(auditAPIAuthFailed, "second_factor") {
			t.Fatal("not audited")
		}
	})
	t.Run("an account whose two-step state cannot be read is refused", func(t *testing.T) {
		breakTable(t, g.st, "user_totp")
		logs := captureLog(t)
		if rec := g.publicGet(ctx, path, good); rec.Code != http.StatusUnauthorized || !strings.Contains(logs.String(), "authentication failed") {
			t.Fatalf("%d, log %s", rec.Code, logs)
		}
	})
}

func TestBasicCredentialsWaitForASlotAndGiveUpWhenTheClientDoes(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	path := fmt.Sprintf("/api/v1/users/%d/settings", uid)
	old := bcryptWait
	bcryptWait = time.Hour // the client must be what ends the wait
	t.Cleanup(func() { bcryptWait = old })
	for i := 0; i < cap(g.b.bcryptSlots); i++ {
		g.b.bcryptSlots <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	req.SetBasicAuth("alice@example.com", testPassword)
	if u, ok, err := g.b.authBasic(req); ok || err != nil || u.ID != 0 {
		t.Fatalf("a client that gave up was authenticated: %+v %v %v", u, ok, err)
	}
}

func TestProviderIDTokensOnTheAPIAreCheckedAgainstTheirIssuer(t *testing.T) {
	g := newRig(t)
	idp := newFakeIdP(t)
	uid, _ := g.signUp("alice")
	path := fmt.Sprintf("/api/v1/users/%d/settings", uid)
	ctx := context.Background()
	p := models.AuthProvider{Slug: "idp", Label: "Test IdP", Kind: models.ProviderKindOIDC, Issuer: idp.URL, ClientID: "client-1", ClientSecret: "s"}
	g.addProvider(p)
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	good := idp.token(t, "client-1", "sub-1", "")
	unsigned := func(iss string) string {
		enc := base64.RawURLEncoding.EncodeToString
		return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"iss":"`+iss+`"}`)) + ".sig"
	}

	if rec := g.publicGet(ctx, path, bearer(good)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a token of a login nobody linked: %d", rec.Code)
	}
	if err := g.st.PutIdentity(ctx, models.Identity{UserID: uid, Provider: "idp", Subject: "sub-1"}); err != nil {
		t.Fatal(err)
	}
	if rec := g.publicGet(ctx, path, bearer(good)); rec.Code != 200 {
		t.Fatalf("a linked login: %d %s", rec.Code, rec.Body)
	}

	for name, tok := range map[string]string{
		"not a JWT at all":             "nonsense",
		"a JWT with no readable body":  "a.!!!.c",
		"a JWT whose body is not JSON": "a." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".c",
		"a token of another issuer":    unsigned("https://elsewhere.example.com"),
		"a token with a bad signature": unsigned(idp.URL),
		"a token for another client":   idp.token(t, "client-2", "sub-1", ""),
	} {
		t.Run(name, func(t *testing.T) {
			if rec := g.publicGet(ctx, path, bearer(tok)); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%d", rec.Code)
			}
		})
	}
	t.Run("a plain OAuth provider never signs API tokens", func(t *testing.T) {
		q := p
		q.Slug, q.Kind = "plain", models.ProviderKindOAuth2
		g.addProvider(q)
		g.b.forgetOIDCProvider(idp.URL)
		if err := g.st.DeleteAuthProvider(ctx, "idp"); err != nil {
			t.Fatal(err)
		}
		if rec := g.publicGet(ctx, path, bearer(good)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%d", rec.Code)
		}
	})
	t.Run("a provider that cannot be reached", func(t *testing.T) {
		dead := newOAuthFake(t)
		url := dead.URL
		dead.Close()
		g.addProvider(models.AuthProvider{Slug: "dead", Label: "Dead", Kind: models.ProviderKindOIDC, Issuer: url, ClientID: "c", ClientSecret: "s"})
		logs := captureLog(t)
		if rec := g.publicGet(ctx, path, bearer(unsigned(url))); rec.Code != http.StatusUnauthorized || !strings.Contains(logs.String(), "authentication failed") {
			t.Fatalf("%d, log %s", rec.Code, logs)
		}
	})
	t.Run("a provider list that cannot be read", func(t *testing.T) {
		breakTable(t, g.st, "auth_providers")
		if rec := g.publicGet(ctx, path, bearer(good)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%d", rec.Code)
		}
	})
}

func TestALinkedProviderLoginThatCannotBeLookedUpIsRefused(t *testing.T) {
	g := newRig(t)
	idp := newFakeIdP(t)
	uid, _ := g.signUp("alice")
	g.addProvider(models.AuthProvider{Slug: "idp", Label: "Test IdP", Kind: models.ProviderKindOIDC, Issuer: idp.URL, ClientID: "client-1", ClientSecret: "s"})
	breakTable(t, g.st, "user_identities")
	rec := g.publicGet(context.Background(), fmt.Sprintf("/api/v1/users/%d/settings", uid), map[string]string{"Authorization": "Bearer " + idp.token(t, "client-1", "sub-1", "")})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("%d", rec.Code)
	}
}

func TestSigningInFailsCleanlyWhenTheStoreLetsItDown(t *testing.T) {
	t.Run("the account cannot be looked up", func(t *testing.T) {
		g := newRig(t)
		g.signUp("alice")
		breakTable(t, g.st, "users")
		wantStatus(t, g.signIn("alice"), http.StatusInternalServerError, "users")
	})
	t.Run("the password cannot be read", func(t *testing.T) {
		g := newRig(t)
		g.signUp("alice")
		breakTable(t, g.st, "user_identities")
		wantStatus(t, g.signIn("alice"), http.StatusInternalServerError, "identities")
	})
	t.Run("the session cannot be started", func(t *testing.T) {
		g := newRig(t)
		g.signUp("alice")
		breakTable(t, g.st, "sessions")
		rep := g.signIn("alice")
		wantStatus(t, rep, http.StatusInternalServerError, "sessions")
		if rep.cookie(sessionCookie) != nil {
			t.Fatal("a session cookie came with the failure")
		}
	})
	t.Run("a setting that cannot be read leaves password sign-in off", func(t *testing.T) {
		g := newRig(t)
		g.signUp("alice")
		breakTable(t, g.st, "app_settings")
		logs := captureLog(t)
		rep := g.signIn("alice")
		var out struct{ Error string }
		rep.decode(t, &out)
		if !strings.Contains(out.Error, "turned off") || !strings.Contains(logs.String(), "read password login setting") {
			t.Fatalf("%s, log %s", rep.body(), logs)
		}
	})
	t.Run("a setting that is forced on stays on", func(t *testing.T) {
		g := newRig(t)
		g.signUp("alice")
		if err := g.st.PutSetting(context.Background(), settingPasswordOn, valueDisabled); err != nil {
			t.Fatal(err)
		}
		if g.b.passwordEnabled(context.Background()) {
			t.Fatal("password sign-in is on although the administrator turned it off")
		}
		g.b.ForcePasswordLogin = true
		if !g.b.passwordEnabled(context.Background()) {
			t.Fatal("the forced setting did not hold")
		}
	})
}

func TestAnOldPasswordHashIsUpgradedAtSignInButNeverAtTheCostOfSigningIn(t *testing.T) {
	setup := func(t *testing.T) (*rig, int64) {
		old := bcryptCost // the tests hash at the cheapest cost, so a "cheap" hash needs a dearer standard
		bcryptCost = bcrypt.MinCost + 1
		t.Cleanup(func() { bcryptCost = old })
		g := newRig(t)
		uid, _ := g.signUp("alice")
		weak, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
		if err != nil {
			t.Fatal(err)
		}
		if err := g.st.PutIdentity(context.Background(), models.Identity{UserID: uid, Provider: models.ProviderLocal, Subject: localSubject(uid), Secret: string(weak)}); err != nil {
			t.Fatal(err)
		}
		return g, uid
	}
	costOf := func(t *testing.T, g *rig, uid int64) int {
		id, err := g.st.GetIdentity(context.Background(), models.ProviderLocal, localSubject(uid))
		if err != nil {
			t.Fatal(err)
		}
		c, err := bcrypt.Cost([]byte(id.Secret))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Run("a cheap hash is replaced", func(t *testing.T) {
		g, uid := setup(t)
		if rep := g.signIn("alice"); rep.cookie(sessionCookie) == nil {
			t.Fatalf("sign-in failed: %s", rep.body())
		}
		if got := costOf(t, g, uid); got != bcrypt.MinCost+1 {
			t.Fatalf("cost %d, want %d", got, bcrypt.MinCost+1)
		}
	})
	t.Run("when the upgrade cannot be saved the sign-in still works", func(t *testing.T) {
		g, uid := setup(t)
		rejectWritesOn(t, g.st, "user_identities", "INSERT", "UPDATE")
		logs := captureLog(t)
		if rep := g.signIn("alice"); rep.cookie(sessionCookie) == nil || !strings.Contains(logs.String(), "upgrade password hash") {
			t.Fatalf("no session or no log, log %s", logs)
		}
		if got := costOf(t, g, uid); got != bcrypt.MinCost {
			t.Fatalf("cost %d changed", got)
		}
	})
	t.Run("a password that bcrypt cannot take is refused", func(t *testing.T) {
		g, uid := setup(t)
		if err := g.b.setPassword(context.Background(), uid, strings.Repeat("x", 73)); err == nil {
			t.Fatal("a 73 byte password was accepted")
		}
	})
}

func TestAPasswordLongerThanBcryptTakesIsRefusedWithAMessage(t *testing.T) {
	msg := validatePassword(strings.Repeat("x", maxPasswordBytes+1), strings.Repeat("x", maxPasswordBytes+1))
	if !strings.Contains(msg, "at most") {
		t.Fatalf("message %q", msg)
	}
}

func TestAnAccountWithoutAnAddressIsAskedToAddOneAtSignIn(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	rec := httptest.NewRecorder()
	target, err := g.b.completeSignIn(rec, httptest.NewRequest("GET", "/", nil), models.User{ID: uid}, "idp")
	if err != nil || !strings.Contains(target, "add+your+e-mail") {
		t.Fatalf("target %q, err %v", target, err)
	}
}

func TestABackendCannotStartWithoutItsCookieKey(t *testing.T) {
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	breakTable(t, st, "app_settings")
	if b, err := NewBackend(st, testBody(st)); err == nil {
		b.Close()
		t.Fatal("a backend started although its key could not be read or made")
	}
}
