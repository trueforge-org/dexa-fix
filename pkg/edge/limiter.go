package edge

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Limiter sheds load before it reaches the handlers behind it. It combines
//
//   - a cap on requests being served at once, with a bounded queue and a bounded wait, so a flood
//     is answered with a cheap 503 instead of piling up work; and
//   - an optional token bucket per key (normally the client address), so one client cannot take
//     the whole budget.
//
// The zero values of the optional fields turn the matching part off.
type Limiter struct {
	// MaxInFlight is the number of requests served at once. 0 means no cap.
	MaxInFlight int
	// MaxQueue is how many more requests may wait for a slot. Beyond that they are refused at once.
	MaxQueue int
	// MaxWait bounds how long a queued request waits for a slot.
	MaxWait time.Duration
	// Rate and Burst configure the per-key token bucket (requests per second, bucket size).
	// Rate 0 turns it off.
	Rate  float64
	Burst int
	// Key names the bucket a request belongs to. nil means IPKey.
	Key func(*http.Request) string
	// Skip exempts a request (cheap static files, health checks).
	Skip func(*http.Request) bool

	once  sync.Once
	slots chan struct{}

	mu      sync.Mutex
	waiting int
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

const maxBuckets = 50000

func (l *Limiter) init() {
	l.once.Do(func() {
		if l.MaxInFlight > 0 {
			l.slots = make(chan struct{}, l.MaxInFlight)
		}
		l.buckets = map[string]*bucket{}
		if l.now == nil {
			l.now = time.Now
		}
	})
}

// Acquire reserves a slot. It reports false when the request must be refused: the queue is full or
// the wait ran out. The returned release must be called when the request is done.
func (l *Limiter) Acquire(ctx context.Context) (release func(), ok bool) {
	l.init()
	if l.slots == nil {
		return func() {}, true
	}
	select {
	case l.slots <- struct{}{}:
		return l.release, true
	default:
	}
	l.mu.Lock()
	if l.waiting >= l.MaxQueue {
		l.mu.Unlock()
		return nil, false
	}
	l.waiting++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.waiting--
		l.mu.Unlock()
	}()

	wait := l.MaxWait
	if wait <= 0 {
		wait = time.Second
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case l.slots <- struct{}{}:
		return l.release, true
	case <-t.C:
	case <-ctx.Done():
	}
	return nil, false
}

func (l *Limiter) release() { <-l.slots }

// Allow takes one token from key's bucket and reports whether there was one.
func (l *Limiter) Allow(key string) bool {
	l.init()
	if l.Rate <= 0 {
		return true
	}
	burst := float64(max(l.Burst, 1))
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxBuckets { // bound memory under a flood of distinct keys
			l.sweep(now, burst)
		}
		b = &bucket{tokens: burst, last: now}
		l.buckets[key] = b
	} else {
		b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*l.Rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have refilled completely, which carry no information. If that frees
// nothing, everything goes rather than growing without bound. Callers hold the lock.
func (l *Limiter) sweep(now time.Time, burst float64) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.Rate >= burst {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= maxBuckets {
		l.buckets = map[string]*bucket{}
	}
}

// Middleware applies the limiter. Refused requests get 429 (over the per-key rate) or 503 (the
// server is at capacity), both with Retry-After, before any handler work is done.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	l.init()
	key := l.Key
	if key == nil {
		key = IPKey
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.Skip != nil && l.Skip(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !l.Allow(key(r)) {
			refuse(w, http.StatusTooManyRequests, 1)
			return
		}
		release, ok := l.Acquire(r.Context())
		if !ok {
			refuse(w, http.StatusServiceUnavailable, 2)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

func refuse(w http.ResponseWriter, status, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	http.Error(w, http.StatusText(status), status)
}
