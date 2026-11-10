package mainbackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// agedSessions is the store with every session reported as created age ago. The sqlstore clock is
// not reachable from this package, so the age is set here instead of in the database.
type agedSessions struct {
	store.Store
	age time.Duration
}

// Unwrap is the store underneath, for testBody to find the body domain in.
func (a agedSessions) Unwrap() store.Store { return a.Store }

func (a agedSessions) GetSession(ctx context.Context, tokenHash string) (models.Session, error) {
	sess, err := a.Store.GetSession(ctx, tokenHash)
	if err == nil {
		sess.CreatedAt = time.Now().Add(-a.age)
	}
	return sess, err
}

// reauthRequest is a POST carrying the form and, when token is set, the session cookie.
func reauthRequest(form url.Values, token string) *http.Request {
	r := httptest.NewRequest("POST", "/u/1/tokens", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	}
	return r
}

// responseSession returns the session cookie value set by resp.
func responseSession(t *testing.T, resp *http.Response) string {
	t.Helper()
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie {
			return ck.Value
		}
	}
	t.Fatalf("no session cookie in %v", resp.Cookies())
	return ""
}

func TestCheckReauthRecentSession(t *testing.T) {
	ts, admin, st := newAuthServer(t)
	ctx := context.Background()
	srv := serverOf(ts)
	signup(t, ts, admin, "root") // user 1, with a password

	idp := newFakeIdP(t)
	addProvider(t, ts, admin, idp, true)
	pl := noRedirectClient()
	r := idp.signInVia(t, ts, pl, "idp") // user 2, without a password
	if r.StatusCode != 303 || r.Header.Get("Location") != "/u/2" {
		t.Fatalf("provider signup: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	token := responseSession(t, r)

	withPassword, err := st.GetUser(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	withoutPassword, err := st.GetUser(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("user without a password passes when not recent", func(t *testing.T) {
		ok, status, msg, err := srv.checkReauth(reauthRequest(nil, ""), withoutPassword, false)
		if err != nil || !ok {
			t.Fatalf("ok=%v status=%d msg=%q err=%v", ok, status, msg, err)
		}
	})

	t.Run("user without a password passes with a session made just now", func(t *testing.T) {
		ok, status, msg, err := srv.checkReauth(reauthRequest(nil, token), withoutPassword, true)
		if err != nil || !ok {
			t.Fatalf("ok=%v status=%d msg=%q err=%v", ok, status, msg, err)
		}
	})

	t.Run("user without a password refused with a session older than the window", func(t *testing.T) {
		aged, err := New(agedSessions{Store: st, age: reauthWindow + time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(aged.Close)
		ok, status, msg, err := aged.checkReauth(reauthRequest(nil, token), withoutPassword, true)
		if err != nil {
			t.Fatal(err)
		}
		if ok || status != http.StatusUnauthorized || !strings.Contains(msg, "Sign in again") {
			t.Fatalf("ok=%v status=%d msg=%q, want refused with 401", ok, status, msg)
		}
	})

	t.Run("user without a password refused without a session cookie", func(t *testing.T) {
		ok, status, _, err := srv.checkReauth(reauthRequest(nil, ""), withoutPassword, true)
		if err != nil {
			t.Fatal(err)
		}
		if ok || status != http.StatusUnauthorized {
			t.Fatalf("ok=%v status=%d, want refused with 401", ok, status)
		}
	})

	t.Run("user with a password needs the current password", func(t *testing.T) {
		ok, status, _, err := srv.checkReauth(reauthRequest(url.Values{}, ""), withPassword, false)
		if err != nil {
			t.Fatal(err)
		}
		if ok || status != http.StatusUnprocessableEntity {
			t.Fatalf("missing current: ok=%v status=%d, want refused with 422", ok, status)
		}

		ok, status, _, err = srv.checkReauth(reauthRequest(url.Values{"current": {"wrong password"}}, ""), withPassword, false)
		if err != nil {
			t.Fatal(err)
		}
		if ok || status != http.StatusUnprocessableEntity {
			t.Fatalf("wrong current: ok=%v status=%d, want refused with 422", ok, status)
		}

		ok, status, msg, err := srv.checkReauth(reauthRequest(url.Values{"current": {testPassword}}, ""), withPassword, false)
		if err != nil || !ok {
			t.Fatalf("right current: ok=%v status=%d msg=%q err=%v", ok, status, msg, err)
		}
	})
}

func TestReservedProviderSlugs(t *testing.T) {
	ts, admin, st := newAuthServer(t)
	ctx := context.Background()
	signup(t, ts, admin, "root")

	for _, slug := range []string{"passkey", "local"} {
		r := post(t, admin, ts.URL+"/admin/providers", url.Values{
			"kind": {"oidc"}, "slug": {slug}, "label": {"Reserved"}, "issuer": {"https://idp.example.com"},
			"client_id": {"client-1"}, "client_secret": {"s3cret-value"},
		})
		if r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), "short+name") {
			t.Errorf("slug %q: %d %s", slug, r.StatusCode, r.Header.Get("Location"))
		}
	}

	providers, err := st.ListAuthProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("reserved slugs were saved: %+v", providers)
	}
}
