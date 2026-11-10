package bodybackend

import (
	"testing"

	"github.com/trueforge-org/dexafix/pkg/estimate"
)

func TestEstimateCacheEvictsAndKeepsNewerVersions(t *testing.T) {
	c := newEstimateCache()
	calls := 0
	res := func() func() (estimate.Result, error) {
		return func() (estimate.Result, error) { calls++; return estimate.Result{}, nil }
	}
	key := func(v int64) estimateKey { return estimateKey{version: v, day: "2026-01-01"} }

	c.get(1, key(5), res())
	c.get(1, key(5), res())
	if calls != 1 {
		t.Errorf("same key computed %d times", calls)
	}
	c.get(1, key(6), res())
	c.get(1, key(5), res()) // a slow request from before the write must not replace the newer result...
	c.get(1, key(6), res()) // ...so version 6 is still cached
	if calls != 3 {
		t.Errorf("calls %d, want 3 (v5, v6, late v5)", calls)
	}
	for id := int64(10); id < 10+estimateCacheUsers+5; id++ {
		c.get(id, key(1), res())
	}
	if len(c.entries) > estimateCacheUsers {
		t.Errorf("cache holds %d users, limit %d", len(c.entries), estimateCacheUsers)
	}
}
