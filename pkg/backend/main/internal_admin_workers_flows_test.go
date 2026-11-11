package mainbackend

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

func (f *flakyStore) ListJobs(ctx context.Context, jf models.JobFilter) ([]models.Job, error) {
	if f.fails("ListJobs") {
		return nil, errFlaky
	}
	return f.Store.ListJobs(ctx, jf)
}

func (f *flakyStore) CancelJob(ctx context.Context, id string, userID int64, at time.Time) (models.Job, error) {
	if f.fails("CancelJob") {
		return models.Job{}, errFlaky
	}
	return f.Store.CancelJob(ctx, id, userID, at)
}

func (f *flakyStore) SetUserJobPriority(ctx context.Context, id int64, p int) error {
	if f.fails("SetUserJobPriority:invalid") {
		return store.ErrInvalid
	}
	return f.Store.SetUserJobPriority(ctx, id, p)
}

func TestAdminWorkersPageFailsCleanlyWhenTheStoreLetsItDown(t *testing.T) {
	scenarios := map[string]func(*testing.T, *adminEnv){
		"the jobs table is gone":      func(t *testing.T, e *adminEnv) { breakTable(t, e.st, "worker_jobs") },
		"the workers table is gone":   func(t *testing.T, e *adminEnv) { breakTable(t, e.st, "worker_instances") },
		"the schedules table is gone": func(t *testing.T, e *adminEnv) { breakTable(t, e.st, "worker_schedules") },
		"the list of jobs fails":      func(t *testing.T, e *adminEnv) { e.flaky.failOn("ListJobs") },
		"the list of users fails":     func(t *testing.T, e *adminEnv) { e.flaky.failOn("ListUsers") },
	}
	for name, breakIt := range scenarios {
		t.Run(name, func(t *testing.T) {
			e := newAdminEnv(t)
			breakIt(t, e)
			if code, out := e.do(t, "GET", "/admin/workers", nil); code != http.StatusServiceUnavailable {
				t.Fatalf("status %d %.80q, want 503", code, out)
			}
		})
	}
}

func TestAdminWorkersPageCountsThePausedSchedules(t *testing.T) {
	e := newAdminEnv(t)
	err := e.st.PutJobSchedule(context.Background(), models.JobSchedule{UserID: 2, Kind: kindWithingsImport, PausedReason: "failed three times"})
	if err != nil {
		t.Fatal(err)
	}
	code, page := e.do(t, "GET", "/admin/workers", nil)
	if code != http.StatusOK || !strings.Contains(page, "/admin/workers/schedules/") && !strings.Contains(page, "paused") && !strings.Contains(page, "Paused") {
		t.Fatalf("the page does not show the paused schedule: %d", code)
	}
}

func TestAdminWorkerSettingsTellTheAdministratorWhatHappenedOrFailCleanly(t *testing.T) {
	kindForm := url.Values{"enabled": {"1"}, "max_running": {"2"}, "max_attempts": {"4"}, "retry_base_seconds": {"30"}, "timeout_seconds": {"120"},
		"schedule_hours": {"6"}, "pause_after": {"3"}, "breaker": {"8"}}
	kind := "/admin/workers/kinds/" + kindStrengthLevelSync
	paused := func(t *testing.T, e *adminEnv) {
		t.Helper()
		if err := e.st.PutSetting(context.Background(), kindSettingName(kindStrengthLevelSync, fieldEnabled), "false"); err != nil {
			t.Fatal(err)
		}
		if err := e.backend.reloadSettings(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	runAdminChanges(t, []adminChange{
		{name: "a kind that does not exist", path: "/admin/workers/kinds/nope.kind", form: kindForm, wantStatus: 404},
		{name: "a kind setting that is not a number", path: kind, form: formWith(kindForm, "max_running", "lots"), wantFlash: "whole number"},
		{name: "an option that is not a number", path: kind, form: formWith(kindForm, "opt_request_delay_ms", "slow"), wantFlash: "whole number"},
		{name: "a kind setting that cannot be stored", setup: failing("PutSetting:" + kindSettingName(kindStrengthLevelSync, fieldMaxAttempts)), path: kind, form: kindForm},
		{name: "an option that cannot be stored", setup: failing("PutSetting:" + optionSettingName(kindStrengthLevelSync, optionRequestDelay)), path: kind, form: formWith(kindForm, "opt_"+optionRequestDelay, "50")},
		{name: "pausing a kind that cannot be paused", setup: failing("PutSetting:" + kindSettingName(kindStrengthLevelSync, fieldEnabled)), path: kind, form: formWith(kindForm, "enabled", "0")},
		{name: "switching a paused kind on when the setting cannot be removed", setup: func(t *testing.T, e *adminEnv) {
			paused(t, e)
			e.flaky.failOn("DeleteSetting:" + kindSettingName(kindStrengthLevelSync, fieldEnabled))
		}, path: kind, form: kindForm},
		{name: "switching a paused kind on when the reason cannot be removed", setup: func(t *testing.T, e *adminEnv) {
			paused(t, e)
			e.flaky.failOn("DeleteSetting:" + kindSettingName(kindStrengthLevelSync, fieldPausedReason))
		}, path: kind, form: kindForm},
		{name: "switching a paused kind on when the breaker cannot be reset", setup: func(t *testing.T, e *adminEnv) {
			paused(t, e)
			e.flaky.failOn("PutSetting:" + kindSettingName(kindStrengthLevelSync, fieldBreakerSince))
		}, path: kind, form: kindForm},
		{name: "switching a paused kind on", setup: paused, path: kind, form: kindForm, wantFlash: "settings saved"},
		{name: "kind settings that cannot be applied", setup: failing("SettingsGeneration"), path: kind, form: kindForm},
		{name: "resuming a kind that does not exist", path: "/admin/workers/kinds/nope.kind/resume", form: url.Values{}, wantStatus: 404},
		{name: "resuming a kind when the setting cannot be removed", setup: failing("DeleteSetting:" + kindSettingName(kindStrengthLevelSync, fieldEnabled)), path: kind + "/resume", form: url.Values{}},
		{name: "resuming a kind when the reason cannot be removed", setup: failing("DeleteSetting:" + kindSettingName(kindStrengthLevelSync, fieldPausedReason)), path: kind + "/resume", form: url.Values{}},
		{name: "resuming a kind when the breaker cannot be reset", setup: failing("PutSetting:" + kindSettingName(kindStrengthLevelSync, fieldBreakerSince)), path: kind + "/resume", form: url.Values{}},
		{name: "resuming a kind that cannot be applied", setup: failing("SettingsGeneration"), path: kind + "/resume", form: url.Values{}},
		{name: "resuming a kind", setup: paused, path: kind + "/resume", form: url.Values{}, wantFlash: "is on again"},
	})
}

func TestAdminSavesTheGlobalWorkerSettings(t *testing.T) {
	good := url.Values{"default_priority": {"60"}, "keep_history_days": {"14"}}
	runAdminChanges(t, []adminChange{
		{name: "saving them", path: "/admin/workers/global", form: good, wantFlash: "Worker settings saved."},
		{name: "a priority that is not a number", path: "/admin/workers/global", form: formWith(good, "default_priority", "high"), wantFlash: "whole number"},
		{name: "days that are not a number", path: "/admin/workers/global", form: formWith(good, "keep_history_days", "long"), wantFlash: "whole number"},
		{name: "a priority out of range", path: "/admin/workers/global", form: formWith(good, "default_priority", "500"), wantFlash: "default priority must be a number from 1 to 100"},
		{name: "days out of range", path: "/admin/workers/global", form: formWith(good, "keep_history_days", "0"), wantFlash: "Days to keep finished jobs"},
		{name: "a priority that cannot be stored", setup: failing("PutSetting:" + settingWorkersDefaultPriority), path: "/admin/workers/global", form: good},
		{name: "days that cannot be stored", setup: failing("PutSetting:" + settingWorkersKeepDays), path: "/admin/workers/global", form: good},
		{name: "settings that cannot be applied", setup: failing("SettingsGeneration"), path: "/admin/workers/global", form: good},
	})
	t.Run("a saved priority is what a new account gets", func(t *testing.T) {
		e := newAdminEnv(t)
		if code, out := e.do(t, "POST", "/admin/workers/global", good); code != http.StatusSeeOther {
			t.Fatalf("saving: %d %q", code, out)
		}
		if got := e.backend.cfg().workers.DefaultPriority; got != 60 {
			t.Fatalf("default priority in force: %d, want 60", got)
		}
	})
}

func TestAdminCancelsAJobFromTheWorkersPage(t *testing.T) {
	queue := func(t *testing.T, e *adminEnv, lease bool) string {
		t.Helper()
		ctx := context.Background()
		job, _, err := e.st.EnqueueJob(ctx, models.NewJob{UserID: 2, Kind: "demo.nothing", Origin: models.JobOriginUser, DedupeKey: "demo", MaxAttempts: 1, Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if lease {
			if _, ok, err := e.st.LeaseJob(ctx, models.LeaseRequest{Kinds: []string{"demo.nothing"}, Owner: "t", TokenHash: "h", LeaseFor: time.Minute}, time.Now()); err != nil || !ok {
				t.Fatalf("lease: %v %v", ok, err)
			}
		}
		return job.PublicID
	}
	t.Run("a queued job is canceled", func(t *testing.T) {
		e := newAdminEnv(t)
		id := queue(t, e, false)
		code, out := e.do(t, "POST", "/admin/workers/jobs/"+id+"/cancel", url.Values{})
		if got := adminFlash(t, out); code != 303 || got != "The job was canceled." {
			t.Fatalf("%d %q", code, got)
		}
		if j, _ := e.st.GetJob(context.Background(), id); j.State != models.JobCanceled {
			t.Fatalf("job state %q", j.State)
		}
	})
	t.Run("a running job is asked to stop", func(t *testing.T) {
		e := newAdminEnv(t)
		id := queue(t, e, true)
		code, out := e.do(t, "POST", "/admin/workers/jobs/"+id+"/cancel", url.Values{})
		if got := adminFlash(t, out); code != 303 || got != "The worker was asked to stop." {
			t.Fatalf("%d %q", code, got)
		}
	})
	t.Run("a job that does not exist", func(t *testing.T) {
		e := newAdminEnv(t)
		if code, _ := e.do(t, "POST", "/admin/workers/jobs/nope/cancel", url.Values{}); code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", code)
		}
	})
	t.Run("a database that cannot cancel is an outage", func(t *testing.T) {
		e := newAdminEnv(t)
		e.flaky.failOn("CancelJob")
		if code, _ := e.do(t, "POST", "/admin/workers/jobs/x/cancel", url.Values{}); code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", code)
		}
	})
}

func TestAdminResumesAnAutomaticRun(t *testing.T) {
	path := "/admin/workers/schedules/2/" + kindWithingsImport + "/resume"
	t.Run("the paused schedule is forgotten", func(t *testing.T) {
		e := newAdminEnv(t)
		if err := e.st.PutJobSchedule(context.Background(), models.JobSchedule{UserID: 2, Kind: kindWithingsImport, PausedReason: "failed"}); err != nil {
			t.Fatal(err)
		}
		code, out := e.do(t, "POST", path, url.Values{})
		if got := adminFlash(t, out); code != 303 || got != "Automatic runs will start again." {
			t.Fatalf("%d %q", code, got)
		}
		if _, err := e.st.GetJobSchedule(context.Background(), 2, kindWithingsImport); err == nil {
			t.Fatal("the paused schedule is still there")
		}
	})
	t.Run("a kind that does not exist", func(t *testing.T) {
		e := newAdminEnv(t)
		if code, _ := e.do(t, "POST", "/admin/workers/schedules/2/nope.kind/resume", url.Values{}); code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", code)
		}
	})
	t.Run("a user id that is not a number", func(t *testing.T) {
		e := newAdminEnv(t)
		if code, _ := e.do(t, "POST", "/admin/workers/schedules/abc/"+kindWithingsImport+"/resume", url.Values{}); code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", code)
		}
	})
	t.Run("a user that does not exist", func(t *testing.T) {
		e := newAdminEnv(t)
		if code, _ := e.do(t, "POST", "/admin/workers/schedules/99/"+kindWithingsImport+"/resume", url.Values{}); code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", code)
		}
	})
}

func TestAdminSetsAUsersJobPriority(t *testing.T) {
	runAdminChanges(t, []adminChange{
		{name: "a priority the database refuses as invalid", setup: failing("SetUserJobPriority:invalid"), path: "/admin/users/2/priority", form: url.Values{"priority": {"50"}}, wantFlash: "invalid"},
		{name: "a priority that cannot be stored", setup: func(t *testing.T, e *adminEnv) { refuseWrites(t, e.st, "users", "id", "2") }, path: "/admin/users/2/priority", form: url.Values{"priority": {"50"}}},
		{name: "a user id that is not a number", path: "/admin/users/abc/priority", form: url.Values{"priority": {"50"}}, wantStatus: 404},
	})
}

func TestAdminWorkerEndpointsRefuseGarbage(t *testing.T) {
	bs, _, root := newInternalServer(t)
	for _, route := range []string{"POST /admin/workers/kinds/withings.import", "POST /admin/workers/global", "POST /admin/users/1/priority"} {
		method, path, _ := strings.Cut(route, " ")
		if got := internalRaw(t, bs.URL, root.Value, method, path, "{not json"); got != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", route, got)
		}
	}
	if got := internalRaw(t, bs.URL, root.Value, "POST", "/admin/users/abc/priority", "{}"); got != http.StatusNotFound {
		t.Errorf("a user id that is not a number: status %d, want 404", got)
	}
	if got := internalRaw(t, bs.URL, root.Value, "POST", "/admin/workers/schedules/abc/withings.import/resume", "{}"); got != http.StatusNotFound {
		t.Errorf("a schedule of user \"abc\": status %d, want 404", got)
	}
}
