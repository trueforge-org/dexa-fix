package strengthlevel

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
)

// serve starts a fake Strength Level made of one handler and returns a client for it.
func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c := newClient(ts.URL, nil)
	c.Delay = 0
	return c
}

// dropConnection ends the connection without an answer.
func dropConnection() { panic(http.ErrAbortHandler) }

// cutShort promises a long body, sends a little and ends the connection.
func cutShort(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "500")
	w.Write([]byte("abc"))
	w.(http.Flusher).Flush()
	panic(http.ErrAbortHandler)
}

const tokenPage = `<form><input type="hidden" name="csrf_token" value="loginTok"></form>`

const signedInPage = `<meta name="csrf-token" content="` + pageCSRF + `"><script>window.app={"user_id":"` + testUID + `"}</script>`

func TestNewClientUsesTheRealSiteAndKeepsTheCallersSettings(t *testing.T) {
	if got := newClient("", nil).base; got != DefaultBase {
		t.Fatalf("default base = %q", got)
	}
	own := &http.Client{Timeout: 5 * time.Second}
	own.Jar, _ = cookiejar.New(nil)
	c := newClient("http://example.test/", own)
	if c.http.Timeout != 5*time.Second || c.base != "http://example.test" {
		t.Fatalf("timeout %v base %q", c.http.Timeout, c.base)
	}
	if c.http.Jar == own.Jar {
		t.Fatal("every client must have a session of its own")
	}
}

func TestLoginFailuresSayWhatWentWrong(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		check   func(error) bool
	}{
		{"the sign-in page is not reachable", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "down", http.StatusInternalServerError)
		}, func(err error) bool { return strings.Contains(err.Error(), "500") }},
		{"the sign-in page is cut short", func(w http.ResponseWriter, r *http.Request) { cutShort(w) },
			func(err error) bool { return !errors.Is(err, ErrLogin) && !errors.Is(err, ErrPage) }},
		{"the sign-in page carries no token", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html></html>")) },
			func(err error) bool { return errors.Is(err, ErrPage) && strings.Contains(err.Error(), "sign-in page") }},
		{"the connection drops while signing in", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				dropConnection()
			}
			w.Write([]byte(tokenPage))
		}, func(err error) bool { return !errors.Is(err, ErrLogin) && !errors.Is(err, ErrPage) }},
		{"the answer to the sign-in is cut short", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				cutShort(w)
			}
			w.Write([]byte(tokenPage))
		}, func(err error) bool { return !errors.Is(err, ErrLogin) && !errors.Is(err, ErrPage) }},
		{"a signed-in visitor is forwarded to a page without a user id", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/login" {
				http.Redirect(w, r, "/crowcontrol", http.StatusFound)
				return
			}
			w.Write([]byte(`<meta name="csrf-token" content="` + pageCSRF + `">`))
		}, func(err error) bool { return errors.Is(err, ErrPage) && strings.Contains(err.Error(), "user id") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := serve(t, tc.handler).Login(context.Background(), "a@b.c", "pw")
			if err == nil || !tc.check(err) {
				t.Fatalf("got %v", err)
			}
		})
	}

	t.Run("the site address is malformed", func(t *testing.T) {
		if err := newClient("http://a b", nil).Login(context.Background(), "a@b.c", "pw"); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestLoginAcceptsTheTokenInAMetaTag(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login" && r.Method == http.MethodGet:
			w.Write([]byte(`<meta name="csrf-token" content="metaTok">`))
		case r.URL.Path == "/login":
			http.Redirect(w, r, "/crowcontrol", http.StatusFound)
		default:
			w.Write([]byte(signedInPage))
		}
	})
	if err := c.Login(context.Background(), "a@b.c", "pw"); err != nil || c.userID != testUID || c.csrf != pageCSRF {
		t.Fatalf("err %v user %q csrf %q", err, c.userID, c.csrf)
	}
}

func TestListFailuresAreReported(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve http.HandlerFunc
		check func(error) bool
	}{
		{"the site refuses", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) },
			func(err error) bool {
				return strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "user_id")
			}},
		{"the answer is not JSON", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) },
			func(err error) bool { return errors.Is(err, ErrPage) }},
		{"the answer is cut short", func(w http.ResponseWriter, r *http.Request) { cutShort(w) },
			func(err error) bool { return !errors.Is(err, ErrPage) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, tc.serve)
			c.userID = testUID
			ctx := context.Background()
			if _, err := c.List(ctx); err == nil || !tc.check(err) {
				t.Fatalf("List: %v", err)
			}
			if _, err := c.Upsert(ctx, workerapi.ExportEntry{Day: "2026-11-07", WeightKg: 73}); err == nil {
				t.Fatal("Upsert must fail when the existing days cannot be read")
			}
			if res, err := c.Sync(ctx, []workerapi.ExportEntry{{Day: "2026-11-07", WeightKg: 73}}, "", Options{}); err == nil || res.Added != 0 {
				t.Fatalf("Sync: %+v %v", res, err)
			}
		})
	}
}

// writesFail answers the list with no days and fails every write the given way.
func writesFail(fail func(w http.ResponseWriter)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"data":[{"id":"e1","date":"2026-11-06","bodyweight":70}]}`))
			return
		}
		fail(w)
	}
}

func TestWriteFailuresAreReportedPerDay(t *testing.T) {
	entry := workerapi.ExportEntry{Day: "2026-11-07", WeightKg: 73}
	old := Day{ID: "e1", Date: "2026-11-06", Weight: 70}
	for _, tc := range []struct {
		name string
		fail func(w http.ResponseWriter)
		want string // part of the message; "" for any error
	}{
		{"the site refuses the write", func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) }, "POST 2026-11-07: 500 Internal Server Error boom"},
		{"the connection drops", func(w http.ResponseWriter) { dropConnection() }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, writesFail(tc.fail))
			c.userID, c.csrf = testUID, pageCSRF
			ctx := context.Background()
			err := c.Add(ctx, entry)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Add: %v", err)
			}
			if err := c.Update(ctx, old, entry); err == nil {
				t.Fatal("Update must fail too")
			}
			res, err := c.Sync(ctx, []workerapi.ExportEntry{entry}, "", Options{})
			if err != nil || res.Added != 0 || len(res.Failed) != 1 || !strings.Contains(res.Failed[0], tc.want) {
				t.Fatalf("a failed day is listed, not fatal: %+v %v", res, err)
			}
		})
	}
}
