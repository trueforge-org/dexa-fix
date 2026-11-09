package edge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterCapsInFlightAndQueue(t *testing.T) {
	l := &Limiter{MaxInFlight: 1, MaxQueue: 1, MaxWait: 50 * time.Millisecond}
	rel, ok := l.Acquire(context.Background())
	if !ok {
		t.Fatal("first request refused")
	}
	// The second waits in the queue and runs out of time; meanwhile a third finds the queue full.
	done := make(chan bool)
	go func() {
		_, ok := l.Acquire(context.Background())
		done <- ok
	}()
	time.Sleep(10 * time.Millisecond)
	if _, ok := l.Acquire(context.Background()); ok {
		t.Error("request admitted beyond the queue")
	}
	if <-done {
		t.Error("queued request admitted although no slot was freed")
	}
	rel()
	if rel2, ok := l.Acquire(context.Background()); !ok {
		t.Error("slot not freed")
	} else {
		rel2()
	}
}

func TestLimiterQueuedRequestRunsWhenSlotFrees(t *testing.T) {
	l := &Limiter{MaxInFlight: 1, MaxQueue: 1, MaxWait: time.Second}
	rel, _ := l.Acquire(context.Background())
	done := make(chan bool)
	go func() {
		r, ok := l.Acquire(context.Background())
		if ok {
			r()
		}
		done <- ok
	}()
	time.Sleep(10 * time.Millisecond)
	rel()
	if !<-done {
		t.Error("queued request was refused after a slot freed")
	}
}

func TestLimiterTokenBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	l := &Limiter{Rate: 2, Burst: 3, now: func() time.Time { return now }}
	for i := range 3 {
		if !l.Allow("a") {
			t.Fatalf("burst request %d refused", i)
		}
	}
	if l.Allow("a") {
		t.Error("request beyond the burst allowed")
	}
	if !l.Allow("b") {
		t.Error("another key shares the bucket")
	}
	now = now.Add(time.Second) // refills 2 tokens
	if !l.Allow("a") || !l.Allow("a") || l.Allow("a") {
		t.Error("refill is not 2 per second")
	}
}

func TestLimiterMiddlewareStatuses(t *testing.T) {
	block := make(chan struct{})
	l := &Limiter{MaxInFlight: 1, MaxQueue: 0, Rate: 100, Burst: 100,
		Skip: func(r *http.Request) bool { return r.URL.Path == "/healthz" }}
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x" {
			<-block
		}
	}))
	started := make(chan struct{})
	go func() {
		close(started)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	}()
	<-started
	time.Sleep(20 * time.Millisecond)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("at capacity: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil)) // skipped: reaches the handler
	close(block)
	if rec.Code == http.StatusServiceUnavailable {
		t.Error("skipped path was limited")
	}
}
