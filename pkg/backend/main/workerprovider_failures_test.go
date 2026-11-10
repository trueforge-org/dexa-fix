package mainbackend

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
)

// wantPermanent checks an error that ends the job for good, with a message the person can read.
func wantPermanent(t *testing.T, err error, text string) {
	t.Helper()
	var p permanentError
	if !errors.As(err, &p) || !strings.Contains(err.Error(), text) {
		t.Fatalf("error %v, want a permanent one saying %q", err, text)
	}
}

// wantTransient checks an error that gives the job back to try again.
func wantTransient(t *testing.T, err error) {
	t.Helper()
	var p permanentError
	if err == nil || errors.As(err, &p) || errors.Is(err, errBadResult) {
		t.Fatalf("error %v, want a transient one", err)
	}
}

func (f *scriptedFixture) configureWithings(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{settingWithingsClientID: "id", settingWithingsSecret: "secret", settingWithingsRedirect: "https://dexa.example.com/cb"} {
		if err := f.st.PutSetting(context.Background(), k, v); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *scriptedFixture) connectWithings(t *testing.T, uid int64) {
	t.Helper()
	if err := f.st.PutIntegration(context.Background(), uid, providerWithings, []byte(`{"access_token":"a","refresh_token":"r"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestWithingsJobsAreRefusedForThingsOnlyTheServerCanFix(t *testing.T) {
	ctx := context.Background()
	lease := func(f *scriptedFixture, kind string, j models.Job) error {
		_, err := f.b.kinds[kind].Lease(ctx, f.b, j)
		return err
	}
	t.Run("the application's settings cannot be read", func(t *testing.T) {
		f := newScriptedFixture(t)
		breakTable(t, f.st, "app_settings")
		wantTransient(t, lease(f, kindWithingsConnect, models.Job{UserID: f.user.ID}))
		wantTransient(t, lease(f, kindWithingsImport, models.Job{UserID: f.user.ID}))
		p, _ := f.b.providerNamed(providerWithings)
		if _, err := p.OAuth.AuthURL(ctx, f.b, "state"); err == nil {
			t.Fatal("an address was made without the application's settings")
		}
	})
	t.Run("a server without the application's settings", func(t *testing.T) {
		f := newScriptedFixture(t)
		wantPermanent(t, lease(f, kindWithingsConnect, models.Job{UserID: f.user.ID}), "not configured")
		wantPermanent(t, lease(f, kindWithingsImport, models.Job{UserID: f.user.ID}), "not configured")
	})
	t.Run("a connection without the code from Withings", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.configureWithings(t)
		wantPermanent(t, lease(f, kindWithingsConnect, models.Job{UserID: f.user.ID, Payload: `{}`}), "no authorisation code")
		wantPermanent(t, lease(f, kindWithingsConnect, models.Job{UserID: f.user.ID, Payload: `nonsense`}), "no authorisation code")
	})
	t.Run("an import for a user who is not connected, or no longer exists", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.configureWithings(t)
		wantPermanent(t, lease(f, kindWithingsImport, models.Job{UserID: f.user.ID}), "not connected")
		f.connectWithings(t, f.user.ID)
		f.flaky.userMissing.Store(true)
		wantPermanent(t, lease(f, kindWithingsImport, models.Job{UserID: f.user.ID}), "no longer exists")
		f.flaky.userMissing.Store(false)
		f.flaky.userBroken.Store(true)
		wantTransient(t, lease(f, kindWithingsImport, models.Job{UserID: f.user.ID}))
		f.flaky.userBroken.Store(false)
		wantPermanent(t, lease(f, kindWithingsImport, models.Job{UserID: f.user.ID, Payload: "nonsense"}), "unreadable import")
	})
	t.Run("the stored connection cannot be read", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.configureWithings(t)
		breakTable(t, f.st, "user_integrations")
		wantTransient(t, lease(f, kindWithingsImport, models.Job{UserID: f.user.ID}))
	})
	t.Run("the users to schedule cannot be listed", func(t *testing.T) {
		f := newScriptedFixture(t)
		breakTable(t, f.st, "user_integrations")
		if _, err := f.b.kinds[kindWithingsImport].ScheduleFor(ctx, f.b); err == nil {
			t.Fatal("no error")
		}
	})
	t.Run("connected users are scheduled with a window of recent days", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.connectWithings(t, f.user.ID)
		targets, err := f.b.kinds[kindWithingsImport].ScheduleFor(ctx, f.b)
		if err != nil || len(targets) != 1 || targets[0].UserID != f.user.ID {
			t.Fatalf("targets %+v, err %v", targets, err)
		}
	})
}

func TestRenewedWithingsTokensAreKeptOnlyWhenTheyAreWholeAndWanted(t *testing.T) {
	ctx := context.Background()
	f := newScriptedFixture(t)
	good := json.RawMessage(`{"access_token":"new","refresh_token":"new"}`)
	job := models.Job{UserID: f.user.ID}
	imp := f.b.kinds[kindWithingsImport].Credentials

	for name, data := range map[string]json.RawMessage{
		"garbage": json.RawMessage(`nonsense`), "no access token": json.RawMessage(`{"refresh_token":"r"}`), "no refresh token": json.RawMessage(`{"access_token":"a"}`),
	} {
		if err := imp(ctx, f.b, job, data); !errors.Is(err, errBadResult) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := imp(ctx, f.b, job, good); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.GetIntegration(ctx, f.user.ID, providerWithings); err == nil {
		t.Fatal("an import brought back a connection the user removed")
	}
	if err := f.b.kinds[kindWithingsConnect].Credentials(ctx, f.b, job, good); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.GetIntegration(ctx, f.user.ID, providerWithings); err != nil {
		t.Fatalf("a connection did not keep its tokens: %v", err)
	}
	if err := imp(ctx, f.b, job, good); err != nil {
		t.Fatalf("an import of a connected user: %v", err)
	}
}

func TestAWithingsResultIsOnlyAppliedWhenItMakesSense(t *testing.T) {
	ctx := context.Background()
	apply := func(f *scriptedFixture, kind string, j models.Job, result string) error {
		_, err := f.b.kinds[kind].Apply(ctx, f.b, j, json.RawMessage(result))
		return err
	}
	t.Run("a connection whose tokens never arrived", func(t *testing.T) {
		f := newScriptedFixture(t)
		if err := apply(f, kindWithingsConnect, models.Job{UserID: f.user.ID}, `{}`); !errors.Is(err, errBadResult) {
			t.Fatalf("%v", err)
		}
		breakTable(t, f.st, "user_integrations")
		wantTransient(t, apply(f, kindWithingsConnect, models.Job{UserID: f.user.ID}, `{}`))
	})
	t.Run("an import with parameters nobody wrote", func(t *testing.T) {
		f := newScriptedFixture(t)
		if err := apply(f, kindWithingsImport, models.Job{UserID: f.user.ID, Payload: "nonsense"}, `{}`); !errors.Is(err, errBadResult) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("an import for a day that is not a day", func(t *testing.T) {
		f := newScriptedFixture(t)
		if err := apply(f, kindWithingsImport, models.Job{UserID: f.user.ID, Payload: `{"day":"yesterday"}`}, `{}`); !errors.Is(err, errBadResult) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("an import for a user who was deleted, or whose account cannot be read", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.flaky.userMissing.Store(true)
		if err := apply(f, kindWithingsImport, models.Job{UserID: f.user.ID}, `{}`); !errors.Is(err, errBadResult) {
			t.Fatalf("%v", err)
		}
		f.flaky.userMissing.Store(false)
		f.flaky.userBroken.Store(true)
		wantTransient(t, apply(f, kindWithingsImport, models.Job{UserID: f.user.ID}, `{}`))
	})
	t.Run("readings that cannot be stored", func(t *testing.T) {
		f := newScriptedFixture(t)
		breakTable(t, f.st, "scale_readings")
		wantTransient(t, apply(f, kindWithingsImport, models.Job{UserID: f.user.ID}, `{}`))
	})
}

func (f *scriptedFixture) enableStrengthLevel(t *testing.T, uid int64) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.SetUserStrengthLevelAuto(ctx, uid, true); err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutIntegration(ctx, uid, strengthLevelProvider, []byte(`{"email":"pat@example.com","password":"pw"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestStrengthLevelJobsAreRefusedForThingsOnlyTheUserCanFix(t *testing.T) {
	ctx := context.Background()
	lease := func(f *scriptedFixture, kind string, j models.Job) error {
		_, err := f.b.kinds[kind].Lease(ctx, f.b, j)
		return err
	}
	each := []string{kindStrengthLevelCheckLogin, kindStrengthLevelDay, kindStrengthLevelSync}

	t.Run("a user who no longer exists", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.flaky.userMissing.Store(true)
		for _, kind := range each {
			wantPermanent(t, lease(f, kind, models.Job{UserID: f.user.ID}), "no longer exists")
		}
	})
	t.Run("an account that cannot be read", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.flaky.userBroken.Store(true)
		for _, kind := range each {
			wantTransient(t, lease(f, kind, models.Job{UserID: f.user.ID}))
		}
	})
	t.Run("an account the administrator did not allow", func(t *testing.T) {
		f := newScriptedFixture(t)
		for _, kind := range each {
			wantPermanent(t, lease(f, kind, models.Job{UserID: f.user.ID}), "not enabled for this account")
		}
	})
	t.Run("a login that was never saved, cannot be read or is unreadable", func(t *testing.T) {
		f := newScriptedFixture(t)
		if err := f.st.SetUserStrengthLevelAuto(ctx, f.user.ID, true); err != nil {
			t.Fatal(err)
		}
		for _, kind := range each[1:] {
			wantPermanent(t, lease(f, kind, models.Job{UserID: f.user.ID}), "Save your Strength Level login")
		}
		if err := f.st.PutIntegration(ctx, f.user.ID, strengthLevelProvider, []byte(`nonsense`)); err != nil {
			t.Fatal(err)
		}
		for _, kind := range each[1:] {
			wantPermanent(t, lease(f, kind, models.Job{UserID: f.user.ID}), "unreadable, save it again")
		}
		breakTable(t, f.st, "user_integrations")
		for _, kind := range each[1:] {
			wantTransient(t, lease(f, kind, models.Job{UserID: f.user.ID}))
		}
	})
	t.Run("a check without a login, a day without a payload, a sync of no days", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.enableStrengthLevel(t, f.user.ID)
		wantPermanent(t, lease(f, kindStrengthLevelCheckLogin, models.Job{UserID: f.user.ID, Payload: `{}`}), "no login to check")
		wantPermanent(t, lease(f, kindStrengthLevelDay, models.Job{UserID: f.user.ID, Payload: `nonsense`}), "unreadable job")
		wantPermanent(t, lease(f, kindStrengthLevelSync, models.Job{UserID: f.user.ID, Payload: `{"days":0}`}), "unreadable job")
		wantPermanent(t, lease(f, kindStrengthLevelSync, models.Job{UserID: f.user.ID, Payload: `{"days":99999}`}), "unreadable job")
		wantPermanent(t, lease(f, kindStrengthLevelDay, models.Job{UserID: f.user.ID, Payload: `{"day":"2001-01-01"}`}), "no weight estimate for 2001-01-01")
	})
	t.Run("estimates that cannot be computed", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.enableStrengthLevel(t, f.user.ID)
		breakTable(t, f.st, "scale_readings")
		wantTransient(t, lease(f, kindStrengthLevelDay, models.Job{UserID: f.user.ID, Payload: `{"day":"2001-01-01"}`}))
		wantTransient(t, lease(f, kindStrengthLevelSync, models.Job{UserID: f.user.ID, Payload: `{"days":7}`}))
	})
}

func TestStrengthLevelSyncsAreScheduledForAllowedUsersOnly(t *testing.T) {
	ctx := context.Background()
	f := newScriptedFixture(t)
	f.enableStrengthLevel(t, f.user.ID)
	if err := f.st.PutIntegration(ctx, f.admin.ID, strengthLevelProvider, []byte(`{"email":"a@b.c","password":"p"}`)); err != nil { // connected, not allowed
		t.Fatal(err)
	}
	targets, err := scheduleStrengthLevelSync(ctx, f.b)
	if err != nil || len(targets) != 1 || targets[0].UserID != f.user.ID || targets[0].Payload.(slSyncPayload).Days != defaultScheduledDays {
		t.Fatalf("targets %+v, err %v", targets, err)
	}
	// without a configured number of days the default holds
	live := *f.b.cfg()
	kinds := map[string]kindSettings{}
	for k, v := range live.workers.Kinds {
		kinds[k] = v
	}
	ks := kinds[kindStrengthLevelSync]
	ks.Options = nil
	kinds[kindStrengthLevelSync] = ks
	live.workers.Kinds = kinds
	f.b.swapSettings(&live)
	if targets, err = scheduleStrengthLevelSync(ctx, f.b); err != nil || targets[0].Payload.(slSyncPayload).Days != defaultScheduledDays {
		t.Fatalf("targets %+v, err %v", targets, err)
	}
	breakTable(t, f.st, "user_integrations")
	if _, err := scheduleStrengthLevelSync(ctx, f.b); err == nil {
		t.Fatal("no error without the integrations")
	}
}

func TestStrengthLevelResultsAreOnlyKeptWhenTheyMakeSense(t *testing.T) {
	ctx := context.Background()
	login := `{"email":"pat@example.com","password":"pw"}`
	apply := func(f *scriptedFixture, kind string, j models.Job, result string) (string, error) {
		return f.b.kinds[kind].Apply(ctx, f.b, j, json.RawMessage(result))
	}
	t.Run("results that are not what was asked", func(t *testing.T) {
		f := newScriptedFixture(t)
		for _, kind := range []string{kindStrengthLevelCheckLogin, kindStrengthLevelDay, kindStrengthLevelSync} {
			if _, err := apply(f, kind, models.Job{UserID: f.user.ID}, `nonsense`); !errors.Is(err, errBadResult) {
				t.Errorf("%s: %v", kind, err)
			}
		}
	})
	t.Run("a login Strength Level refused is counted like a wrong password", func(t *testing.T) {
		f := newScriptedFixture(t)
		for i := 0; i < maxStrengthLevelLoginFail; i++ {
			if _, err := apply(f, kindStrengthLevelCheckLogin, models.Job{UserID: f.user.ID, Payload: login}, `{"login_ok":false}`); err != nil {
				t.Fatal(err)
			}
		}
		if !f.b.limiter.Blocked(ctx, "sl:"+strconv.FormatInt(f.user.ID, 10), maxStrengthLevelLoginFail) {
			t.Fatal("the refusals were not counted")
		}
		if _, err := f.st.GetIntegration(ctx, f.user.ID, strengthLevelProvider); err == nil {
			t.Fatal("a refused login was kept")
		}
	})
	t.Run("a login whose permission was removed meanwhile is not kept", func(t *testing.T) {
		f := newScriptedFixture(t)
		if _, err := apply(f, kindStrengthLevelCheckLogin, models.Job{UserID: f.user.ID, Payload: login}, `{"login_ok":true}`); !errors.Is(err, errBadResult) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a login nobody can read is not kept", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.enableStrengthLevel(t, f.user.ID)
		if _, err := apply(f, kindStrengthLevelCheckLogin, models.Job{UserID: f.user.ID, Payload: `{}`}, `{"login_ok":true}`); !errors.Is(err, errBadResult) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a login that cannot be stored", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.enableStrengthLevel(t, f.user.ID)
		rejectWritesOn(t, f.st, "user_integrations", "INSERT", "UPDATE")
		_, err := apply(f, kindStrengthLevelCheckLogin, models.Job{UserID: f.user.ID, Payload: login}, `{"login_ok":true}`)
		wantTransient(t, err)
	})
	t.Run("a sync that could not write some days says so in the log", func(t *testing.T) {
		f := newScriptedFixture(t)
		logs := captureLog(t)
		out, err := apply(f, kindStrengthLevelSync, models.Job{UserID: f.user.ID, Payload: `{"days":7}`}, `{"failed":2,"failed_messages":["no such day"]}`)
		if err != nil || !strings.Contains(out, `"days":7`) || !strings.Contains(logs.String(), "no such day") {
			t.Fatalf("%s, %v, log %s", out, err, logs)
		}
	})
}

func TestTheLastStrengthLevelSyncIsDescribedForEveryState(t *testing.T) {
	ctx := context.Background()
	f := newScriptedFixture(t)
	f.enableStrengthLevel(t, f.user.ID)
	set := func(state, errText, result string) {
		t.Helper()
		if err := f.st.ExecForTests(ctx, "UPDATE worker_jobs SET state = ?, error = ?, result = ?", state, errText, result); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := f.b.slJobViewOf(ctx, f.user.ID); err != nil || v != nil {
		t.Fatalf("before any sync: %+v %v", v, err)
	}
	if _, _, err := f.b.enqueueJob(ctx, enqueueSpec{UserID: f.user.ID, Kind: kindStrengthLevelSync, Origin: models.JobOriginUser}); err != nil {
		t.Fatal(err)
	}
	summary, _ := json.Marshal(slSyncSummary{ExportResult: workerapi.ExportResult{Added: 1, Failed: 2}, Days: 7, Update: true})
	for state, want := range map[string]string{
		models.JobQueued:    "waiting for a worker",
		models.JobCanceled:  "was canceled",
		models.JobFailed:    "stopped: boom",
		models.JobExpired:   "stopped: boom",
		models.JobSucceeded: "2 could not be written",
	} {
		set(state, "boom", string(summary))
		v, err := f.b.slJobViewOf(ctx, f.user.ID)
		if err != nil || !strings.Contains(v.Text, want) {
			t.Errorf("state %s: %+v %v, want %q", state, v, err, want)
		}
	}
	f.flaky.jobBroken.Store(true)
	if _, err := f.b.slJobViewOf(ctx, f.user.ID); err == nil {
		t.Error("a job that cannot be read was described")
	}
}

func TestStoppingStrengthLevelFailsSoftWhenTheJobsCannotBeCanceled(t *testing.T) {
	f := newScriptedFixture(t)
	breakTable(t, f.st, "worker_jobs")
	logs := captureLog(t)
	f.b.stopStrengthLevel(context.Background(), f.user.ID)
	if !strings.Contains(logs.String(), "stop the Strength Level jobs") {
		t.Fatalf("log %s", logs)
	}
}
