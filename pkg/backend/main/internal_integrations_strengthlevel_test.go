package mainbackend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

type flashResult struct {
	flash apiclient.Flash
	err   error
}

// inTheBackground runs a request that waits for a job and returns its answer later.
func inTheBackground(f func() (apiclient.Flash, error)) <-chan flashResult {
	out := make(chan flashResult, 1)
	go func() {
		fl, err := f()
		out <- flashResult{fl, err}
	}()
	return out
}

// waitForActiveJob waits until a job of the kind is queued or running.
func (r *jobRig) waitForActiveJob(kind string) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		jobs, err := r.st.ListJobs(context.Background(), models.JobFilter{UserID: 2, Kinds: []string{kind}, Limit: 5})
		if err != nil {
			r.t.Fatal(err)
		}
		for _, j := range jobs {
			if models.JobActive(j.State) {
				return
			}
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("no active %s job", kind)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wantFlash(t *testing.T, f apiclient.Flash, err error, want string) {
	t.Helper()
	if err != nil || !strings.Contains(f.Message, want) {
		t.Fatalf("message %q, error %v, want a message with %q", f.Message, err, want)
	}
}

func wantUnavailable(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, apiclient.ErrUnavailable) {
		t.Fatalf("error %v, want the backend to be reported unavailable", err)
	}
}

func TestSavingAStrengthLevelLoginTellsThePersonHowTheCheckEnded(t *testing.T) {
	login := func(r *jobRig) (apiclient.Flash, error) {
		return r.api.SaveStrengthLevelLogin(r.alice, 2, "a@b.c", "pw")
	}
	t.Run("an address that is not one is refused", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		f, err := r.api.SaveStrengthLevelLogin(r.alice, 2, "nope", "pw")
		wantFlash(t, f, err, "Enter the e-mail address")
	})
	t.Run("too many wrong logins are refused for a while", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		for range maxStrengthLevelLoginFail {
			r.b.limiter.Fail(context.Background(), "sl:2")
		}
		f, err := login(r)
		wantFlash(t, f, err, "Too many failed Strength Level sign-ins")
	})
	t.Run("without a worker the person is told so", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		f, err := login(r)
		wantFlash(t, f, err, "No Strength Level worker is running")
	})
	t.Run("a paused kind is reported", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		r.pauseKind(kindStrengthLevelCheckLogin)
		f, err := login(r)
		wantFlash(t, f, err, "paused by an administrator")
	})
	t.Run("a database that cannot queue the check is an outage", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		r.workerFor(stillBusy(), kindStrengthLevelCheckLogin)
		r.flaky.failOn("EnqueueJob")
		_, err := login(r)
		wantUnavailable(t, err)
	})
	t.Run("a database that cannot be asked about the job is an outage", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		r.workerFor(stillBusy(), kindStrengthLevelCheckLogin)
		r.flaky.failOn("GetJob")
		_, err := login(r)
		wantUnavailable(t, err)
	})
	t.Run("a second check while one runs is not mixed up with it", func(t *testing.T) {
		impatient(t)
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		r.workerFor(stillBusy(), kindStrengthLevelCheckLogin)
		first := inTheBackground(func() (apiclient.Flash, error) { return login(r) })
		r.waitForActiveJob(kindStrengthLevelCheckLogin)
		f, err := login(r)
		wantFlash(t, f, err, "already running")
		got := <-first
		wantFlash(t, got.flash, got.err, "being checked in the background")
	})
	t.Run("a check that is canceled says so", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		r.workerIdle(kindStrengthLevelCheckLogin)
		first := inTheBackground(func() (apiclient.Flash, error) { return login(r) })
		r.waitForActiveJob(kindStrengthLevelCheckLogin)
		if err := r.st.CancelJobs(context.Background(), 2, []string{kindStrengthLevelCheckLogin}, time.Now()); err != nil {
			t.Fatal(err)
		}
		got := <-first
		wantFlash(t, got.flash, got.err, "was canceled")
	})
	t.Run("a worker that fails the check gives its reason", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		r.workerFor(failsWith("Strength Level is down"), kindStrengthLevelCheckLogin)
		f, err := login(r)
		wantFlash(t, f, err, "Could not sign in to Strength Level: Strength Level is down")
	})
	t.Run("a wrong password is reported and not stored", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		r.workerFor(succeeds(`{"login_ok":false,"message":"wrong password"}`), kindStrengthLevelCheckLogin)
		f, err := login(r)
		wantFlash(t, f, err, "Could not sign in to Strength Level: wrong password")
		if _, err := r.st.GetIntegration(context.Background(), 2, strengthLevelProvider); err == nil {
			t.Fatal("a login that Strength Level refused was stored")
		}
	})
	t.Run("a good login is saved", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		r.workerFor(succeeds(`{"login_ok":true}`), kindStrengthLevelCheckLogin)
		f, err := login(r)
		wantFlash(t, f, err, "Strength Level login saved.")
		if _, err := r.st.GetIntegration(context.Background(), 2, strengthLevelProvider); err != nil {
			t.Fatalf("the login was not stored: %v", err)
		}
	})
}

func TestRemovingAStrengthLevelLoginStopsItsJobsAndForgetsIt(t *testing.T) {
	t.Run("the stored login is removed", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		r.saveStrengthLevelLogin()
		f, err := r.api.DeleteStrengthLevelLogin(r.alice, 2)
		wantFlash(t, f, err, "Strength Level login removed.")
		if _, err := r.st.GetIntegration(context.Background(), 2, strengthLevelProvider); err == nil {
			t.Fatal("the login is still stored")
		}
	})
	t.Run("removing a login that was never saved is fine", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		f, err := r.api.DeleteStrengthLevelLogin(r.alice, 2)
		wantFlash(t, f, err, "Strength Level login removed.")
	})
	t.Run("a database that cannot remove it is an outage", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		r.saveStrengthLevelLogin()
		r.flaky.failOn("DeleteIntegration")
		_, err := r.api.DeleteStrengthLevelLogin(r.alice, 2)
		wantUnavailable(t, err)
		if _, err := r.st.GetIntegration(context.Background(), 2, strengthLevelProvider); err != nil {
			t.Fatalf("the login went although the delete failed: %v", err)
		}
	})
}

func TestStartingTheStrengthLevelSyncTellsThePersonWhatHappened(t *testing.T) {
	sync := func(r *jobRig, days string) (apiclient.Flash, error) {
		return r.api.StartStrengthLevelSync(r.alice, 2, days, false)
	}
	ready := func(t *testing.T) *jobRig {
		r := newJobRig(t)
		r.allowStrengthLevel(true)
		r.saveStrengthLevelLogin()
		return r
	}
	t.Run("without a saved login the person is sent to save one", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(true)
		f, err := sync(r, "10")
		wantFlash(t, f, err, "Save your Strength Level login first.")
	})
	t.Run("a number of days that is not a number", func(t *testing.T) {
		f, err := sync(ready(t), "many")
		wantFlash(t, f, err, "between 1 and 3650")
	})
	t.Run("without a worker the person is told so", func(t *testing.T) {
		f, err := sync(ready(t), "")
		wantFlash(t, f, err, "No Strength Level worker is running")
	})
	t.Run("a database that cannot queue the sync is an outage", func(t *testing.T) {
		r := ready(t)
		r.workerFor(stillBusy(), kindStrengthLevelSync)
		r.flaky.failOn("EnqueueJob")
		_, err := sync(r, "10")
		wantUnavailable(t, err)
	})
	t.Run("a second sync while one runs is refused, and the status shows the first", func(t *testing.T) {
		r := ready(t)
		r.workerFor(stillBusy(), kindStrengthLevelSync)
		f, err := sync(r, "10")
		wantFlash(t, f, err, "Sync started in the background.")
		f, err = sync(r, "10")
		wantFlash(t, f, err, "A sync is already running.")
		r.waitForActiveJob(kindStrengthLevelSync)
		v, err := r.api.StrengthLevelStatus(r.alice, 2)
		if err != nil || v == nil || !v.Running {
			t.Fatalf("status %+v, error %v, want a running sync", v, err)
		}
	})
}

func TestTheStrengthLevelSyncStatus(t *testing.T) {
	t.Run("a person without the multi-day sync does not see the page", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		_, err := r.api.StrengthLevelStatus(r.alice, 2)
		if !apiclient.IsCode(err, apiclient.CodeNotFound) {
			t.Fatalf("error %v, want not found", err)
		}
	})
	t.Run("before any sync there is nothing to show", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(true)
		if v, err := r.api.StrengthLevelStatus(r.alice, 2); err != nil || v != nil {
			t.Fatalf("status %+v, error %v, want none", v, err)
		}
	})
	t.Run("a jobs table that is gone is an outage", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(true)
		breakTable(t, r.st, "worker_jobs")
		_, err := r.api.StrengthLevelStatus(r.alice, 2)
		wantUnavailable(t, err)
	})
}

func TestSendingOneDayToStrengthLevelTellsThePersonHowItEnded(t *testing.T) {
	day := time.Now().UTC().Format(dateLayout)
	send := func(r *jobRig) (apiclient.Flash, error) { return r.api.SendStrengthLevelDay(r.alice, 2, day) }
	ready := func(t *testing.T) *jobRig {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		r.saveStrengthLevelLogin()
		kg := 80.0 // the day needs a weight estimate to send
		if _, err := r.st.UpsertScaleReadings(context.Background(), 2, []models.ScaleReading{{MeasuredAt: time.Now(), WeightKg: &kg}}); err != nil {
			t.Fatal(err)
		}
		return r
	}
	t.Run("without a saved login the person is sent to Settings", func(t *testing.T) {
		r := newJobRig(t)
		r.allowStrengthLevel(false)
		f, err := send(r)
		wantFlash(t, f, err, "Save your Strength Level login in Settings first.")
	})
	t.Run("without a worker the person is told so", func(t *testing.T) {
		f, err := send(ready(t))
		wantFlash(t, f, err, "No Strength Level worker is running")
	})
	t.Run("a database that cannot queue the day is an outage", func(t *testing.T) {
		r := ready(t)
		r.workerFor(stillBusy(), kindStrengthLevelDay)
		r.flaky.failOn("EnqueueJob")
		_, err := send(r)
		wantUnavailable(t, err)
	})
	t.Run("a database that cannot be asked about the job is an outage", func(t *testing.T) {
		r := ready(t)
		r.workerFor(stillBusy(), kindStrengthLevelDay)
		r.flaky.failOn("GetJob")
		_, err := send(r)
		wantUnavailable(t, err)
	})
	t.Run("a day that takes long goes on in the background", func(t *testing.T) {
		impatient(t)
		r := ready(t)
		r.workerFor(stillBusy(), kindStrengthLevelDay)
		f, err := send(r)
		wantFlash(t, f, err, "in the background")
	})
	t.Run("a day that is canceled says so", func(t *testing.T) {
		r := ready(t)
		r.workerIdle(kindStrengthLevelDay)
		first := inTheBackground(func() (apiclient.Flash, error) { return send(r) })
		r.waitForActiveJob(kindStrengthLevelDay)
		if err := r.st.CancelJobs(context.Background(), 2, []string{kindStrengthLevelDay}, time.Now()); err != nil {
			t.Fatal(err)
		}
		got := <-first
		wantFlash(t, got.flash, got.err, "was canceled")
	})
	t.Run("a worker that fails the day gives its reason", func(t *testing.T) {
		r := ready(t)
		r.workerFor(failsWith("Strength Level said no"), kindStrengthLevelDay)
		f, err := send(r)
		wantFlash(t, f, err, "Strength Level said no")
	})
	t.Run("a day that was added", func(t *testing.T) {
		r := ready(t)
		r.workerFor(succeeds(`{"added":1}`), kindStrengthLevelDay)
		f, err := send(r)
		wantFlash(t, f, err, day+" added")
	})
}
