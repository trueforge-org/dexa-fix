package mainbackend

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// splitClient is a client for a backend that listens on a real connection, like a separate
// process would, with the visitor's request described by rq.
func splitClient(t *testing.T, bs *httptest.Server) *apiclient.Client {
	t.Helper()
	c, err := apiclient.New(bs.URL, nil, "current-secret-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSplitFailedLoginIsAuditedUnderTheForwardedAddress(t *testing.T) {
	bs, srv, _ := newInternalServer(t)
	c := splitClient(t, bs)
	ctx := apiclient.WithRequest(context.Background(), apiclient.Request{ClientIP: "203.0.113.9", Host: "dexa.example.com", Secure: true})

	o, err := c.Login(ctx, "root@example.com", "wrong password")
	if err != nil || o.Error == "" || o.Status != http.StatusUnauthorized || o.Redirect != "" {
		t.Fatalf("outcome %+v err %v", o, err)
	}
	events, err := srv.store.ListAuditEvents(context.Background(), store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.Action == auditLoginFailed {
			found = true
			if e.IP != "203.0.113.9" {
				t.Errorf("failed login recorded for %q, want the forwarded address", e.IP)
			}
		}
	}
	if !found {
		t.Error("no failed-login event")
	}
}

func TestSplitFailuresThrottleTheForwardedAddressNotTheFrontend(t *testing.T) {
	bs, _, _ := newInternalServer(t)
	c := splitClient(t, bs)
	noisy := apiclient.WithRequest(context.Background(), apiclient.Request{ClientIP: "203.0.113.9"})
	for i := range maxIPFail + 1 { // a new login name each time, so only the address accumulates
		c.Login(noisy, fmt.Sprintf("nobody%d@example.com", i), "x")
	}
	if o, _ := c.Login(noisy, "root@example.com", testPassword); o.Status != http.StatusTooManyRequests {
		t.Errorf("the noisy visitor was not throttled: %+v", o)
	}
	// Another visitor behind the same frontend is not affected.
	quiet := apiclient.WithRequest(context.Background(), apiclient.Request{ClientIP: "198.51.100.7"})
	if o, err := c.Login(quiet, "root@example.com", testPassword); err != nil || o.Redirect == "" {
		t.Errorf("another visitor was locked out: %+v %v", o, err)
	}
}

func TestSplitSessionCookieReachesTheRelay(t *testing.T) {
	bs, _, _ := newInternalServer(t)
	c := splitClient(t, bs)
	rec := httptest.NewRecorder()
	ctx := apiclient.WithRequest(context.Background(), apiclient.Request{ClientIP: "198.51.100.7", Secure: true, Relay: rec})
	o, err := c.Login(ctx, "root@example.com", testPassword)
	if err != nil || o.Redirect != "/u/1" {
		t.Fatalf("outcome %+v err %v", o, err)
	}
	var sess *http.Cookie
	for _, ck := range (&http.Response{Header: rec.Header()}).Cookies() {
		if ck.Name == sessionCookie {
			sess = ck
		}
	}
	if sess == nil || !sess.HttpOnly || !sess.Secure || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" {
		t.Fatalf("session cookie %+v", sess)
	}
	// The cookie signs the visitor in on the backend.
	back := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: sessionCookie + "=" + sess.Value})
	if u, err := c.Session(back); err != nil || u == nil || u.Email != "root@example.com" {
		t.Errorf("session %+v %v", u, err)
	}
}

func newClientWithSecret(base, secret string) (*apiclient.Client, error) {
	return apiclient.New(base, nil, secret)
}

func relayedCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, ck := range (&http.Response{Header: rec.Header()}).Cookies() {
		if ck.Name == name && ck.MaxAge >= 0 {
			return ck
		}
	}
	return nil
}

func TestSplitSecondStepUsesOnlyForwardedCookies(t *testing.T) {
	bs, srv, _ := newInternalServer(t)
	tfEnable(t, srv, 1)
	c := splitClient(t, bs)

	first := httptest.NewRecorder()
	ctx := apiclient.WithRequest(context.Background(), apiclient.Request{ClientIP: "198.51.100.7", Secure: true, Relay: first})
	o, err := c.Login(ctx, "root@example.com", testPassword)
	if err != nil || o.Redirect != "/login/2fa" {
		t.Fatalf("login outcome %+v err %v", o, err)
	}
	mfa := relayedCookie(first, mfaCookie)
	if mfa == nil || !mfa.HttpOnly || !mfa.Secure || mfa.Path != "/login/" {
		t.Fatalf("mfa cookie %+v", mfa)
	}
	if relayedCookie(first, sessionCookie) != nil {
		t.Fatal("a session was started before the second step")
	}
	withMFA := func(rec http.ResponseWriter) context.Context {
		return apiclient.WithRequest(context.Background(), apiclient.Request{ClientIP: "198.51.100.7", Secure: true, Cookie: mfaCookie + "=" + mfa.Value, Relay: rec})
	}

	// Without the cookie there is nothing to continue.
	if st, err := c.LoginSecondStepPage(apiclient.WithRequest(context.Background(), apiclient.Request{})); err != nil || st.Redirect != "/" {
		t.Errorf("no cookie: %+v %v", st, err)
	}
	if st, err := c.LoginSecondStepPage(withMFA(nil)); err != nil || st.Mode != "verify" || st.Redirect != "" {
		t.Errorf("page: %+v %v", st, err)
	}
	// A wrong code is refused and keeps the cookie usable.
	if st, err := c.LoginSecondStep(withMFA(nil), "000000"); err != nil || st.Status != http.StatusUnauthorized || st.Error == "" {
		t.Errorf("wrong code: %+v %v", st, err)
	}

	second := httptest.NewRecorder()
	st, err := c.LoginSecondStep(withMFA(second), tfCode(t, srv, 1))
	if err != nil || st.Redirect != "/u/1" {
		t.Fatalf("second step %+v err %v", st, err)
	}
	sess := relayedCookie(second, sessionCookie)
	if sess == nil {
		t.Fatal("the session cookie did not reach the relay")
	}
	if cleared := second.Header().Values("Set-Cookie"); !strings.Contains(strings.Join(cleared, "\n"), mfaCookie+"=;") {
		t.Errorf("the mfa cookie was not cleared: %v", cleared)
	}
	back := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: sessionCookie + "=" + sess.Value})
	if u, err := c.Session(back); err != nil || u == nil || u.ID != 1 {
		t.Errorf("session %+v %v", u, err)
	}

	// The state that was used once signs nobody in again, even with a fresh code.
	replay := httptest.NewRecorder()
	st, err = c.LoginSecondStep(withMFA(replay), tfCode(t, srv, 1))
	if err != nil || st.Redirect != "/" || relayedCookie(replay, sessionCookie) != nil {
		t.Errorf("replayed state: %+v %v session=%v", st, err, relayedCookie(replay, sessionCookie))
	}
}

// recordingBackend wraps the backend and records what the requests looked like on the wire.
type recordingBackend struct {
	next http.Handler
	mu   sync.Mutex
	urls []string
	tok  []string
}

func (b *recordingBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.urls = append(b.urls, r.URL.String())
	if v := r.Header.Get(apiclient.HeaderResetToken); v != "" {
		b.tok = append(b.tok, v)
	}
	b.mu.Unlock()
	b.next.ServeHTTP(w, r)
}

func TestSplitResetTokenTravelsInAHeaderNotTheURL(t *testing.T) {
	bs, srv, _ := newInternalServer(t)
	rec := &recordingBackend{next: bs.Config.Handler}
	spy := httptest.NewServer(rec)
	t.Cleanup(spy.Close)
	c := splitClient(t, spy)
	ctx := apiclient.WithRequest(context.Background(), apiclient.Request{ClientIP: "198.51.100.7"})

	token, _, err := srv.newResetToken(context.Background(), 1, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := c.ResetFormPage(ctx, token); err != nil || st.Mode != "form" || st.NeedsCode {
		t.Fatalf("form page %+v %v", st, err)
	}
	if st, err := c.ResetFormPage(ctx, "not-a-token"); err != nil || st.Mode != "invalid" || st.Status != http.StatusNotFound {
		t.Fatalf("bad token %+v %v", st, err)
	}
	relay := httptest.NewRecorder()
	ctx = apiclient.WithRequest(context.Background(), apiclient.Request{ClientIP: "198.51.100.7", Relay: relay})
	st, err := c.Reset(ctx, token, "a brand new password", "a brand new password", "")
	if err != nil || st.Redirect != "/u/1" {
		t.Fatalf("reset %+v %v", st, err)
	}
	if relayedCookie(relay, sessionCookie) == nil {
		t.Error("the session cookie did not reach the relay")
	}
	if o, err := c.Login(ctx, "root@example.com", "a brand new password"); err != nil || o.Redirect == "" {
		t.Errorf("new password: %+v %v", o, err)
	}
	// The link works once.
	if st, err := c.Reset(ctx, token, "another password 123", "another password 123", ""); err != nil || st.Mode != "invalid" {
		t.Errorf("second use: %+v %v", st, err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.tok) == 0 {
		t.Fatal("no request carried the token header")
	}
	for _, tk := range rec.tok {
		if tk != token && tk != "not-a-token" {
			t.Errorf("unexpected token header %q", tk)
		}
	}
	for _, u := range rec.urls {
		if strings.Contains(u, token) || strings.Contains(u, "not-a-token") {
			t.Errorf("the token is in the request URL %q", u)
		}
	}
}

func TestSplitPasskeyCeremonyCookieReachesTheRelay(t *testing.T) {
	bs, _, _ := newInternalServer(t)
	c := splitClient(t, bs)
	rec := httptest.NewRecorder()
	ctx := apiclient.WithRequest(context.Background(), apiclient.Request{ClientIP: "198.51.100.7", Host: "localhost:4000", Relay: rec})
	opts, err := c.PasskeyLoginBegin(ctx)
	if err != nil || !strings.Contains(string(opts), "publicKey") {
		t.Fatalf("begin %s %v", opts, err)
	}
	ck := relayedCookie(rec, passkeyCookie)
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" {
		t.Fatalf("ceremony cookie %+v", ck)
	}
	// Without the cookie there is no ceremony to finish; the refusal keeps its status and text.
	_, err = c.PasskeyLoginFinish(apiclient.WithRequest(context.Background(), apiclient.Request{Host: "localhost:4000"}), strings.NewReader("{}"))
	if apiclient.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("finish without cookie: %v", err)
	}
	// With the cookie the backend opens it (the sealing key never left the backend) and parses the
	// forwarded body itself: garbage fails the assertion, not the cookie.
	withCookie := apiclient.WithRequest(context.Background(), apiclient.Request{Host: "localhost:4000", Cookie: passkeyCookie + "=" + ck.Value})
	_, err = c.PasskeyLoginFinish(withCookie, strings.NewReader("{}"))
	if apiclient.StatusOf(err) != http.StatusUnauthorized || !strings.Contains(err.Error(), passkeyLoginFailed) {
		t.Errorf("finish with garbage: %v", err)
	}
}

func TestSplitAdminEndpointRefusesWithoutStepUpProof(t *testing.T) {
	_, srv, sess := newInternalServer(t)
	tfEnable(t, srv, 1)
	// A test-only admin endpoint behind the guard, served like the real internal API.
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+apiclient.Prefix+"/test/admin", srv.internalAdmin(func(w http.ResponseWriter, r *http.Request, u models.User) {
		internalJSON(w, http.StatusOK, map[string]string{"admin": u.Email})
	}))
	mux.HandleFunc(apiclient.Prefix+"/", srv.InternalHandler().ServeHTTP)
	ts := httptest.NewServer(srv.serviceAuth(srv.visitorContext(srv.withAuth(mux))))
	t.Cleanup(ts.Close)
	c := splitClient(t, ts)
	call := func(cookie string, relay http.ResponseWriter) error {
		ctx := apiclient.WithRequest(context.Background(), apiclient.Request{ClientIP: "198.51.100.7", Cookie: cookie, Relay: relay})
		return c.Get(ctx, "/test/admin", nil)
	}
	signedIn := sessionCookie + "=" + sess.Value

	if err := call("", nil); !apiclient.IsCode(err, apiclient.CodeUnauthorized) {
		t.Errorf("anonymous: %v", err)
	}
	if err := call(signedIn, nil); !apiclient.IsCode(err, apiclient.CodeStepUp) || apiclient.StatusOf(err) != http.StatusForbidden {
		t.Errorf("signed in without the proof: %v", err)
	}
	ctx := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: signedIn})
	if need, err := c.AdminStepUp(ctx); err != nil || !need {
		t.Errorf("step-up check: %v %v", need, err)
	}
	// A wrong code does not give the proof; a right one does, and only the cookie carries it.
	if o, err := c.AdminVerify(ctx, "000000"); err != nil || o.Status != http.StatusUnauthorized || o.Error == "" {
		t.Errorf("wrong code: %+v %v", o, err)
	}
	rec := httptest.NewRecorder()
	vctx := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: signedIn, Relay: rec})
	if o, err := c.AdminVerify(vctx, tfCode(t, srv, 1)); err != nil || o.Redirect != "/admin" {
		t.Fatalf("verify: %+v %v", o, err)
	}
	proof := relayedCookie(rec, adminCookie)
	if proof == nil || proof.Path != "/admin" {
		t.Fatalf("proof cookie %+v", proof)
	}
	if err := call(signedIn+"; "+adminCookie+"="+proof.Value, nil); err != nil {
		t.Errorf("with the proof: %v", err)
	}
}
