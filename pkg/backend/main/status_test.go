package mainbackend

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

var statusList = regexp.MustCompile(`(?s)<ul class="status-list">.*?</ul>`)

func noCache(t *testing.T) {
	t.Helper()
	old := statusKeep
	statusKeep = 0
	t.Cleanup(func() { statusKeep = old })
}

// /status is the same for everybody and shows a state per part and nothing else; the detail is a
// page of its own for administrators.
func TestStatusPageSummaryAndDetail(t *testing.T) {
	noCache(t)
	ts, alice, _ := newAuthServer(t) // the first account is the administrator
	signup(t, ts, alice, "alice")
	bob := noRedirectClient()
	signup(t, ts, bob, "bob")
	anon := noRedirectClient()

	var lists []string
	for name, c := range map[string]*http.Client{"anonymous": anon, "user": bob, "administrator": alice} {
		code, page := body(t, c, ts.URL+"/status")
		if code != http.StatusOK {
			t.Fatalf("%s: /status answered %d", name, code)
		}
		for _, want := range []string{"All systems operational", "Web", "API", "Database", "Body service", "Withings workers", "Strength Level workers"} {
			if !strings.Contains(page, want) {
				t.Errorf("%s: /status lacks %q", name, want)
			}
		}
		// The summary names no worker, version or reason: it is for visitors and monitors.
		for _, secret := range []string{"test-worker", "Version", "registered", "ping"} {
			if strings.Contains(page, secret) {
				t.Errorf("%s: /status shows %q", name, secret)
			}
		}
		list := statusList.FindString(page)
		if list == "" {
			t.Fatalf("%s: no component list in /status", name)
		}
		lists = append(lists, list)
		if admin := strings.Contains(page, `href="/admin/status"`); admin != (name == "administrator") {
			t.Errorf("%s: link to the detail shown = %v", name, admin)
		}
	}
	if lists[0] != lists[1] || lists[1] != lists[2] {
		t.Error("the component list of /status differs between visitors: a cache could not tell them apart safely")
	}

	for name, c := range map[string]*http.Client{"anonymous": anon, "user": bob} {
		code, page := body(t, c, ts.URL+"/admin/status")
		if code == http.StatusOK || strings.Contains(page, "test-worker") {
			t.Errorf("%s: /admin/status answered %d", name, code)
		}
	}
	code, page := body(t, alice, ts.URL+"/admin/status")
	if code != http.StatusOK {
		t.Fatalf("/admin/status as administrator: %d", code)
	}
	for _, want := range []string{"test-worker", "withings.import", "running"} {
		if !strings.Contains(page, want) {
			t.Errorf("/admin/status lacks %q", want)
		}
	}
}

// A database that does not answer is down, the page says 503 for monitors, and the reason stays out
// of the public page.
func TestStatusPageDatabaseDown(t *testing.T) {
	noCache(t)
	ts, _, st := newAuthServer(t)
	anon := noRedirectClient()
	if code, _ := body(t, anon, ts.URL+"/status"); code != http.StatusOK {
		t.Fatalf("/status with a working database: %d", code)
	}
	st.Close()
	code, page := body(t, anon, ts.URL+"/status")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/status with the database closed: %d", code)
	}
	if !strings.Contains(page, "Some systems are down") || !strings.Contains(page, `state-down">Down</span> <span>Database`) {
		t.Errorf("the database is not shown as down: %.600s", statusList.FindString(page))
	}
	if strings.Contains(page, "closed") {
		t.Error("the reason leaked into the public page")
	}
}

// A provider's workers are up while one asks for work, down when some registered but none asks, and
// not in use when none registered in a day. Workers of replaced containers stay on record for a day
// and must not turn a working provider into a fault.
func TestStatusWorkerStates(t *testing.T) {
	noCache(t)
	ts, _, _ := newAuthServer(t)
	b := serverOf(ts).Backend
	ctx := context.Background()
	state := func(name string) string {
		t.Helper()
		for _, c := range b.checkStatus(ctx).components {
			if c.Name == name {
				return c.State
			}
		}
		t.Fatalf("no component %q", name)
		return ""
	}
	if got := state("workers.withings"); got != "up" {
		t.Fatalf("with the test worker running: %s", got)
	}
	// The worker of a container that was replaced an hour ago is still on record.
	if err := b.store.TouchWorker(ctx, "replaced", []string{"strengthlevel.sync"}, "old", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := state("workers.strengthlevel"); got != "up" {
		t.Errorf("a stale worker next to a live one: %s", got)
	}
	// Nobody asks for work any more.
	old := workerSeen.Get()
	workerSeen.Set(time.Nanosecond)
	t.Cleanup(func() { workerSeen.Set(old) })
	if got := state("workers.withings"); got != "down" {
		t.Errorf("registered but silent: %s", got)
	}
}

// Ready is what the health listener of the api process answers.
func TestBackendReady(t *testing.T) {
	ts, _, st := newAuthServer(t)
	b := serverOf(ts).Backend
	if err := b.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := b.Ready(context.Background()); err == nil {
		t.Error("a closed database is not ready")
	}
}
