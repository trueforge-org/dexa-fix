package mainbackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// fillSlots takes every password check slot and frees them again when the test ends.
func fillSlots(t *testing.T, b *Backend) {
	t.Helper()
	old := bcryptWait
	bcryptWait = 20 * time.Millisecond
	t.Cleanup(func() { bcryptWait = old })
	for range cap(b.bcryptSlots) {
		b.bcryptSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(b.bcryptSlots) {
			<-b.bcryptSlots
		}
	})
}

func TestPasswordChecksOfTheWebWaitForASlotAndThenGiveUp(t *testing.T) {
	t.Run("sign-in says the server is busy and counts no failure", func(t *testing.T) {
		g := newRig(t)
		g.signUp("alice")
		fillSlots(t, g.b)
		rep := g.do("POST", "/auth/password", map[string]string{"Email": "alice@example.com", "Password": testPassword})
		var out apiclient.SignInOutcome
		rep.decode(t, &out)
		if out.Status != http.StatusServiceUnavailable || !strings.Contains(out.Error, "busy") || out.Redirect != "" {
			t.Fatalf("sign-in with every slot busy: %+v", out)
		}
		if n, _ := g.b.limiter.st.CountFailures(context.Background(), loginKey("alice@example.com"), time.Now().Add(-time.Hour)); n != 0 {
			t.Fatalf("a busy server counted %d failed attempts against the account", n)
		}
	})
	t.Run("confirming the password is answered as throttled", func(t *testing.T) {
		ts, c, _ := newAuthServer(t)
		signup(t, ts, c, "alice")
		srv := serverOf(ts)
		u, err := srv.store.GetUser(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		fillSlots(t, srv.Backend)
		ok, status, msg, err := srv.checkReauth(reauthRequest(url.Values{"current": {testPassword}}, ""), u, false)
		if err != nil || ok || status != http.StatusTooManyRequests || !strings.Contains(msg, "busy") {
			t.Fatalf("ok=%v status=%d msg=%q err=%v, want refused with 429", ok, status, msg, err)
		}
	})
	t.Run("a visitor who left does not wait for a slot", func(t *testing.T) {
		g := newRig(t)
		fillSlots(t, g.b)
		old := bcryptWait
		bcryptWait = time.Hour // the visitor must be what ends the wait
		t.Cleanup(func() { bcryptWait = old })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if release, ok := g.b.bcryptSlot(ctx); ok || release != nil {
			t.Fatal("a slot was given to a visitor who had gone")
		}
	})
	t.Run("a free slot is taken and given back", func(t *testing.T) {
		g := newRig(t)
		release, ok := g.b.bcryptSlot(httptest.NewRequest("GET", "/", nil).Context())
		if !ok || len(g.b.bcryptSlots) != 1 {
			t.Fatalf("ok=%v, slots in use %d, want 1", ok, len(g.b.bcryptSlots))
		}
		release()
		if len(g.b.bcryptSlots) != 0 {
			t.Fatalf("slots in use after release: %d", len(g.b.bcryptSlots))
		}
	})
}
