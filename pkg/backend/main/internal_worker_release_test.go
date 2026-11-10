package mainbackend

import (
	"context"
	"errors"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
)

func TestAWorkerThatGivesAJobBackLeavesItQueuedForTheNext(t *testing.T) {
	f := newScriptedFixture(t)
	j := f.queue(t, scriptKind, models.JobOriginUser)
	l := f.leaseKind(t, scriptKind)
	if err := f.client.Release(context.Background(), l.Job.ID, l.Token); err != nil {
		t.Fatal(err)
	}
	if got := f.job(t, j.PublicID); got.State != models.JobQueued {
		t.Fatalf("after the release: %+v", got)
	}
	if err := f.client.Release(context.Background(), l.Job.ID, l.Token); !errors.Is(err, workerapi.ErrLeaseLost) {
		t.Fatalf("a second release: %v", err)
	}
}

func TestAReportThatArrivesAfterTheLeaseRanOutIsTurnedAway(t *testing.T) {
	f := newScriptedFixture(t)
	f.queue(t, scriptKind, models.JobOriginUser)
	l := f.leaseKind(t, scriptKind)
	f.flaky.completeLost.Store(true)
	err := f.client.Complete(context.Background(), l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeFail, Error: "x"})
	if !errors.Is(err, workerapi.ErrLeaseLost) {
		t.Fatalf("%v", err)
	}
}
