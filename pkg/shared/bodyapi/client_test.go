package bodyapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newClientFor(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s, _ := newPair(t)
	c, err := New(srv.URL, nil, "secret", s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewRefusesWhatCannotWork(t *testing.T) {
	s, _ := newPair(t)
	for name, tc := range map[string]struct {
		base, secret string
		signer       *Signer
	}{
		"an address without a host": {"not an address", "secret", s},
		"no secret":                 {"http://body:1", "", s},
		"no signing key":            {"http://body:1", "secret", nil},
	} {
		if _, err := New(tc.base, nil, tc.secret, tc.signer); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if c, err := New("http://body:1", nil, "secret", s); err != nil || c.hc != http.DefaultClient {
		t.Errorf("a missing http client should default: %v", err)
	}
}

func TestHealthReportsWhatWentWrong(t *testing.T) {
	t.Run("a service that answers 500", func(t *testing.T) {
		c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) })
		var e *Error
		if err := c.Health(context.Background()); !errors.As(err, &e) || e.Status != 500 || e.Code != CodeUnavailable {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a service that cuts the connection", func(t *testing.T) {
		c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		})
		var e *Error
		if err := c.Health(context.Background()); !errors.As(err, &e) || e.Status != 503 {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a cancelled context", func(t *testing.T) {
		c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.Health(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no context at all", func(t *testing.T) {
		c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {})
		var ctx context.Context
		if err := c.Health(ctx); err == nil {
			t.Fatal("a request could not be built, yet no error")
		}
	})
}

func TestACallThatCannotBeBuiltIsNotSent(t *testing.T) {
	c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not be called") })
	ctx := context.Background()
	if _, err := c.call(ctx, "GET", "/x", nil, 1, ScopeRead, nil, nil, make(chan int), nil); err == nil {
		t.Error("a body that cannot be encoded was accepted")
	}
	if _, err := c.call(ctx, "bad method", "/x", nil, 1, ScopeRead, nil, nil, nil, nil); err == nil {
		t.Error("an invalid method was accepted")
	}
}

func TestACallReportsFailuresOfTheLink(t *testing.T) {
	ctx := context.Background()
	t.Run("a cancelled context is returned as it is", func(t *testing.T) {
		c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {})
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := c.DeleteUser(cctx, 1); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a cut connection is unavailable", func(t *testing.T) {
		c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		})
		if err := c.DeleteUser(ctx, 1); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("an answer cut off in the middle is unavailable", func(t *testing.T) {
		c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.Write([]byte(`{"a"`))
		})
		if _, err := c.Stats(ctx); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("garbage where JSON should be", func(t *testing.T) {
		c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{nope`)) })
		if _, err := c.Stats(ctx); err == nil || !strings.Contains(err.Error(), "body service answer") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a 500 that is not JSON counts as unavailable", func(t *testing.T) {
		c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) })
		if _, err := c.Stats(ctx); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a 500 that says internal also counts as unavailable", func(t *testing.T) {
		c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			w.Write([]byte(`{"code":"internal","message":"x"}`))
		})
		if _, err := c.Stats(ctx); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a 403 that is not JSON is an internal error", func(t *testing.T) {
		c := newClientFor(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 403) })
		var e *Error
		if _, err := c.Stats(ctx); !errors.As(err, &e) || e.Status != 403 || e.Code != CodeInternal {
			t.Fatalf("got %v", err)
		}
	})
}

func TestEveryCallCarriesTheSecretAndAFreshToken(t *testing.T) {
	s, pub := newPair(t)
	var secret, token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, token = r.Header.Get(HeaderSecret), r.Header.Get(HeaderToken)
		w.Write([]byte(`[3,4]`))
	}))
	t.Cleanup(srv.Close)
	c, _ := New(srv.URL, nil, "s3cret", s)
	now := time.Now()
	c.now = func() time.Time { return now }
	ids, err := c.UserIDs(context.Background())
	if err != nil || len(ids) != 2 || secret != "s3cret" {
		t.Fatalf("ids %v err %v secret %q", ids, err, secret)
	}
	claims, err := NewVerifier(pub).Verify(token, now)
	if err != nil || claims.Scope != ScopeAdmin {
		t.Fatalf("claims %+v err %v", claims, err)
	}
}
