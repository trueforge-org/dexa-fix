package mainbackend

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/store"
)

// gapSessions is the session store of a backend where somebody signs in with the old password in
// the moment between ending a user's sessions and writing the new password: after the first
// DeleteUserSessions it plants a session of that user. failOn is the call that fails (0: none).
type gapSessions struct {
	sessionStore
	mu     sync.Mutex
	calls  int
	failOn int
}

const plantedSession = "planted-by-a-sign-in-in-the-gap"

func (g *gapSessions) DeleteUserSessions(ctx context.Context, userID int64) error {
	g.mu.Lock()
	g.calls++
	n := g.calls
	g.mu.Unlock()
	if n == g.failOn {
		return errors.New("the sessions are unavailable")
	}
	if err := g.sessionStore.DeleteUserSessions(ctx, userID); err != nil {
		return err
	}
	if n == 1 {
		return g.sessionStore.CreateSession(ctx, hashToken(plantedSession), userID, "password", time.Now().Add(time.Hour))
	}
	return nil
}

func (g *gapSessions) survives(t *testing.T) bool {
	t.Helper()
	_, err := g.sessionStore.GetSession(context.Background(), hashToken(plantedSession))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func TestASessionMadeInTheGapOfAPasswordChangeDoesNotSurviveIt(t *testing.T) {
	const next = "a brand new password"

	t.Run("a user changes the password", func(t *testing.T) {
		ts, c, _ := newAuthServer(t)
		signup(t, ts, c, "alice")
		gap := &gapSessions{sessionStore: serverOf(ts).sessions}
		serverOf(ts).sessions = gap
		r := post(t, c, ts.URL+"/u/1/password", url.Values{"current": {testPassword}, "password": {next}, "confirm": {next}})
		if r.StatusCode != http.StatusOK {
			t.Fatalf("change: %d", r.StatusCode)
		}
		if gap.survives(t) {
			t.Fatal("a session made between ending the sessions and writing the password survived the change")
		}
	})
	t.Run("a user changes the password and the second ending fails", func(t *testing.T) {
		ts, c, _ := newAuthServer(t)
		signup(t, ts, c, "alice")
		serverOf(ts).sessions = &gapSessions{sessionStore: serverOf(ts).sessions, failOn: 2}
		r := post(t, c, ts.URL+"/u/1/password", url.Values{"current": {testPassword}, "password": {next}, "confirm": {next}})
		if r.StatusCode < http.StatusInternalServerError {
			t.Fatalf("change with sessions that cannot be ended: %d, want a server error", r.StatusCode)
		}
	})
	t.Run("an administrator sets the password", func(t *testing.T) {
		ts, admin, _ := newAuthServer(t)
		signup(t, ts, admin, "root")
		signup(t, ts, noRedirectClient(), "bob")
		gap := &gapSessions{sessionStore: serverOf(ts).sessions}
		serverOf(ts).sessions = gap
		r := post(t, admin, ts.URL+"/admin/users/2/password", url.Values{"password": {next}})
		if r.StatusCode != http.StatusSeeOther {
			t.Fatalf("set: %d", r.StatusCode)
		}
		if gap.survives(t) {
			t.Fatal("a session made between ending the sessions and writing the password survived")
		}
	})
	t.Run("an administrator sets the password and the second ending fails", func(t *testing.T) {
		ts, admin, _ := newAuthServer(t)
		signup(t, ts, admin, "root")
		signup(t, ts, noRedirectClient(), "bob")
		serverOf(ts).sessions = &gapSessions{sessionStore: serverOf(ts).sessions, failOn: 2}
		if r := post(t, admin, ts.URL+"/admin/users/2/password", url.Values{"password": {next}}); r.StatusCode < http.StatusInternalServerError {
			t.Fatalf("set with sessions that cannot be ended: %d, want a server error", r.StatusCode)
		}
	})
	t.Run("a reset link is used", func(t *testing.T) {
		g := newRig(t)
		uid, _ := g.signUp("alice")
		gap := &gapSessions{sessionStore: g.b.sessions}
		g.b.sessions = gap
		token, _, err := g.b.newResetToken(context.Background(), uid, nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		rep := g.doWithToken("POST", "/auth/password/reset", token, map[string]string{"Password": next, "Confirm": next})
		if rep.Code >= http.StatusBadRequest {
			t.Fatalf("reset: %d %s", rep.Code, rep.body())
		}
		if gap.survives(t) {
			t.Fatal("a session made between ending the sessions and the reset survived it")
		}
	})
	t.Run("a reset link is used and the second ending fails", func(t *testing.T) {
		g := newRig(t)
		uid, _ := g.signUp("alice")
		g.b.sessions = &gapSessions{sessionStore: g.b.sessions, failOn: 2}
		token, _, err := g.b.newResetToken(context.Background(), uid, nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus(t, g.doWithToken("POST", "/auth/password/reset", token, map[string]string{"Password": next, "Confirm": next}), http.StatusInternalServerError, "reset")
	})
}
