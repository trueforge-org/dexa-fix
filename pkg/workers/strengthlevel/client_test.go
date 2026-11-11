package strengthlevel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
)

const (
	testUID  = "019474c6-b1b1-7299-bd69-57017308050a"
	pageCSRF = "0123456789abcdef0123456789abcdef"
)

// fake mimics the parts of my.strengthlevel.com the client uses.
type fake struct {
	mu      sync.Mutex
	entries map[string]map[string]any // by date
	posted  []map[string]any
	puts    []map[string]any
	badPage bool
}

func (f *fake) handler() http.Handler {
	mux := http.NewServeMux()
	loggedIn := func(r *http.Request) bool {
		c, err := r.Cookie("strengthsession")
		return err == nil && c.Value == "ok"
	}
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if loggedIn(r) {
			http.Redirect(w, r, "/crowcontrol", http.StatusFound)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "strengthsession", Value: "anon", Path: "/"})
		w.Write([]byte(`<form method="post" action="/login"><input name="email"><input type="hidden" name="csrf_token" value="loginTok"></form>`))
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostForm.Get("csrf_token") != "loginTok" || r.PostForm.Get("email") != "a@b.c" || r.PostForm.Get("password") != "pw" {
			w.Write([]byte("<h1>Log In</h1> bad credentials")) // the site re-renders the form
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "strengthsession", Value: "ok", Path: "/"})
		http.Redirect(w, r, "/crowcontrol", http.StatusFound)
	})
	mux.HandleFunc("GET /crowcontrol", func(w http.ResponseWriter, r *http.Request) {
		if !loggedIn(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if f.badPage {
			w.Write([]byte("<html></html>"))
			return
		}
		w.Write([]byte(`<meta name="csrf-token" content="` + pageCSRF + `"><script>window.app={"user_id":"` + testUID + `"}</script>`))
	})
	mux.HandleFunc("GET /api/bodyweights", func(w http.ResponseWriter, r *http.Request) {
		if !loggedIn(r) || r.URL.Query().Get("user_id") != testUID {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		data := []map[string]any{}
		for _, e := range f.entries {
			data = append(data, e)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	mux.HandleFunc("PUT /api/bodyweights/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !loggedIn(r) || r.Header.Get("X-CSRF-Token") != pageCSRF {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		for date, e := range f.entries {
			if e["id"] == r.PathValue("id") {
				body["id"] = e["id"]
				f.entries[date] = body
				f.puts = append(f.puts, body)
				w.Write([]byte(`{"data":{}}`))
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("POST /api/bodyweights", func(w http.ResponseWriter, r *http.Request) {
		if !loggedIn(r) || r.Header.Get("X-CSRF-Token") != pageCSRF {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.posted = append(f.posted, body)
		f.entries[body["date"].(string)] = map[string]any{"id": "x", "date": body["date"], "bodyweight": body["bodyweight"]}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"data":{"id":"new"}}`))
	})
	return mux
}

func fp(v float64) *float64 { return &v }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newFake(t *testing.T) (*fake, *Client) {
	f := &fake{entries: map[string]map[string]any{
		"2026-11-06": {"id": "e1", "date": "2026-11-06", "bodyweight": 72.7, "bodyfat": 15.8, "bodyfat_method": "caliper", "timezone": "Europe/Amsterdam", "timezone_offset_mins": -60},
	}}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	c := newClient(ts.URL, nil)
	c.Delay = 0
	return f, c
}

func TestLoginAndSync(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	if err := c.Login(ctx, "a@b.c", "pw"); err != nil {
		t.Fatal(err)
	}
	local := []workerapi.ExportEntry{ // newest first, as DailyEntries returns them
		{Day: "2026-11-08", WeightKg: 72.74, FatPct: fp(15.84)},
		{Day: "2026-11-07", WeightKg: 73},
		{Day: "2026-11-06", WeightKg: 99}, // exists remotely with another value: left alone
		{Day: "2026-10-01", WeightKg: 70}, // before since
	}
	res, err := c.Sync(ctx, local, "2026-11-01", Options{})
	if err != nil || res.Added != 2 || res.Skipped != 1 || len(res.Failed) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(f.posted) != 2 || f.posted[0]["date"] != "2026-11-07" || f.posted[1]["date"] != "2026-11-08" {
		t.Fatalf("posts (oldest first): %+v", f.posted)
	}
	if p := f.posted[0]; p["bodyfat"] != nil || p["user_id"] != testUID || p["bodyweight"] != 73.0 {
		t.Fatalf("no-fat post: %+v", p)
	}
	if p := f.posted[1]; p["bodyfat"] != 15.8 || p["bodyweight"] != 72.7 || p["bodyfat_method"] != "scales" {
		t.Fatalf("fat post: %+v", p)
	}
	if res, _ = c.Sync(ctx, local, "2026-11-01", Options{}); res.Added != 0 || res.Skipped != 3 {
		t.Fatalf("second sync must be a no-op: %+v", res)
	}
	if f.entries["2026-11-06"]["bodyweight"] != 72.7 {
		t.Fatal("existing entry was changed")
	}
}

func TestSyncStopsAtOnceWhenCancelledDuringThePauseBetweenImports(t *testing.T) {
	_, c := newFake(t)
	c.Delay = time.Hour
	if err := c.Login(context.Background(), "a@b.c", "pw"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// cancel once Strength Level has answered the first write, while the sync pauses; cancelling when
	// the fake only received it could cancel the request itself
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err == nil && r.Method == http.MethodPost && r.URL.Path == "/api/bodyweights" {
			cancel()
		}
		return resp, err
	})
	local := []workerapi.ExportEntry{{Day: "2026-11-08", WeightKg: 72}, {Day: "2026-11-07", WeightKg: 73}}
	start := time.Now()
	res, err := c.Sync(ctx, local, "2026-11-01", Options{})
	if !errors.Is(err, context.Canceled) || res.Added != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("the sync waited for the pause")
	}
}

func TestLoginRefused(t *testing.T) {
	_, c := newFake(t)
	if err := c.Login(context.Background(), "a@b.c", "wrong"); !errors.Is(err, ErrLogin) {
		t.Fatalf("want ErrLogin, got %v", err)
	}
}

func TestLoginUnexpectedPage(t *testing.T) {
	f, c := newFake(t)
	f.badPage = true
	err := c.Login(context.Background(), "a@b.c", "pw")
	if !errors.Is(err, ErrPage) || !strings.Contains(err.Error(), "token") {
		t.Fatalf("want ErrPage, got %v", err)
	}
}

func TestLoginWhenAlreadySignedIn(t *testing.T) {
	f, c := newFake(t)
	u, _ := url.Parse(c.base)
	c.http.Jar.SetCookies(u, []*http.Cookie{{Name: "strengthsession", Value: "ok", Path: "/"}})
	if err := c.Login(context.Background(), "someone@else", "not-checked"); err != nil {
		t.Fatalf("a forwarded sign-in page must count as signed in: %v", err)
	}
	if days, err := c.List(context.Background()); err != nil || len(days) != len(f.entries) {
		t.Fatalf("%v %v", days, err)
	}
}

func TestSyncUpdatesExistingDays(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	if err := c.Login(ctx, "a@b.c", "pw"); err != nil {
		t.Fatal(err)
	}
	var progress []int
	local := []workerapi.ExportEntry{
		{Day: "2026-11-07", WeightKg: 73},
		{Day: "2026-11-06", WeightKg: 71.04, FatPct: fp(14.26)}, // other values than on the site
	}
	res, err := c.Sync(ctx, local, "", Options{Update: true, Progress: func(done, total int) { progress = append(progress, done, total) }})
	if err != nil || res.Added != 1 || res.Updated != 1 || res.Skipped != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(progress) != 4 || progress[1] != 2 || progress[2] != 2 {
		t.Fatalf("progress (done, total pairs): %v", progress)
	}
	if len(f.puts) != 1 {
		t.Fatalf("puts: %+v", f.puts)
	}
	p := f.puts[0]
	if p["bodyweight"] != 71.0 || p["bodyfat"] != 14.3 || p["bodyfat_method"] != "caliper" || p["timezone"] != "Europe/Amsterdam" || p["date"] != "2026-11-06" {
		t.Fatalf("update keeps the entry's method and time zone: %+v", p)
	}

	// The same values again change nothing; an entry without body fat keeps the one on the site.
	f.puts = nil
	res, _ = c.Sync(ctx, []workerapi.ExportEntry{{Day: "2026-11-06", WeightKg: 71.0, FatPct: fp(14.3)}}, "", Options{Update: true})
	if res.Updated != 0 || res.Skipped != 1 || len(f.puts) != 0 {
		t.Fatalf("unchanged day: %+v %v", res, f.puts)
	}
	res, _ = c.Sync(ctx, []workerapi.ExportEntry{{Day: "2026-11-06", WeightKg: 70}}, "", Options{Update: true})
	if res.Updated != 1 || f.puts[0]["bodyfat"] != 14.3 || f.puts[0]["bodyweight"] != 70.0 {
		t.Fatalf("body fat on the site must survive: %+v %+v", res, f.puts)
	}
}

func TestUpsertSingleDay(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	if err := c.Login(ctx, "a@b.c", "pw"); err != nil {
		t.Fatal(err)
	}
	for day, want := range map[string]Outcome{"2026-11-09": Added, "2026-11-06": Updated} {
		if got, err := c.Upsert(ctx, workerapi.ExportEntry{Day: day, WeightKg: 75}); err != nil || got != want {
			t.Fatalf("%s: %v %v", day, got, err)
		}
	}
	if got, _ := c.Upsert(ctx, workerapi.ExportEntry{Day: "2026-11-06", WeightKg: 75}); got != Unchanged || len(f.puts) != 1 {
		t.Fatalf("second upsert must not write: %v %d", got, len(f.puts))
	}
}
