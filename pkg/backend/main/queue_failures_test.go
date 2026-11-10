package mainbackend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
)

func TestAJobIsNotQueuedWhenItsRequestCannotBeServed(t *testing.T) {
	ctx := context.Background()
	t.Run("an interactive job when the workers cannot be listed", func(t *testing.T) {
		f := newScriptedFixture(t)
		breakTable(t, f.st, "worker_instances")
		_, _, err := f.b.enqueueJob(ctx, enqueueSpec{UserID: f.user.ID, Kind: scriptKind, Origin: models.JobOriginUser, Interactive: true})
		if err == nil || errors.Is(err, errNoWorker) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("a payload that cannot be written", func(t *testing.T) {
		f := newScriptedFixture(t)
		_, _, err := f.b.enqueueJob(ctx, enqueueSpec{UserID: f.user.ID, Kind: scriptKind, Origin: models.JobOriginUser, Payload: make(chan int)})
		if err == nil {
			t.Fatal("a payload that is no JSON was queued")
		}
		if jobs := f.jobsOf(t); len(jobs) != 0 {
			t.Fatalf("%d jobs queued", len(jobs))
		}
	})
	t.Run("a timeout beyond the longest an administrator may set is cut", func(t *testing.T) {
		f := newScriptedFixture(t)
		j, _, err := f.b.enqueueJob(ctx, enqueueSpec{UserID: f.user.ID, Kind: scriptKind, Origin: models.JobOriginUser, Timeout: 1000 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if got := f.job(t, j.PublicID).Timeout; got != maxJobTimeoutSeconds*time.Second {
			t.Fatalf("timeout %v", got)
		}
	})
}

func TestWaitingForAJobEndsWhenTheCallerGivesUp(t *testing.T) {
	f := newScriptedFixture(t)
	j := f.queue(t, scriptKind, models.JobOriginUser)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	got, err := f.b.waitJob(ctx, j.PublicID, time.Minute)
	if !errors.Is(err, context.Canceled) || got.State != models.JobQueued {
		t.Fatalf("state %q, err %v", got.State, err)
	}
}

func TestTheQueueSweepReportsWhatItCannotDo(t *testing.T) {
	sweep := func(f *scriptedFixture) {
		old := maintainEvery.Get()
		maintainEvery.Set(0)
		t.Cleanup(func() { maintainEvery.Set(old) })
		f.b.lastSweep.Store(0)
		f.b.maintainQueue(context.Background())
	}
	t.Run("expired leases cannot be ended", func(t *testing.T) {
		f := newScriptedFixture(t)
		breakTable(t, f.st, "worker_jobs")
		logs := captureLog(t)
		sweep(f)
		if !strings.Contains(logs.String(), "end expired leases") {
			t.Fatalf("log %s", logs)
		}
	})
	t.Run("queued jobs that waited too long cannot be expired", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.flaky.expireQueued.Store(true)
		logs := captureLog(t)
		sweep(f)
		if !strings.Contains(logs.String(), "expire queued jobs") {
			t.Fatalf("log %s", logs)
		}
	})
}

func TestTextFromAWorkerIsClippedByCharacters(t *testing.T) {
	if got := clip("short", 10); got != "short" {
		t.Errorf("short text changed to %q", got)
	}
	if got := clip("ééééé", 3); got != "ééé…" {
		t.Errorf("clipped to %q", got)
	}
}

func TestANewAccountStillGetsCreatedWhenItsPriorityCannotBeSet(t *testing.T) {
	f := newScriptedFixture(t)
	f.setSetting(t, settingWorkersDefaultPriority, "20")
	if err := f.st.ExecForTests(context.Background(), "CREATE TRIGGER reject_priority BEFORE UPDATE OF job_priority ON users BEGIN SELECT RAISE(ABORT, 'the test broke the priority'); END"); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	u, err := f.b.createUser(context.Background(), "dave")
	if err != nil || u.JobPriority == 20 || !strings.Contains(logs.String(), "default job priority") {
		t.Fatalf("user %+v, err %v, log %s", u, err, logs)
	}
}

func TestProvidersAreDescribedForMessagesAndConnections(t *testing.T) {
	f := newScriptedFixture(t)
	if got := f.b.providerLabelOf(scriptKind); got != "Script" {
		t.Errorf("label of a known kind: %q", got)
	}
	if got := f.b.providerLabelOf("nothing.here"); got != "nothing.here" {
		t.Errorf("label of an unknown kind: %q", got)
	}
	p, ok := f.b.providerNamed("script")
	if !ok {
		t.Fatal("provider not found")
	}
	if f.b.connected(context.Background(), p, f.user.ID) {
		t.Error("a provider users do not connect to reported a connection")
	}
	if k := p.kind("nothing"); k != nil {
		t.Errorf("a kind that does not exist: %+v", k)
	}
	if permanent(nil) != nil {
		t.Error("no error became a permanent one")
	}
	cause := errors.New("cause")
	if got := errors.Unwrap(permanent(cause)); got != cause {
		t.Errorf("unwrapped %v", got)
	}
}
