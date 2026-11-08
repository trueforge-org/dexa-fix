package web

import (
	"strconv"
	"sync"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
)

// Estimates are computed from a user's whole history, which gets expensive after years of daily
// readings, and every visit to the Estimates page and every API call asks for them again. They
// only change when the data does, so the result is kept in memory.
//
// Invalidation is the part that must be right. A cached result is valid for exactly one pair:
//
//   - the user's data version, a counter in the database that every write to scans, readings and
//     the settings the estimates depend on increments in the same transaction (see
//     Store.bumpVersion). It is read together with the user on every request, so a change made by
//     any path (web form, API, import, another process on the same Postgres) is seen at once,
//     and there is no list of places to remember to invalidate from.
//   - the date, because the series runs up to today and holds the last value forward.
//
// A server upgrade starts with an empty cache, so a change in the algorithm is never masked.

// estimateCacheUsers bounds the memory used: results are kept for this many users at a time, the
// least recently used one is dropped first. Measured with BenchmarkCompute10Years' data (a daily
// reading for ten years): one result keeps about 5 MB and takes about 30 ms to compute, so the
// worst case is roughly 160 MB; typical histories are far smaller.
const estimateCacheUsers = 32

type estimateKey struct {
	version int64
	day     string
}

type estimateEntry struct {
	mu   sync.Mutex // held while computing, so concurrent requests for one user compute once
	key  estimateKey
	res  estimate.Result
	ok   bool
	used time.Time
}

type estimateCache struct {
	mu      sync.Mutex
	entries map[int64]*estimateEntry
	hits    int64
	misses  int64
}

func newEstimateCache() *estimateCache {
	return &estimateCache{entries: map[int64]*estimateEntry{}}
}

// get returns the cached result for the user if it matches key, or else computes, stores and
// returns it. Callers must treat the result as read-only: it is shared.
func (c *estimateCache) get(userID int64, key estimateKey, compute func() (estimate.Result, error)) (estimate.Result, error) {
	c.mu.Lock()
	e := c.entries[userID]
	if e == nil {
		if len(c.entries) >= estimateCacheUsers {
			c.evictOldest()
		}
		e = &estimateEntry{}
		c.entries[userID] = e
	}
	e.used = time.Now()
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ok && e.key == key {
		c.count(true)
		return e.res, nil
	}
	c.count(false)
	res, err := compute()
	if err != nil {
		return estimate.Result{}, err
	}
	// Never go back to an older version: a slow request that started before a write must not
	// replace the result of a request that saw the write.
	if !e.ok || key.version >= e.key.version {
		e.key, e.res, e.ok = key, res, true
	}
	return res, nil
}

func (c *estimateCache) count(hit bool) {
	c.mu.Lock()
	if hit {
		c.hits++
	} else {
		c.misses++
	}
	c.mu.Unlock()
}

// evictOldest drops the least recently used user. The caller holds c.mu.
func (c *estimateCache) evictOldest() {
	var oldest int64
	var at time.Time
	for id, e := range c.entries {
		if at.IsZero() || e.used.Before(at) {
			oldest, at = id, e.used
		}
	}
	delete(c.entries, oldest)
}

// estimateETag identifies the estimates API response of a user: it changes with their data, the
// day and the running version of the program, and so lets clients keep their own copy.
func (s *Server) estimateETag(userID, version int64, day string) string {
	return `W/"est-` + strconv.FormatInt(userID, 10) + "-" + strconv.FormatInt(version, 10) + "-" + day + "-" + hashToken(s.Version)[:8] + `"`
}
