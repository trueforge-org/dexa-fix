package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/store"
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
