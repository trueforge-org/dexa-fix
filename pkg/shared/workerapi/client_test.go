package workerapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

func clientFor(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, nil, "worker-secret")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewRefusesABackendAddressThatIsNotAnOrigin(t *testing.T) {
	for _, base := range []string{"nonsense", "ftp://api:1", "http://"} {
		if _, err := New(base, nil, "s"); err == nil {
			t.Errorf("%q accepted", base)
		}
	}
}

func TestHealthReportsAnUnhealthyOrUnreachableBackend(t *testing.T) {
	ctx := context.Background()
	t.Run("a healthy backend", func(t *testing.T) {
		c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(HeaderWorker) != "worker-secret" || r.URL.Path != Prefix+"/healthz" {
				t.Errorf("request %s with headers %v", r.URL.Path, r.Header)
			}
		})
		if err := c.Health(ctx); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a backend that answers 500", func(t *testing.T) {
		c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) })
		if err := c.Health(ctx); !errors.Is(err, apiclient.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a backend that cuts the connection", func(t *testing.T) {
		c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		})
		if err := c.Health(ctx); !errors.Is(err, apiclient.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a cancelled context", func(t *testing.T) {
		c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {})
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := c.Health(cctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("an answer that is not JSON", func(t *testing.T) {
		c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{nope")) })
		if _, err := c.Config(ctx); err == nil || !strings.Contains(err.Error(), "backend answer") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestACallThatCannotBeBuiltIsNotSent(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not be called") })
	ctx := context.Background()
	if _, err := c.do(ctx, "GET", "/x", "", make(chan int), nil); err == nil {
		t.Error("a body that cannot be encoded was sent")
	}
	if _, err := c.do(ctx, "bad method", "/x", "", nil, nil); err == nil {
		t.Error("an invalid method was accepted")
	}
}

func TestErrorsFromTheBackendKeepTheirMeaning(t *testing.T) {
	ctx := context.Background()
	t.Run("409 is a lost lease", func(t *testing.T) {
		c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusConflict) })
		if _, err := c.Heartbeat(ctx, "j1", "tok", nil); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a 4xx with a message shows it", func(t *testing.T) {
		c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"code":"invalid","message":"Unknown kind."}`))
		})
		if err := c.Release(ctx, "j1", "tok"); !apiclient.IsCode(err, apiclient.CodeInvalid) || err.Error() != "Unknown kind." {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a 4xx that is not JSON shows its text", func(t *testing.T) {
		c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "go away", 403) })
		if err := c.Release(ctx, "j1", "tok"); apiclient.StatusOf(err) != 403 || err.Error() != "go away" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no lease available is not an error", func(t *testing.T) {
		c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
		if l, err := c.Lease(ctx, LeaseRequest{}); l != nil || err != nil {
			t.Fatalf("got %v, %v", l, err)
		}
	})
}

func TestOptionMillisFallsBackForMissingOrInvalidValues(t *testing.T) {
	k := KindConfig{Options: map[string]string{"pause": "250", "bad": "soon", "negative": "-5"}}
	def := 3 * time.Second
	for name, want := range map[string]time.Duration{
		"pause": 250 * time.Millisecond, "bad": def, "negative": def, "missing": def,
	} {
		if got := k.OptionMillis(name, def); got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
}
