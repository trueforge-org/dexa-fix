package mainbackend

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// oauthFake is an identity provider that answers whatever a test sets: the token endpoint, the
// user-info endpoint and the OpenID discovery document.
type oauthFake struct {
	*httptest.Server
	mu             sync.Mutex
	tokenStatus    int
	tokenBody      string
	userinfoStatus int
	userinfoBody   string
}

func newOAuthFake(t *testing.T) *oauthFake {
	t.Helper()
	f := &oauthFake{tokenStatus: 200, tokenBody: `{"access_token":"at","token_type":"Bearer"}`, userinfoStatus: 200, userinfoBody: `{"sub":"subject-1","name":"Pat","email":"pat@example.com","email_verified":true}`}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.tokenStatus)
		fmt.Fprint(w, f.tokenBody)
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.userinfoStatus)
		fmt.Fprint(w, f.userinfoBody)
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q}`, f.URL, f.URL+"/auth", f.URL+"/token", f.URL+"/jwks")
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"keys":[]}`) })
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *oauthFake) set(fn func(*oauthFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// provider is the stored form of a plain OAuth 2.0 provider that points at the fake.
func (f *oauthFake) provider() models.AuthProvider {
	return models.AuthProvider{Slug: "idp", Label: "Test IdP", Kind: models.ProviderKindOAuth2, AuthURL: f.URL + "/auth", TokenURL: f.URL + "/token",
		UserinfoURL: f.URL + "/userinfo", ClientID: "client-1", ClientSecret: "s3cret", AllowSignup: true}
}

// oidcProvider is the stored form of an OpenID Connect (or Apple) provider that points at the fake.
func (f *oauthFake) oidcProvider(kind string) models.AuthProvider {
	p := f.provider()
	p.Kind, p.Issuer = kind, f.URL
	return p
}

func (g *rig) addProvider(p models.AuthProvider) {
	g.t.Helper()
	if err := g.st.PutAuthProvider(context.Background(), p); err != nil {
		g.t.Fatal(err)
	}
}

// beginProvider starts a sign-in (or, with a session, a link) and returns what the browser would
// hold: the state parameter and the cookie.
func (g *rig) beginProvider(slug string, body any, cookies ...*http.Cookie) (state string, cookie *http.Cookie, rep reply) {
	g.t.Helper()
	rep = g.do("POST", "/auth/providers/"+slug+"/start", body, cookies...)
	var out apiclient.SignInOutcome
	rep.decode(g.t, &out)
	if out.Redirect == "" {
		return "", nil, rep
	}
	u, err := url.Parse(out.Redirect)
	if err != nil {
		g.t.Fatal(err)
	}
	return u.Query().Get("state"), rep.cookie(providerCookie), rep
}

// callback delivers the provider's answer.
func (g *rig) callback(slug, state string, cookie *http.Cookie, extra map[string]string) apiclient.SignInOutcome {
	g.t.Helper()
	params := map[string]string{"state": state, "code": "the-code"}
	for k, v := range extra {
		params[k] = v
	}
	rep := g.do("POST", "/auth/providers/"+slug+"/callback", params, cookie)
	var out apiclient.SignInOutcome
	rep.decode(g.t, &out)
	if out.Status == 0 && rep.Code != 200 {
		out.Status = rep.Code
	}
	return out
}

// pemOf makes a PKCS#8 private key in PEM, the form Apple hands out.
func pemOf(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestStartingAProviderSignInFailsCleanly(t *testing.T) {
	t.Run("an unknown provider is not found", func(t *testing.T) {
		g := newRig(t)
		rep := g.do("POST", "/auth/providers/nobody/start", map[string]any{})
		wantStatus(t, rep, http.StatusNotFound, "unknown provider")
	})
	t.Run("a provider table that is gone fails the request", func(t *testing.T) {
		g := newRig(t)
		breakTable(t, g.st, "auth_providers")
		wantStatus(t, g.do("POST", "/auth/providers/idp/start", map[string]any{}), http.StatusInternalServerError, "no providers")
		wantStatus(t, g.do("POST", "/auth/providers/idp/callback", map[string]string{}), http.StatusInternalServerError, "no providers")
	})
	t.Run("a provider that cannot be discovered says it is not available", func(t *testing.T) {
		g := newRig(t)
		idp := newOAuthFake(t)
		p := idp.oidcProvider(models.ProviderKindOIDC)
		idp.Close()
		g.addProvider(p)
		logs := captureSignInLog(t)
		var out apiclient.SignInOutcome
		rep := g.do("POST", "/auth/providers/idp/start", map[string]any{})
		rep.decode(t, &out)
		if out.Status != http.StatusBadGateway || !strings.Contains(out.Error, "Test IdP sign-in is not available") || !strings.Contains(logs.String(), "provider setup failed") {
			t.Fatalf("answer %+v, log %s", out, logs)
		}
		if rep.cookie(providerCookie) != nil {
			t.Fatal("a state cookie was set although the provider could not be reached")
		}
	})
	t.Run("an Apple provider with a key that is not usable says it is not available", func(t *testing.T) {
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		for name, key := range map[string]string{
			"text":      "not a key",
			"not PKCS8": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")})),
			"RSA key":   pemOf(t, rsaKey),
		} {
			t.Run(name, func(t *testing.T) {
				g := newRig(t)
				idp := newOAuthFake(t)
				p := idp.oidcProvider(models.ProviderKindApple)
				p.ClientSecret = key
				g.addProvider(p)
				var out apiclient.SignInOutcome
				g.do("POST", "/auth/providers/idp/start", map[string]any{}).decode(t, &out)
				if out.Status != http.StatusBadGateway {
					t.Fatalf("answer %+v", out)
				}
			})
		}
	})
	t.Run("Apple's sign-in asks for a form post", func(t *testing.T) {
		g := newRig(t)
		idp := newOAuthFake(t)
		ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		p := idp.oidcProvider(models.ProviderKindApple)
		p.ClientSecret = pemOf(t, ec)
		g.addProvider(p)
		var out apiclient.SignInOutcome
		g.do("POST", "/auth/providers/idp/start", map[string]any{}).decode(t, &out)
		if !strings.Contains(out.Redirect, "response_mode=form_post") {
			t.Fatalf("redirect %q", out.Redirect)
		}
	})
	t.Run("the state cookie is for a secure connection only when the visitor's is", func(t *testing.T) {
		g := newRig(t)
		g.addProvider(newOAuthFake(t).provider())
		req := httptest.NewRequest("POST", apiclient.Prefix+"/auth/providers/idp/start", strings.NewReader("{}"))
		req.Header.Set(apiclient.HeaderService, rigSecret)
		req.Header.Set(apiclient.HeaderSecure, "1")
		rec := httptest.NewRecorder()
		g.h.ServeHTTP(rec, req)
		var got *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == providerCookie {
				got = c
			}
		}
		if got == nil || !got.Secure || got.SameSite != http.SameSiteNoneMode {
			t.Fatalf("state cookie %+v", got)
		}
	})
	t.Run("linking needs the current password", func(t *testing.T) {
		g := newRig(t)
		g.addProvider(newOAuthFake(t).provider())
		_, session := g.signUp("alice")
		rep := g.do("POST", "/auth/providers/idp/start", map[string]any{"Link": true, "Current": "not the password"}, session)
		if rep.Code == http.StatusOK || rep.cookie(providerCookie) != nil {
			t.Fatalf("linking without the password: %d %s", rep.Code, rep.body())
		}
	})
}

func TestFinishingAProviderSignInRefusesWhatDoesNotMatch(t *testing.T) {
	g := newRig(t)
	idp := newOAuthFake(t)
	g.addProvider(idp.provider())
	other := idp.provider()
	other.Slug = "other"
	g.addProvider(other)
	state, cookie, _ := g.beginProvider("idp", map[string]any{})

	t.Run("without the state cookie", func(t *testing.T) {
		if out := g.callback("idp", state, nil, nil); out.Status != http.StatusBadRequest {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("with another provider's cookie", func(t *testing.T) {
		if out := g.callback("other", state, cookie, nil); out.Status != http.StatusBadRequest {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("with a different state", func(t *testing.T) {
		if out := g.callback("idp", "forged", cookie, nil); out.Status != http.StatusBadRequest {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("with a forged cookie", func(t *testing.T) {
		if out := g.callback("idp", state, &http.Cookie{Name: providerCookie, Value: "x.y"}, nil); out.Status != http.StatusBadRequest {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("with a cookie made for the second step", func(t *testing.T) {
		sealed, _, _ := g.b.sealValue(sealMFA, providerState{Slug: "idp", State: state}, 60e9)
		if out := g.callback("idp", state, &http.Cookie{Name: providerCookie, Value: sealed}, nil); out.Status != http.StatusBadRequest {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("with an expired cookie", func(t *testing.T) {
		sealed, _, _ := g.b.sealValue(sealProvider, providerState{Slug: "idp", State: state}, -60e9)
		if out := g.callback("idp", state, &http.Cookie{Name: providerCookie, Value: sealed}, nil); out.Status != http.StatusBadRequest {
			t.Fatalf("%+v", out)
		}
	})
}

func TestAProviderThatDoesNotCompleteTheSignInIsReportedToTheVisitor(t *testing.T) {
	g := newRig(t)
	idp := newOAuthFake(t)
	g.addProvider(idp.provider())
	uid, session := g.signUp("alice")

	t.Run("the provider reports an error", func(t *testing.T) {
		state, cookie, _ := g.beginProvider("idp", map[string]any{})
		out := g.callback("idp", state, cookie, map[string]string{"error": "access_denied", "code": ""})
		if out.Status != http.StatusBadGateway || out.Error != "Test IdP did not complete the sign-in." {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("a visitor who was linking is sent back to the settings with the message", func(t *testing.T) {
		state, cookie, _ := g.beginProvider("idp", map[string]any{"Link": true, "Current": testPassword}, session)
		out := g.callback("idp", state, cookie, map[string]string{"error": "access_denied"})
		if !strings.HasPrefix(out.Redirect, fmt.Sprintf("/u/%d/settings", uid)) || !strings.Contains(out.Redirect, "did+not+complete") {
			t.Fatalf("%+v", out)
		}
	})
}

func TestTheUserinfoOfAPlainOAuthProviderIsTreatedAsUntrusted(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*oauthFake)
		message string
	}{
		{"the token endpoint refuses", func(f *oauthFake) { f.tokenStatus, f.tokenBody = 400, `{"error":"invalid_grant"}` }, "Could not sign in with Test IdP."},
		{"the user-info endpoint answers an error", func(f *oauthFake) { f.userinfoStatus = 500 }, "Could not sign in with Test IdP."},
		{"the user-info endpoint answers garbage", func(f *oauthFake) { f.userinfoBody = "<html>" }, "Could not sign in with Test IdP."},
		{"the user-info has no subject", func(f *oauthFake) { f.userinfoBody = `{"name":"Pat"}` }, "Could not sign in with Test IdP."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t)
			idp := newOAuthFake(t)
			g.addProvider(idp.provider())
			idp.set(c.setup)
			state, cookie, _ := g.beginProvider("idp", map[string]any{})
			out := g.callback("idp", state, cookie, nil)
			if out.Status != http.StatusBadGateway || out.Error != c.message {
				t.Fatalf("%+v", out)
			}
			if n, _ := g.st.ListUsers(context.Background()); len(n) != 0 {
				t.Fatalf("an account was made from a failed sign-in")
			}
		})
	}
	t.Run("the user-info address is not usable", func(t *testing.T) {
		g := newRig(t)
		idp := newOAuthFake(t)
		p := idp.provider()
		p.UserinfoURL = "http://[::1"
		g.addProvider(p)
		state, cookie, _ := g.beginProvider("idp", map[string]any{})
		if out := g.callback("idp", state, cookie, nil); out.Status != http.StatusBadGateway {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("the user-info server is gone", func(t *testing.T) {
		g := newRig(t)
		idp := newOAuthFake(t)
		p := idp.provider()
		p.UserinfoURL = "http://127.0.0.1:1/userinfo"
		g.addProvider(p)
		state, cookie, _ := g.beginProvider("idp", map[string]any{})
		if out := g.callback("idp", state, cookie, nil); out.Status != http.StatusBadGateway {
			t.Fatalf("%+v", out)
		}
	})
}

func TestAnOpenIDProviderMustAnswerWithAnIDTokenThatVerifies(t *testing.T) {
	for name, body := range map[string]string{
		"no id_token":     `{"access_token":"at","token_type":"Bearer"}`,
		"a forged token":  `{"access_token":"at","token_type":"Bearer","id_token":"a.b.c"}`,
		"a garbled token": `{"access_token":"at","token_type":"Bearer","id_token":"nonsense"}`,
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t)
			idp := newOAuthFake(t)
			g.addProvider(idp.oidcProvider(models.ProviderKindOIDC))
			idp.set(func(f *oauthFake) { f.tokenBody = body })
			state, cookie, _ := g.beginProvider("idp", map[string]any{})
			out := g.callback("idp", state, cookie, nil)
			if out.Status != http.StatusBadGateway || out.Redirect != "" {
				t.Fatalf("%+v", out)
			}
		})
	}
	t.Run("a provider whose discovery vanished after the start", func(t *testing.T) {
		g := newRig(t)
		idp := newOAuthFake(t)
		g.addProvider(idp.oidcProvider(models.ProviderKindOIDC))
		state, cookie, _ := g.beginProvider("idp", map[string]any{})
		g.b.forgetOIDCProvider(idp.URL)
		idp.Close()
		if out := g.callback("idp", state, cookie, nil); out.Status != http.StatusBadGateway {
			t.Fatalf("%+v", out)
		}
	})
}

func TestAPlainOAuthSignInMatchesAccountsBySubjectOnly(t *testing.T) {
	g := newRig(t)
	idp := newOAuthFake(t)
	p := idp.provider()
	p.SubjectClaim = "id"
	g.addProvider(p)
	idp.set(func(f *oauthFake) { f.userinfoBody = `{"id":12345,"login":"pat"}` })

	state, cookie, _ := g.beginProvider("idp", map[string]any{})
	out := g.callback("idp", state, cookie, nil)
	if out.Redirect == "" || out.Error != "" {
		t.Fatalf("first sign-in: %+v", out)
	}
	if _, err := g.st.GetIdentity(context.Background(), "idp", "12345"); err != nil {
		t.Fatalf("the numeric subject was not kept as text: %v", err)
	}
	users, _ := g.st.ListUsers(context.Background())
	if len(users) != 1 || users[0].Name != "idp-12345" {
		t.Fatalf("accounts %+v, want one named after the provider and the subject", users)
	}
	state, cookie, _ = g.beginProvider("idp", map[string]any{})
	if out = g.callback("idp", state, cookie, nil); out.Redirect == "" {
		t.Fatalf("second sign-in: %+v", out)
	}
	if users, _ = g.st.ListUsers(context.Background()); len(users) != 1 {
		t.Fatalf("the second sign-in made %d accounts", len(users))
	}
}

func TestAProviderSignInFailsCleanlyWhenTheStoreLetsItDown(t *testing.T) {
	prepare := func(t *testing.T) (*rig, *oauthFake) {
		g := newRig(t)
		idp := newOAuthFake(t)
		g.addProvider(idp.provider())
		return g, idp
	}
	t.Run("the identity cannot be looked up", func(t *testing.T) {
		g, _ := prepare(t)
		state, cookie, _ := g.beginProvider("idp", map[string]any{})
		breakTable(t, g.st, "user_identities")
		wantStatus(t, g.do("POST", "/auth/providers/idp/callback", map[string]string{"state": state, "code": "c"}, cookie), http.StatusInternalServerError, "identity")
	})
	t.Run("the account of a known identity cannot be read", func(t *testing.T) {
		g, _ := prepare(t)
		uid, _ := g.signUp("alice")
		if err := g.st.PutIdentity(context.Background(), models.Identity{UserID: uid, Provider: "idp", Subject: "subject-1"}); err != nil {
			t.Fatal(err)
		}
		state, cookie, _ := g.beginProvider("idp", map[string]any{})
		breakTable(t, g.st, "users")
		wantStatus(t, g.do("POST", "/auth/providers/idp/callback", map[string]string{"state": state, "code": "c"}, cookie), http.StatusInternalServerError, "account")
	})
	t.Run("the session of a known identity cannot be started", func(t *testing.T) {
		g, _ := prepare(t)
		uid, _ := g.signUp("alice")
		if err := g.st.PutIdentity(context.Background(), models.Identity{UserID: uid, Provider: "idp", Subject: "subject-1"}); err != nil {
			t.Fatal(err)
		}
		state, cookie, _ := g.beginProvider("idp", map[string]any{})
		breakTable(t, g.st, "sessions")
		wantStatus(t, g.do("POST", "/auth/providers/idp/callback", map[string]string{"state": state, "code": "c"}, cookie), http.StatusInternalServerError, "session")
	})
	t.Run("whether sign-up is open cannot be read", func(t *testing.T) {
		g, _ := prepare(t)
		state, cookie, _ := g.beginProvider("idp", map[string]any{})
		breakTable(t, g.st, "app_settings")
		wantStatus(t, g.do("POST", "/auth/providers/idp/callback", map[string]string{"state": state, "code": "c"}, cookie), http.StatusInternalServerError, "settings")
	})
	t.Run("the new account cannot be made", func(t *testing.T) {
		g, _ := prepare(t)
		state, cookie, _ := g.beginProvider("idp", map[string]any{})
		breakTable(t, g.st, "users")
		wantStatus(t, g.do("POST", "/auth/providers/idp/callback", map[string]string{"state": state, "code": "c"}, cookie), http.StatusInternalServerError, "new account")
	})
	t.Run("the identity cannot be linked", func(t *testing.T) {
		g, _ := prepare(t)
		_, session := g.signUp("alice")
		state, cookie, _ := g.beginProvider("idp", map[string]any{"Link": true, "Current": testPassword}, session)
		rejectWritesOn(t, g.st, "user_identities", "INSERT")
		wantStatus(t, g.do("POST", "/auth/providers/idp/callback", map[string]string{"state": state, "code": "c"}, cookie), http.StatusInternalServerError, "link")
	})
}

func TestALoginOfAnotherAccountCannotBeLinkedToMe(t *testing.T) {
	g := newRig(t)
	idp := newOAuthFake(t)
	g.addProvider(idp.provider())
	bob, _ := g.signUp("bob")
	if err := g.st.PutIdentity(context.Background(), models.Identity{UserID: bob, Provider: "idp", Subject: "subject-1"}); err != nil {
		t.Fatal(err)
	}
	_, alice := g.signUp("alice")
	state, cookie, _ := g.beginProvider("idp", map[string]any{"Link": true, "Current": testPassword}, alice)
	out := g.callback("idp", state, cookie, nil)
	if !strings.Contains(out.Redirect, "already+linked+to+another+account") {
		t.Fatalf("%+v", out)
	}
	if id, _ := g.st.GetIdentity(context.Background(), "idp", "subject-1"); id.UserID != bob {
		t.Fatalf("the login moved to user %d", id.UserID)
	}
}

func TestProviderNamesAndAddressesAreChosenCarefully(t *testing.T) {
	p := models.AuthProvider{Slug: "idp"}
	if got := providerUserName(p, "42", map[string]any{}); got != "idp-42" {
		t.Errorf("name without claims = %q", got)
	}
	long := strings.Repeat("é", 100)
	if got := providerUserName(p, "42", map[string]any{"name": long}); len(got) > userNameLimit || !strings.HasPrefix(long, got) || got == "" {
		t.Errorf("long name cut to %q", got)
	}
	if got := verifiedEmail(map[string]any{"email_verified": true, "email": "not an address"}); got != "" {
		t.Errorf("an invalid address was trusted: %q", got)
	}
	for name, claims := range map[string]map[string]any{
		"unverified":        {"email_verified": false, "email": "a@example.com"},
		"unverified string": {"email_verified": "false", "email": "a@example.com"},
		"no statement":      {"email": "a@example.com"},
	} {
		if got := verifiedEmail(claims); got != "" {
			t.Errorf("%s: %q was trusted", name, got)
		}
	}
	if got := verifiedEmail(map[string]any{"email_verified": "true", "email": "A@Example.com"}); got != "a@example.com" {
		t.Errorf("verified string = %q", got)
	}
}
