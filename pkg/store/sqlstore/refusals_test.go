package sqlstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// Every change to a user that does not exist, or that is not allowed, says so.
func TestChangesAreRefusedForWhatDoesNotExistOrIsNotAllowed(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	const nobody = 424242
	for _, c := range []struct {
		name string
		do   func() error
		want error
	}{
		{"a profile of a user that does not exist", func() error { return s.UpdateUserProfile(ctx, nobody, "n", "n@example.com", nil) }, store.ErrNotFound},
		{"a time zone that does not exist", func() error { return s.UpdateUserTimezone(ctx, fx.user, "Nowhere/Land") }, store.ErrInvalid},
		{"an empty time zone", func() error { return s.UpdateUserTimezone(ctx, fx.user, "") }, store.ErrInvalid},
		{"the time zone Local", func() error { return s.UpdateUserTimezone(ctx, fx.user, "Local") }, store.ErrInvalid},
		{"the time zone of a user that does not exist", func() error { return s.UpdateUserTimezone(ctx, nobody, "Europe/Berlin") }, store.ErrNotFound},
		{"a role that does not exist", func() error { return s.SetUserRole(ctx, fx.user, "root") }, store.ErrInvalid},
		{"deleting a user that does not exist", func() error { return s.DeleteUser(ctx, nobody) }, store.ErrNotFound},
		{"deleting the last administrator", func() error { return s.DeleteUser(ctx, fx.admin) }, store.ErrLastAdmin},
		{"demoting the last administrator", func() error { return s.SetUserRole(ctx, fx.admin, models.RoleUser) }, store.ErrLastAdmin},
		{"a sign-in method of a user that does not exist", func() error {
			return s.PutIdentity(ctx, models.Identity{UserID: nobody, Provider: "x", Subject: "1"})
		}, store.ErrNotFound},
		{"a token of a user that does not exist", func() error {
			return s.CreateAccessToken(ctx, &models.AccessToken{UserID: nobody, Name: "n", Prefix: "p", Hash: "h"})
		}, store.ErrNotFound},
		{"removing a sign-in provider that does not exist", func() error { return s.DeleteAuthProvider(ctx, "nothing") }, store.ErrNotFound},
		{"hidden columns of a user that does not exist", func() error { return s.UpdateUserHiddenColumns(ctx, nobody, []string{"a"}) }, store.ErrNotFound},
		{"automatic sync of a user that does not exist", func() error { return s.SetUserStrengthLevelAuto(ctx, nobody, true) }, store.ErrNotFound},
		{"a password reset for a user that does not exist", func() error {
			return s.CreatePasswordReset(ctx, "h", nobody, nil, time.Now().Add(time.Hour))
		}, store.ErrNotFound},
		{"a two-step secret for a user that does not exist", func() error { return s.PutPendingTOTP(ctx, nobody, "S") }, store.ErrNotFound},
		{"recovery codes for a user that does not exist", func() error { return s.ReplaceRecoveryCodes(ctx, nobody, []string{"a"}) }, store.ErrNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.do(); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestReplacingRecoveryCodesWithNoneRemovesThem(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	if err := s.ReplaceRecoveryCodes(ctx, fx.user, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountRecoveryCodes(ctx, fx.user); err != nil || n != 0 {
		t.Fatalf("%d codes left, %v", n, err)
	}
}

func TestAnAuditEventIsClippedAndKeptWithoutAUserThatIsGone(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	gone := int64(424242)
	long := strings.Repeat("x", 2*maxAuditField)
	if err := s.AddAuditEvent(ctx, &models.AuditEvent{ActorID: &gone, TargetID: &fx.user, Action: "login", Detail: long, At: fx.at}); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListAuditEvents(ctx, store.AuditFilter{Limit: 1})
	if err != nil || len(events) != 1 {
		t.Fatalf("%v %v", events, err)
	}
	if got := events[0]; len(got.Detail) != maxAuditField || got.ActorID != nil || got.TargetID != nil || got.Action != "login" {
		t.Fatalf("stored event: detail %d bytes, actor %v, target %v", len(got.Detail), got.ActorID, got.TargetID)
	}
}

func TestAPasswordResetThatIsTakenByAnotherRequestIsNotUsedTwice(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	seedFaultData(t, s)
	// The link is found, but by the time it is deleted somebody else has used it.
	if err := s.ExecForTests(ctx, `CREATE TRIGGER first_come BEFORE DELETE ON password_resets BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompletePasswordReset(ctx, "reset-hash", "new"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want not found", err)
	}
}

func TestAnAdminDeleteThatMatchesNothingRollsTheAuditCleanupBack(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	if err := s.DeleteUser(ctx, fx.admin); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.GetUser(ctx, fx.admin); err != nil {
		t.Fatal(err)
	}
}

// ---- the job queue ----

func TestAJobRunsOnlyAfterItsStartTime(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	later := time.Now().Add(time.Hour)
	if _, created, err := s.EnqueueJob(ctx, models.NewJob{UserID: fx.user, Kind: "later.kind", Origin: models.JobOriginUser, MaxAttempts: 1, Timeout: time.Minute, RunAfter: later}); err != nil || !created {
		t.Fatalf("enqueue: %v %v", created, err)
	}
	req := models.LeaseRequest{Kinds: []string{"later.kind"}, Owner: "w", TokenHash: "t", LeaseFor: time.Minute}
	if _, ok, err := s.LeaseJob(ctx, req, time.Now()); ok || err != nil {
		t.Fatalf("leased before its time: %v %v", ok, err)
	}
	if _, ok, err := s.LeaseJob(ctx, req, later.Add(time.Second)); !ok || err != nil {
		t.Fatalf("not leased after its time: %v %v", ok, err)
	}
}

func TestAJobWhoseInsertIsSwallowedIsReportedNotInventedOrRetriedForever(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	if err := s.ExecForTests(ctx, `CREATE TRIGGER swallow BEFORE INSERT ON worker_jobs BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	job := models.NewJob{UserID: fx.user, Kind: testKind, Origin: models.JobOriginUser, MaxAttempts: 1, Timeout: time.Minute}
	if _, _, err := s.EnqueueJob(ctx, job); err == nil || !strings.Contains(err.Error(), "was not stored") {
		t.Fatalf("without a key: %v", err)
	}
	job.DedupeKey = "key-nobody-holds"
	if _, _, err := s.EnqueueJob(ctx, job); err == nil || !strings.Contains(err.Error(), "kept changing hands") {
		t.Fatalf("with a key: %v", err)
	}
}

func TestACompletionIsRefusedWhenTheLeaseWasLostInTheMeantime(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	// The state changes between the check and the update (the reaper got there first).
	if err := s.ExecForTests(ctx, `CREATE TRIGGER lost BEFORE UPDATE ON worker_jobs BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{models.OutcomeOK, models.OutcomeRetry, models.OutcomeFail} {
		if _, err := s.CompleteJob(ctx, fx.runningJob, "tok-w", models.Completion{Outcome: outcome}, time.Now()); !errors.Is(err, store.ErrLeaseLost) {
			t.Errorf("outcome %s: %v", outcome, err)
		}
	}
	if err := s.ReleaseJob(ctx, fx.runningJob, "tok-w", time.Now()); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("release: %v", err)
	}
	// The reaper skips a job whose worker reported in time after all.
	changed, err := s.ReapExpiredLeases(ctx, time.Now().Add(time.Hour), nil)
	if err != nil || len(changed) != 0 {
		t.Errorf("reaped %v %v", changed, err)
	}
}

func TestAnUnknownOutcomeOfAJobIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	if _, err := s.CompleteJob(ctx, fx.runningJob, "tok-w", models.Completion{Outcome: "shrug"}, time.Now()); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestARetryWithoutAReasonIsRecordedAsATemporaryFailure(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	j, err := s.CompleteJob(ctx, fx.runningJob, "tok-w", models.Completion{Outcome: models.OutcomeRetry}, time.Now())
	if err != nil || j.State != models.JobQueued || j.Error != "temporary failure" {
		t.Fatalf("%+v %v", j, err)
	}
}

func TestReleasingACanceledJobEndsItInsteadOfQueueingItAgain(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	if _, err := s.CancelJob(ctx, fx.runningJob, fx.user, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseJob(ctx, fx.runningJob, "tok-w", time.Now()); err != nil {
		t.Fatal(err)
	}
	if j, err := s.GetJob(ctx, fx.runningJob); err != nil || j.State != models.JobCanceled {
		t.Fatalf("%+v %v", j, err)
	}
}

func TestCancelingJobsOfNoKindDoesNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	fx := seedFaultData(t, s)
	if err := s.CancelJobs(ctx, fx.user, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if j, err := s.GetJob(ctx, fx.queuedJob); err != nil || j.State != models.JobQueued {
		t.Fatalf("%+v %v", j, err)
	}
}

func TestLeasingGivesUpWhenEveryCandidateIsTakenFirst(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, nil)
	seedFaultData(t, s)
	if err := s.ExecForTests(ctx, `CREATE TRIGGER faster BEFORE UPDATE ON worker_jobs WHEN NEW.state = 'running' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	req := models.LeaseRequest{Kinds: []string{testKind}, Owner: "w9", TokenHash: "t9", LeaseFor: time.Minute}
	if j, ok, err := s.LeaseJob(ctx, req, time.Now().Add(time.Hour)); ok || err != nil {
		t.Fatalf("leased %+v %v", j, err)
	}
}

// The payload of a job is sealed with the job's own context: moving it to another job fails.
func TestAJobPayloadMovedToAnotherJobCannotBeRead(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, testCipher(t, 1))
	fx := keyedSeed(t, s)
	if err := s.ExecForTests(ctx, `UPDATE worker_jobs SET payload = (SELECT payload FROM worker_jobs WHERE state = 'running') WHERE state = 'queued'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetJob(ctx, fx.queuedJob); err == nil || !strings.Contains(err.Error(), "payload of job") {
		t.Fatalf("got %v", err)
	}
}

func TestAJobResultMovedToAnotherJobCannotBeRead(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, testCipher(t, 1))
	fx := keyedSeed(t, s)
	if err := s.ExecForTests(ctx, `UPDATE worker_jobs SET result = (SELECT result FROM worker_jobs WHERE state = 'succeeded') WHERE state = 'queued'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetJob(ctx, fx.queuedJob); err == nil || !strings.Contains(err.Error(), "result of job") {
		t.Fatalf("got %v", err)
	}
}
