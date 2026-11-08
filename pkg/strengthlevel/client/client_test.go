package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/strengthlevel"
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
	badPage bool
}

func (f *fake) handler() http.Handler {
	mux := http.NewServeMux()
	loggedIn := func(r *http.Request) bool {
		c, err := r.Cookie("strengthsession")
		return err == nil && c.Value == "ok"
	}
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("GET /crowcontrol", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("home")) })
	mux.HandleFunc("GET /{user}/bodyweight", func(w http.ResponseWriter, r *http.Request) {
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

func newFake(t *testing.T) (*fake, *Client) {
	f := &fake{entries: map[string]map[string]any{
		"2026-11-06": {"id": "e1", "date": "2026-11-06", "bodyweight": 72.7, "bodyfat": 15.8},
	}}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	c := New(ts.URL, nil)
	c.Delay = 0
	return f, c
}

func TestLoginAndSync(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	if err := c.Login(ctx, "crowcontrol", "a@b.c", "pw"); err != nil {
		t.Fatal(err)
	}
	local := []strengthlevel.Entry{ // newest first, as DailyEntries returns them
		{Day: "2026-11-08", WeightKg: 72.74, FatPct: fp(15.84)},
		{Day: "2026-11-07", WeightKg: 73},
		{Day: "2026-11-06", WeightKg: 99}, // exists remotely with another value: left alone
		{Day: "2026-10-01", WeightKg: 70}, // before since
	}
	res, err := c.Sync(ctx, local, "2026-11-01")
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
	if res, _ = c.Sync(ctx, local, "2026-11-01"); res.Added != 0 || res.Skipped != 3 {
		t.Fatalf("second sync must be a no-op: %+v", res)
	}
	if f.entries["2026-11-06"]["bodyweight"] != 72.7 {
		t.Fatal("existing entry was changed")
	}
}

func TestLoginRefused(t *testing.T) {
	_, c := newFake(t)
	if err := c.Login(context.Background(), "crowcontrol", "a@b.c", "wrong"); !errors.Is(err, ErrLogin) {
		t.Fatalf("want ErrLogin, got %v", err)
	}
}

func TestLoginUnexpectedPage(t *testing.T) {
	f, c := newFake(t)
	f.badPage = true
	err := c.Login(context.Background(), "crowcontrol", "a@b.c", "pw")
	if !errors.Is(err, ErrPage) || !strings.Contains(err.Error(), "token") {
		t.Fatalf("want ErrPage, got %v", err)
	}
}
