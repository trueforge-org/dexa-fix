package mainbackend

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
