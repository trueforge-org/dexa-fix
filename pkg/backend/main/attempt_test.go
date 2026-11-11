package mainbackend

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// flakyState is a state store whose recording or counting of failures can be made to fail.
type flakyState struct {
	StateStore
	failRecord, failCount bool
}

func (f flakyState) RecordFailure(ctx context.Context, key string, at time.Time) error {
	if f.failRecord {
		return errors.New("cannot record")
	}
	return f.StateStore.RecordFailure(ctx, key, at)
}

func (f flakyState) CountFailures(ctx context.Context, key string, since time.Time) (int, error) {
	if f.failCount {
		return 0, errors.New("cannot count")
	}
	return f.StateStore.CountFailures(ctx, key, since)
}

func TestAnAttemptIsCountedBeforeItIsDecided(t *testing.T) {
	ctx := context.Background()

	t.Run("exactly max attempts pass, one after the other", func(t *testing.T) {
		g := newRig(t)
		for i := 1; i <= 5; i++ {
			if !g.b.limiter.Attempt(ctx, "k", 5) {
				t.Fatalf("attempt %d of 5 was refused", i)
			}
		}
		if g.b.limiter.Attempt(ctx, "k", 5) {
			t.Fatal("the sixth attempt passed")
		}
	})
	t.Run("never more than max of many parallel attempts pass", func(t *testing.T) {
		g := newRig(t)
		var passed atomic.Int32
		var wg sync.WaitGroup
		for range 40 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if g.b.limiter.Attempt(ctx, "burst", 5) {
					passed.Add(1)
				}
			}()
		}
		wg.Wait()
		// Each attempt is on record before it is counted, so the ones that count late see the
		// others too: fewer than max may pass, never more. A check that counted after the fact
		// would let all 40 through.
		if got := passed.Load(); got > 5 {
			t.Fatalf("%d of 40 parallel attempts passed, want at most 5", got)
		}
	})
	t.Run("a right answer resets the count", func(t *testing.T) {
		g := newRig(t)
		for range 5 {
			g.b.limiter.Attempt(ctx, "k", 5)
		}
		g.b.limiter.Reset(ctx, "k")
		if !g.b.limiter.Attempt(ctx, "k", 5) {
			t.Fatal("the first attempt after a reset was refused")
		}
	})
	t.Run("a store that cannot record or count refuses", func(t *testing.T) {
		g := newRig(t)
		if newFailureLimiter(flakyState{StateStore: g.st, failRecord: true}).Attempt(ctx, "k", 5) {
			t.Fatal("an attempt that could not be recorded passed")
		}
		if newFailureLimiter(flakyState{StateStore: g.st, failCount: true}).Attempt(ctx, "k", 5) {
			t.Fatal("an attempt that could not be counted passed")
		}
	})
}

// racedState is a state store in which other attempts land between the read-only check and the
// attempt that is counted: the first passFirst counts say "nothing yet", later ones say "full".
type racedState struct {
	StateStore
	calls     atomic.Int32
	passFirst int32
}

func (r *racedState) CountFailures(ctx context.Context, key string, since time.Time) (int, error) {
	if r.calls.Add(1) <= r.passFirst {
		return 0, nil
	}
	return 1000, nil
}

func TestAnAttemptThatLosesTheRaceIsRefusedBeforeThePasswordIsChecked(t *testing.T) {
	t.Run("a sign-in", func(t *testing.T) {
		g := newRig(t)
		g.signUp("alice")
		g.b.limiter = newFailureLimiter(&racedState{StateStore: g.st, passFirst: 2}) // the address and the login pass the check
		rep := g.do("POST", "/auth/password", map[string]string{"Email": "alice@example.com", "Password": testPassword})
		var out apiclient.SignInOutcome
		rep.decode(t, &out)
		if out.Status != http.StatusTooManyRequests || out.Redirect != "" || !strings.Contains(out.Error, "Too many failed attempts") {
			t.Fatalf("a right password that lost the race: %+v, want it refused as throttled", out)
		}
	})
	t.Run("a confirmation of the password", func(t *testing.T) {
		ts, c, _ := newAuthServer(t)
		signup(t, ts, c, "alice")
		srv := serverOf(ts)
		u, err := srv.store.GetUser(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		srv.limiter = newFailureLimiter(&racedState{StateStore: srv.store, passFirst: 2})
		ok, status, msg, err := srv.checkReauth(reauthRequest(url.Values{"current": {testPassword}}, ""), u, false)
		if err != nil || ok || status != http.StatusTooManyRequests || !strings.Contains(msg, "Too many wrong passwords") {
			t.Fatalf("ok=%v status=%d msg=%q err=%v, want a right password that lost the race refused", ok, status, msg, err)
		}
	})
}
