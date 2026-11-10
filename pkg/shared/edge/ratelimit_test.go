package edge

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestALimiterAnswers429WhenAClientIsOverItsRate(t *testing.T) {
	now := time.Unix(1000, 0)
	l := &Limiter{Rate: 1, Burst: 1, now: func() time.Time { return now }}
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	do := func(remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("198.51.100.7:1"); rec.Code != http.StatusOK {
		t.Fatalf("first request: %d", rec.Code)
	}
	rec := do("198.51.100.7:2")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second request: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := do("198.51.100.8:1"); rec.Code != http.StatusOK {
		t.Fatalf("another client shares the bucket: %d", rec.Code)
	}
}
