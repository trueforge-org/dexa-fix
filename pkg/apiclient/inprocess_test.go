package apiclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	c, err := New("http://backend.internal", &http.Client{Transport: InProcess(h)}, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDoDecodesJSONAndRelaysCookies(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Prefix+"/x" || r.Header.Get(HeaderService) != "s3cret" ||
			r.Header.Get(HeaderCookie) != "a=b" || r.Header.Get(HeaderClientIP) != "203.0.113.9" ||
			r.Header.Get(HeaderSecure) != "1" || r.Header.Get(HeaderHost) != "dexa.example.com" {
			t.Errorf("request %s headers %v", r.URL.Path, r.Header)
		}
		http.SetCookie(w, &http.Cookie{Name: "n", Value: "v"})
		w.Write([]byte(`{"a":1}`))
	}))
	rec := httptest.NewRecorder()
	ctx := WithRequest(context.Background(), Request{Cookie: "a=b", ClientIP: "203.0.113.9", Host: "dexa.example.com", Secure: true, Relay: rec})
	var out struct{ A int }
	if err := c.Get(ctx, "/x", &out); err != nil || out.A != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if got := rec.Header().Get("Set-Cookie"); !strings.HasPrefix(got, "n=v") {
		t.Errorf("Set-Cookie not relayed: %q", got)
	}
}

func TestErrorsAndUnavailable(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case Prefix + "/bad":
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"code":"throttled","message":"Too many failed attempts."}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	err := c.Get(context.Background(), "/bad", nil)
	if !IsCode(err, CodeThrottled) || StatusOf(err) != 429 || err.Error() != "Too many failed attempts." {
		t.Errorf("bad: %v", err)
	}
	if err := c.Get(context.Background(), "/boom", nil); !isUnavailable(err) {
		t.Errorf("5xx: %v", err)
	}
}

func isUnavailable(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrUnavailable.Error())
}

func TestInProcessPanicBecomes500AndStreams(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == Prefix+"/panic" {
			panic("oops")
		}
		for range 3 {
			w.Write([]byte(strings.Repeat("x", 1000)))
		}
	}))
	if err := c.Get(context.Background(), "/panic", nil); !isUnavailable(err) {
		t.Errorf("panic: %v", err)
	}
	resp, err := c.Raw(context.Background(), "GET", "/stream", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(b) != 3000 {
		t.Errorf("streamed %d bytes", len(b))
	}
}

func TestInProcessCancel(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.Get(ctx, "/slow", nil); err == nil {
		t.Error("no error although the context expired")
	}
}

func TestNewRejectsBadURL(t *testing.T) {
	if _, err := New("nonsense", nil, "x"); err == nil {
		t.Error("accepted nonsense")
	}
}
