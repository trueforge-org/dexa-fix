package apiclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// clientFor returns a client of a real TCP server running h.
func clientFor(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, nil, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// failingCalls is every client call that decodes an answer or reports a transport failure.
func failingCalls(c *Client) map[string]func() error {
	ctx := context.Background()
	return map[string]func() error{
		"Do":             func() error { return c.Get(ctx, "/x", &struct{}{}) },
		"FrontendConfig": func() error { _, _, err := c.FrontendConfig(ctx, ""); return err },
		"Passkey":        func() error { _, err := c.PasskeyLoginFinish(ctx, strings.NewReader("{}")); return err },
		"Reset":          func() error { _, err := c.ResetFormPage(ctx, "tok"); return err },
		"Status":         func() error { _, err := c.Status(ctx); return err },
		"AdminStatus":    func() error { _, err := c.AdminStatus(ctx); return err },
		"Session":        func() error { _, err := c.Session(ctx); return err },
	}
}

func TestEveryCallReportsAnAnswerThatIsNotJSON(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{this is not json`)) })
	for name, call := range failingCalls(c) {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil || !strings.Contains(err.Error(), "backend answer") {
				t.Fatalf("want a 'backend answer' error, got %v", err)
			}
		})
	}
}

func TestEveryCallReportsACutConnectionAsUnavailable(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	calls := failingCalls(c)
	calls["Health"] = func() error { return c.Health(context.Background()) }
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("want ErrUnavailable, got %v", err)
			}
		})
	}
}

func TestHealthRefusesAnythingButOK(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "sick", http.StatusServiceUnavailable) })
	err := c.Health(context.Background())
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("got %v", err)
	}
}

func TestAnErrorWithoutAMessageNamesTheStatus(t *testing.T) {
	err := error(&Error{Status: 404})
	if got := err.Error(); got != "backend answered 404" {
		t.Fatalf("got %q", got)
	}
	if StatusOf(errors.New("plain")) != 0 {
		t.Fatal("a plain error has no status")
	}
	if IsCode(errors.New("plain"), CodeForbidden) {
		t.Fatal("a plain error has no code")
	}
}

func TestACallThatCannotBeEncodedOrBuiltFailsBeforeSending(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the backend must not be called")
	})
	ctx := context.Background()
	if _, err := c.Do(ctx, "GET", "/x", make(chan int), nil); err == nil {
		t.Error("Do sent a value that cannot be encoded as JSON")
	}
	if _, err := c.withToken(ctx, "POST", "/x", "tok", make(chan int)); err == nil {
		t.Error("withToken sent a value that cannot be encoded as JSON")
	}
	if _, err := c.Raw(ctx, "bad method", "/x", nil, nil); err == nil {
		t.Error("Raw accepted an invalid method")
	}
}

func TestACancelledCallReturnsTheContextError(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Get(ctx, "/x", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestAnUnreachableBackendIsUnavailable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens here any more
	c, err := New("http://"+addr, nil, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), "/x", nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestSessionSeparatesSignedOutFromFailure(t *testing.T) {
	out := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"unauthorized"}`))
	})
	if u, err := out.Session(context.Background()); u != nil || err != nil {
		t.Fatalf("signed out: got %v, %v", u, err)
	}
	forbidden := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"code":"forbidden","message":"Not for you."}`))
	})
	u, err := forbidden.Session(context.Background())
	if u != nil || !IsCode(err, CodeForbidden) {
		t.Fatalf("forbidden: got %v, %v", u, err)
	}
}

func TestOverallIsDegradedWhenOneComponentIsAndNoneIsDown(t *testing.T) {
	s := Status{Components: []StatusComponent{{Name: "api", State: StateUp}, {Name: "body", State: StateDegraded}}}
	if got := s.Overall(); got != StateDegraded {
		t.Fatalf("got %q", got)
	}
}

func TestSettingsPageSendsTheFlashMessage(t *testing.T) {
	var got string
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("msg")
		w.Write([]byte(`{}`))
	})
	if _, err := c.SettingsPage(context.Background(), 7, "Saved & done"); err != nil {
		t.Fatal(err)
	}
	if got != "Saved & done" {
		t.Fatalf("the backend saw msg=%q", got)
	}
}
