package mainbackend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
)

// scheduledFor makes the scripted provider ask for a job for each of the users.
func (f *scriptedFixture) scheduledFor(users ...int64) {
	f.sc.set(func(s *scripted) {
		s.sched = func(context.Context, *Backend) ([]scheduleTarget, error) {
			var out []scheduleTarget
			for _, id := range users {
				out = append(out, scheduleTarget{UserID: id})
			}
			return out, nil
		}
	})
}

func (f *scriptedFixture) jobsOf(t *testing.T) []models.Job {
	t.Helper()
	jobs, err := f.st.ListJobs(context.Background(), models.JobFilter{Kinds: []string{scriptKind}})
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestTheMaintenanceLoopSchedulesJobsAgainAndAgainUntilTheBackendCloses(t *testing.T) {
	f := newScriptedFixture(t)
	f.setSetting(t, kindSettingName(scriptKind, fieldScheduleHours), "1")
	f.scheduledFor(f.user.ID)
	old := maintainTick
	maintainTick = 10 * time.Millisecond
	t.Cleanup(func() { maintainTick = old })

	f.b.startWorkerMaintenance()
	waitFor(t, "the first scheduled job", func() bool { return len(f.jobsOf(t)) == 1 })
	f.scheduledFor(f.user.ID, f.admin.ID) // a later round picks up the new target
	waitFor(t, "the job of the second round", func() bool { return len(f.jobsOf(t)) == 2 })
	f.b.bg.Stop() // the loop must end when the backend closes
}

// waitFor polls until the condition holds or two seconds passed.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOldJobsAndWorkersArePurgedAndAFailureToDoSoIsLogged(t *testing.T) {
	ctx := context.Background()
	t.Run("a finished job older than the history is forgotten", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.setSetting(t, settingWorkersKeepDays, "1")
		j := f.queue(t, scriptKind, models.JobOriginUser)
		l := f.leaseKind(t, scriptKind)
		if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeFail, Error: "x"}); err != nil {
			t.Fatal(err)
		}
		if err := f.st.ExecForTests(ctx, "UPDATE worker_jobs SET finished_at = '2000-01-01 00:00:00'"); err != nil {
			t.Fatal(err)
		}
		logs := captureLog(t)
		f.b.purgeJobs(ctx)
		if _, err := f.st.GetJob(ctx, j.PublicID); err == nil || !strings.Contains(logs.String(), "purged old jobs") {
			t.Fatalf("job still there (%v), log %s", err, logs)
		}
	})
	t.Run("the jobs cannot be purged", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.setSetting(t, settingWorkersKeepDays, "1")
		breakTable(t, f.st, "worker_jobs")
		logs := captureLog(t)
		f.b.purgeJobs(ctx)
		if !strings.Contains(logs.String(), "purge old jobs") {
			t.Fatalf("log %s", logs)
		}
	})
	t.Run("the workers cannot be purged", func(t *testing.T) {
		f := newScriptedFixture(t)
		breakTable(t, f.st, "worker_instances")
		logs := captureLog(t)
		f.b.purgeJobs(ctx)
		if !strings.Contains(logs.String(), "purge old workers") {
			t.Fatalf("log %s", logs)
		}
	})
}

func TestTheSchedulerFailsCleanlyWhenWhatItNeedsFails(t *testing.T) {
	ctx := context.Background()
	live := kindSettings{Enabled: true, ScheduleEvery: time.Hour}
	setup := func(t *testing.T) *scriptedFixture {
		f := newScriptedFixture(t)
		f.setSetting(t, kindSettingName(scriptKind, fieldScheduleHours), "1")
		f.scheduledFor(f.user.ID)
		return f
	}
	spec := func(f *scriptedFixture) *kindSpec { return f.b.kinds[scriptKind] }

	t.Run("the list of users cannot be made", func(t *testing.T) {
		f := setup(t)
		f.sc.set(func(s *scripted) {
			s.sched = func(context.Context, *Backend) ([]scheduleTarget, error) { return nil, errors.New("no users today") }
		})
		logs := captureLog(t)
		f.b.runScheduler(ctx)
		if !strings.Contains(logs.String(), "schedule jobs") || len(f.jobsOf(t)) != 0 {
			t.Fatalf("log %s", logs)
		}
	})
	t.Run("the schedules cannot be read", func(t *testing.T) {
		f := setup(t)
		breakTable(t, f.st, "worker_schedules")
		if err := f.b.scheduleKind(ctx, spec(f), live); err == nil || len(f.jobsOf(t)) != 0 {
			t.Fatalf("err %v, jobs %d", err, len(f.jobsOf(t)))
		}
	})
	t.Run("a kind that was paused meanwhile stops the round without an error", func(t *testing.T) {
		f := setup(t)
		f.setSetting(t, kindSettingName(scriptKind, fieldEnabled), "false")
		if err := f.b.scheduleKind(ctx, spec(f), live); err != nil || len(f.jobsOf(t)) != 0 {
			t.Fatalf("err %v, jobs %d", err, len(f.jobsOf(t)))
		}
	})
	t.Run("a user who was deleted meanwhile is skipped", func(t *testing.T) {
		f := setup(t)
		f.scheduledFor(987654, f.user.ID)
		if err := f.b.scheduleKind(ctx, spec(f), live); err != nil || len(f.jobsOf(t)) != 1 {
			t.Fatalf("err %v, jobs %d", err, len(f.jobsOf(t)))
		}
	})
	t.Run("a job that cannot be queued ends the round with the error", func(t *testing.T) {
		f := setup(t)
		f.flaky.enqueue.Store(true)
		if err := f.b.scheduleKind(ctx, spec(f), live); !errors.Is(err, errFlaky) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("a schedule that cannot be saved ends the round with the error", func(t *testing.T) {
		f := setup(t)
		rejectWritesOn(t, f.st, "worker_schedules", "INSERT")
		if err := f.b.scheduleKind(ctx, spec(f), live); err == nil {
			t.Fatal("no error")
		}
	})
}

func TestTheScheduleOfAUserFollowsTheirScheduledJobs(t *testing.T) {
	ctx := context.Background()
	finish := func(f *scriptedFixture, outcome string) {
		t.Helper()
		f.queue(t, scriptKind, models.JobOriginSchedule)
		l := f.leaseKind(t, scriptKind)
		if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: outcome, Error: "bad login", Result: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a success with nothing to forgive leaves no schedule behind", func(t *testing.T) {
		f := newScriptedFixture(t)
		finish(f, models.OutcomeOK)
		if sc, err := f.st.GetJobSchedule(ctx, f.user.ID, scriptKind); err == nil {
			t.Fatalf("a schedule was written: %+v", sc)
		}
	})
	t.Run("a schedule that cannot be read is logged", func(t *testing.T) {
		f := newScriptedFixture(t)
		breakTable(t, f.st, "worker_schedules")
		logs := captureLog(t)
		finish(f, models.OutcomeFail)
		if !strings.Contains(logs.String(), "read the schedule") {
			t.Fatalf("log %s", logs)
		}
	})
	t.Run("a schedule that cannot be written is logged", func(t *testing.T) {
		f := newScriptedFixture(t)
		rejectWritesOn(t, f.st, "worker_schedules", "INSERT", "UPDATE")
		logs := captureLog(t)
		finish(f, models.OutcomeFail)
		if !strings.Contains(logs.String(), "write the schedule") {
			t.Fatalf("log %s", logs)
		}
	})
	t.Run("the reason of a paused schedule is told to the page", func(t *testing.T) {
		f := newScriptedFixture(t)
		if got := f.b.pausedReason(ctx, f.user.ID, scriptKind); got != "" {
			t.Fatalf("reason %q without a schedule", got)
		}
		finish(f, models.OutcomeFail)
		finish(f, models.OutcomeFail) // PauseAfter is 2
		if got := f.b.pausedReason(ctx, f.user.ID, scriptKind); !strings.Contains(got, "bad login") {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a schedule that cannot be forgotten is logged", func(t *testing.T) {
		f := newScriptedFixture(t)
		breakTable(t, f.st, "worker_schedules")
		logs := captureLog(t)
		f.b.resumeSchedule(ctx, f.user.ID, scriptKind)
		if !strings.Contains(logs.String(), "resume the schedule") {
			t.Fatalf("log %s", logs)
		}
	})
}

func TestTheCircuitBreakerFailsSafeWhenItsStoreFails(t *testing.T) {
	ctx := context.Background()
	failing := func(f *scriptedFixture) {
		t.Helper()
		f.queue(t, scriptKind, models.JobOriginUser)
		l := f.leaseKind(t, scriptKind)
		if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeFail, Error: "down"}); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(t *testing.T) *scriptedFixture {
		f := newScriptedFixture(t)
		f.setSetting(t, kindSettingName(scriptKind, fieldBreaker), "2")
		return f
	}
	t.Run("the recent jobs cannot be read", func(t *testing.T) {
		f := setup(t)
		f.flaky.lastFinished.Store(true)
		logs := captureLog(t)
		failing(f)
		failing(f)
		if !strings.Contains(logs.String(), "check the circuit breaker") || !f.b.cfg().workers.Kinds[scriptKind].Enabled {
			t.Fatalf("log %s", logs)
		}
	})
	t.Run("a success among the recent jobs keeps the kind running", func(t *testing.T) {
		f := setup(t)
		f.queue(t, scriptKind, models.JobOriginUser)
		l := f.leaseKind(t, scriptKind)
		f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeOK, Result: []byte(`{}`)})
		failing(f)
		if !f.b.cfg().workers.Kinds[scriptKind].Enabled {
			t.Fatal("the kind was paused")
		}
	})
	t.Run("the pause cannot be saved", func(t *testing.T) {
		f := setup(t)
		f.flaky.putFails.Store(kindSettingName(scriptKind, fieldEnabled), true)
		logs := captureLog(t)
		failing(f)
		failing(f)
		if !strings.Contains(logs.String(), "pause the kind") || !f.b.cfg().workers.Kinds[scriptKind].Enabled {
			t.Fatalf("log %s", logs)
		}
	})
	t.Run("the reason cannot be saved but the kind is paused", func(t *testing.T) {
		f := setup(t)
		f.flaky.putFails.Store(kindSettingName(scriptKind, fieldPausedReason), true)
		logs := captureLog(t)
		failing(f)
		failing(f)
		if !strings.Contains(logs.String(), "write the reason") || f.b.cfg().workers.Kinds[scriptKind].Enabled {
			t.Fatalf("log %s", logs)
		}
	})
	t.Run("the settings cannot be reloaded afterwards", func(t *testing.T) {
		f := setup(t)
		failing(f)
		f.flaky.generation.Store(true)
		logs := captureLog(t)
		failing(f)
		if !strings.Contains(logs.String(), "reload the settings") {
			t.Fatalf("log %s", logs)
		}
	})
}
