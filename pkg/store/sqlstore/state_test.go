package sqlstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/store"
)

func TestNonceIsSingleUseUntilItExpires(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if seen, _ := st.NonceSeen(ctx, "n1"); seen {
		t.Fatal("seen before use")
	}
	if first, err := st.UseNonce(ctx, "n1", time.Now().Add(time.Hour)); err != nil || !first {
		t.Fatalf("first use: %v %v", first, err)
	}
	if first, _ := st.UseNonce(ctx, "n1", time.Now().Add(time.Hour)); first {
		t.Error("the second use was accepted")
	}
	if seen, _ := st.NonceSeen(ctx, "n1"); !seen {
		t.Error("a used nonce is not seen")
	}
	// An expired nonce can be claimed again (the sealed value it belonged to is expired anyway).
	if first, _ := st.UseNonce(ctx, "old", time.Now().Add(-time.Minute)); !first {
		t.Fatal("first use of an expiring nonce")
	}
	if seen, _ := st.NonceSeen(ctx, "old"); seen {
		t.Error("an expired nonce counts as seen")
	}
	if first, _ := st.UseNonce(ctx, "old", time.Now().Add(time.Hour)); !first {
		t.Error("an expired nonce could not be claimed again")
	}
	n, err := st.PurgeNonces(ctx, time.Now().Add(2*time.Hour))
	if err != nil || n != 2 {
		t.Errorf("purged %d (%v)", n, err)
	}
}

// Two callers racing for one nonce: exactly one wins.
func TestNonceRaceHasOneWinner(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if first, err := st.UseNonce(ctx, "race", time.Now().Add(time.Hour)); err == nil && first {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("%d winners", wins.Load())
	}
}

func TestFailuresAreCountedInAWindowAndCleared(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	t0 := time.Now()
	for i := range 3 {
		if err := st.RecordFailure(ctx, "login:a", t0.Add(-time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	st.RecordFailure(ctx, "login:a", t0.Add(-time.Hour)) // outside the window
	st.RecordFailure(ctx, "login:b", t0)
	if n, _ := st.CountFailures(ctx, "login:a", t0.Add(-15*time.Minute)); n != 3 {
		t.Errorf("counted %d, want 3", n)
	}
	if n, _ := st.CountFailures(ctx, "login:b", t0.Add(-15*time.Minute)); n != 1 {
		t.Errorf("another key counted %d", n)
	}
	if err := st.ClearFailures(ctx, "login:a"); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountFailures(ctx, "login:a", t0.Add(-2*time.Hour)); n != 0 {
		t.Errorf("%d left after a clear", n)
	}
	if n, err := st.PurgeFailures(ctx, t0.Add(time.Minute)); err != nil || n != 1 {
		t.Errorf("purged %d (%v)", n, err)
	}
}

func TestStrengthLevelJobStatusIsStoredPerUser(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.GetStrengthLevelJob(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	for _, s := range []string{`{"a":1}`, `{"a":2}`} {
		if err := st.PutStrengthLevelJob(ctx, u.ID, s); err != nil {
			t.Fatal(err)
		}
	}
	got, at, err := st.GetStrengthLevelJob(ctx, u.ID)
	if err != nil || got != `{"a":2}` || time.Since(at) > time.Minute || time.Until(at) > time.Minute {
		t.Fatalf("got %q at %v (%v)", got, at, err)
	}
	if err := st.PutStrengthLevelJob(ctx, u.ID+999, "{}"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}
