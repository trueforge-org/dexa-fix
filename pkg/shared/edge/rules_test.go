package edge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// seen is what the handler behind Proxy.Headers learned about the visitor.
type seen struct {
	addr   string
	secure bool
	host   string
}

func throughProxy(t *testing.T, trusted []string, peer string, header http.Header) seen {
	t.Helper()
	prefixes, err := ParseTrustedProxies(trusted)
	if err != nil {
		t.Fatal(err)
	}
	var got seen
	var p Proxy
	h := p.Headers(func() (*url.URL, []netip.Prefix) { return nil, prefixes }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{ClientIP(r), IsSecure(r), r.Host}
	}))
	req := httptest.NewRequest("GET", "http://dexa.test/", nil)
	req.RemoteAddr = peer
	for k, v := range header {
		req.Header[k] = v
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func TestAnUntrustedPeerCannotSetForwardedHeaders(t *testing.T) {
	got := throughProxy(t, []string{"10.0.0.0/8"}, "198.51.100.7:4000", http.Header{
		"X-Forwarded-For":   {"203.0.113.9"},
		"X-Real-Ip":         {"203.0.113.9"},
		"X-Forwarded-Proto": {"https"},
		"X-Forwarded-Host":  {"evil.example"},
	})
	if got.addr != "198.51.100.7" || got.secure || got.host != "dexa.test" {
		t.Fatalf("believed an untrusted peer: %+v", got)
	}
}

func TestATrustedProxyDecidesTheVisitorFromTheRight(t *testing.T) {
	trusted := []string{"10.0.0.0/8"}
	for name, tc := range map[string]struct {
		header http.Header
		want   string
	}{
		"the visitor behind one proxy": {http.Header{"X-Forwarded-For": {"203.0.113.9"}}, "203.0.113.9"},
		"a made-up entry on the left is not looked at": {
			http.Header{"X-Forwarded-For": {"1.2.3.4, 203.0.113.9, 10.0.0.2"}}, "203.0.113.9"},
		"only trusted entries: the leftmost is the visitor": {http.Header{"X-Forwarded-For": {"10.0.0.5, 10.0.0.2"}}, "10.0.0.5"},
		"X-Real-IP when there is no X-Forwarded-For":        {http.Header{"X-Real-Ip": {"203.0.113.10"}}, "203.0.113.10"},
		"garbage keeps the proxy's own address":             {http.Header{"X-Forwarded-For": {"203.0.113.9, not-an-address"}}, "10.0.0.1"},
		"a garbage X-Real-IP keeps the proxy's address":     {http.Header{"X-Real-Ip": {"nope"}}, "10.0.0.1"},
		"no headers at all":                                 {http.Header{}, "10.0.0.1"},
	} {
		if got := throughProxy(t, trusted, "10.0.0.1:4000", tc.header); got.addr != tc.want {
			t.Errorf("%s: got %q, want %q", name, got.addr, tc.want)
		}
	}
}

func TestATrustedProxyMaySaySecureAndNameTheHost(t *testing.T) {
	got := throughProxy(t, []string{"10.0.0.0/8"}, "10.0.0.1:4000", http.Header{
		"X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"dexa.example.com"},
	})
	if !got.secure || got.host != "dexa.example.com" {
		t.Fatalf("got %+v", got)
	}
	got = throughProxy(t, []string{"10.0.0.0/8"}, "10.0.0.1:4000", http.Header{"X-Forwarded-Host": {"bad host/x"}})
	if got.host != "dexa.test" {
		t.Fatalf("an invalid forwarded host was used: %+v", got)
	}
}

func TestClientIPOfAnAddressWithoutAPort(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "198.51.100.7"
	if got := ClientIP(r); got != "198.51.100.7" {
		t.Fatalf("got %q", got)
	}
}

func TestLimiterWithoutACapAdmitsEverything(t *testing.T) {
	l := &Limiter{}
	release, ok := l.Acquire(context.Background())
	if !ok {
		t.Fatal("refused without a cap")
	}
	release()
	if !l.Allow("anyone") {
		t.Fatal("the rate limit is off, yet a request was refused")
	}
}

func TestAQueuedRequestGivesUpWhenItsContextEnds(t *testing.T) {
	l := &Limiter{MaxInFlight: 1, MaxQueue: 1} // MaxWait 0 means a second; the context ends first
	release, _ := l.Acquire(context.Background())
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := l.Acquire(ctx); ok {
		t.Fatal("admitted although the caller is gone and the slot is taken")
	}
}

func TestTheBucketTableStaysBounded(t *testing.T) {
	now := time.Unix(1000, 0)
	l := &Limiter{Rate: 1, Burst: 2, now: func() time.Time { return now }}
	l.init()

	// Full buckets carry no information: a sweep drops them and keeps the one that was used.
	l.buckets = map[string]*bucket{
		"full":    {tokens: 2, last: now},
		"refills": {tokens: 0, last: now.Add(-10 * time.Second)},
		"used":    {tokens: 0, last: now},
	}
	l.sweep(now, 2)
	if _, ok := l.buckets["used"]; len(l.buckets) != 1 || !ok {
		t.Fatalf("after the sweep: %v", l.buckets)
	}

	// If nothing can be dropped, everything goes rather than growing without bound.
	l.buckets = map[string]*bucket{}
	for i := range maxBuckets {
		l.buckets["k"+strconv.Itoa(i)] = &bucket{tokens: 0, last: now}
	}
	if !l.Allow("one more") {
		t.Fatal("a new key was refused after the table was cleared")
	}
	if len(l.buckets) != 1 {
		t.Fatalf("%d buckets left after the flood", len(l.buckets))
	}
}

func TestGzipKeepsTheFirstStatus(t *testing.T) {
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusTeapot)
		w.WriteHeader(http.StatusOK) // too late, ignored
		w.Write([]byte("hello"))
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d, encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
}
