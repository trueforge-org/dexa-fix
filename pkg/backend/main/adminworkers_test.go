package mainbackend

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// The workers section of the Admin page, through the pages as a browser uses them.
func TestAdminWorkersPage(t *testing.T) {
	ts, c := newTestServer(t)
	signup(t, ts, c, "alice") // the first account is the administrator
	bob := &http.Client{CheckRedirect: c.CheckRedirect}
	signup(t, ts, bob, "bob")

	// Everyone but an administrator gets a 404: the page does not advertise itself.
	if code, _ := body(t, bob, ts.URL+"/admin/workers"); code != http.StatusNotFound {
		t.Fatalf("workers page as a user: %d", code)
	}
	if r := post(t, bob, ts.URL+"/admin/workers/global", url.Values{"default_priority": {"1"}, "keep_history_days": {"1"}}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("workers settings as a user: %d", r.StatusCode)
	}
	if r := post(t, bob, ts.URL+"/admin/users/2/priority", url.Values{"priority": {"100"}}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("priority as a user: %d", r.StatusCode)
	}

	code, page := body(t, c, ts.URL+"/admin/workers")
	if code != http.StatusOK {
		t.Fatalf("workers page: %d", code)
	}
	for _, want := range []string{"Running workers", "test-worker", "withings.import", "strengthlevel.sync", "Pause between two writes to Strength Level", "No jobs yet.", `action="/admin/workers/global"`} {
		if !strings.Contains(page, want) {
			t.Errorf("workers page lacks %q", want)
		}
	}

	flash := func(r *http.Response) string {
		t.Helper()
		if r.StatusCode != http.StatusSeeOther {
			t.Fatalf("status %d", r.StatusCode)
		}
		loc, err := url.Parse(r.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		return loc.Path + "?" + loc.Query().Get("msg")
	}
	form := func(over map[string]string) url.Values {
		v := url.Values{"enabled": {"1"}, "max_running": {"2"}, "max_attempts": {"4"}, "retry_base_seconds": {"30"}, "timeout_seconds": {"120"},
			"schedule_hours": {"6"}, "pause_after": {"3"}, "breaker": {"8"}}
		for k, x := range over {
			v.Set(k, x)
		}
		return v
	}
	if got := flash(post(t, c, ts.URL+"/admin/workers/kinds/withings.import", form(nil))); got != "/admin/workers?Withings import settings saved." {
		t.Fatalf("save: %q", got)
	}
	ks := serverOf(ts).cfg().workers.Kinds[kindWithingsImport]
	if ks.MaxRunning != 2 || ks.MaxAttempts != 4 || ks.ScheduleEvery.Hours() != 6 || ks.Breaker != 8 {
		t.Fatalf("settings in force: %+v", ks)
	}
	if _, page := body(t, c, ts.URL+"/admin/workers"); !strings.Contains(page, `name="max_attempts" value="4"`) {
		t.Error("the page does not show the saved value")
	}
	// A value out of range is refused with the reason, and nothing changes.
	if got := flash(post(t, c, ts.URL+"/admin/workers/kinds/withings.import", form(map[string]string{"max_attempts": "99"}))); !strings.Contains(got, "must be a number from 1 to 10") {
		t.Fatalf("out of range: %q", got)
	}
	if got := flash(post(t, c, ts.URL+"/admin/workers/kinds/withings.import", form(map[string]string{"breaker": "many"}))); !strings.Contains(got, "whole number") {
		t.Fatalf("not a number: %q", got)
	}
	if serverOf(ts).cfg().workers.Kinds[kindWithingsImport].MaxAttempts != 4 {
		t.Fatal("a refused save changed the settings")
	}
	// An option of the kind goes through under its own name.
	if got := flash(post(t, c, ts.URL+"/admin/workers/kinds/strengthlevel.sync", form(map[string]string{"opt_request_delay_ms": "50"}))); !strings.Contains(got, "saved") {
		t.Fatalf("option: %q", got)
	}
	if serverOf(ts).cfg().workers.Kinds[kindStrengthLevelSync].Options[optionRequestDelay] != 50 {
		t.Fatal("the option was not applied")
	}
	// Pausing a kind and turning it on again.
	if got := flash(post(t, c, ts.URL+"/admin/workers/kinds/withings.import", form(map[string]string{"enabled": "0"}))); !strings.Contains(got, "saved") {
		t.Fatal(got)
	}
	if serverOf(ts).cfg().workers.Kinds[kindWithingsImport].Enabled {
		t.Fatal("the kind was not paused")
	}
	if _, page := body(t, c, ts.URL+"/admin/workers"); !strings.Contains(page, "Paused.") || !strings.Contains(page, "/admin/workers/kinds/withings.import/resume") {
		t.Error("a paused kind has no way to turn it on again")
	}
	flash(post(t, c, ts.URL+"/admin/workers/kinds/withings.import/resume", nil))
	if !serverOf(ts).cfg().workers.Kinds[kindWithingsImport].Enabled {
		t.Fatal("the kind was not turned on")
	}
	if r := post(t, c, ts.URL+"/admin/workers/kinds/nope.kind", form(nil)); r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown kind: %d", r.StatusCode)
	}

	// Priorities, on the users table of the main Admin page.
	if got := flash(post(t, c, ts.URL+"/admin/users/2/priority", url.Values{"priority": {"77"}})); !strings.HasPrefix(got, "/admin?") || !strings.Contains(got, "77") {
		t.Fatalf("priority: %q", got)
	}
	if u, _ := serverOf(ts).store.GetUser(context.Background(), 2); u.JobPriority != 77 {
		t.Fatalf("stored priority %d", u.JobPriority)
	}
	if _, page := body(t, c, ts.URL+"/admin"); !strings.Contains(page, `name="priority" value="77"`) || !strings.Contains(page, `/admin/workers`) {
		t.Error("the Admin page shows no priority field or no link to the workers")
	}
	if got := flash(post(t, c, ts.URL+"/admin/users/2/priority", url.Values{"priority": {"101"}})); !strings.Contains(got, "from 1 to 100") {
		t.Fatalf("priority out of range: %q", got)
	}
	if got := flash(post(t, c, ts.URL+"/admin/users/2/priority", url.Values{"priority": {"high"}})); !strings.Contains(got, "whole number") {
		t.Fatalf("priority not a number: %q", got)
	}

	// A queued job shows up, and can be canceled from the page.
	job, _, err := serverOf(ts).enqueueJob(context.Background(), enqueueSpec{UserID: 2, Kind: kindWithingsImport, Origin: models.JobOriginUser, Dedupe: "page-test", Expires: 0})
	if err != nil {
		t.Fatal(err)
	}
	// (the test worker may take it at once: it only has no connection, so it ends the job as failed)
	_, page = body(t, c, ts.URL+"/admin/workers")
	if !strings.Contains(page, "bob") || !strings.Contains(page, job.Kind) {
		t.Errorf("the job is not listed:\n%s", page)
	}
}
