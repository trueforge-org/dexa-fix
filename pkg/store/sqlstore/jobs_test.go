package sqlstore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

const testKind = "demo.import"

// jobFixture returns a store with an administrator and some ordinary users.
func jobFixture(t *testing.T, users int) (*Store, context.Context, []models.User) {
	t.Helper()
	s := newStore(t)
	ctx := context.Background()
	var us []models.User
	for i := 0; i <= users; i++ { // the first user created is the administrator
		u, err := s.CreateUser(ctx, "user"+string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		us = append(us, u)
	}
	if us[0].Role != models.RoleAdmin || us[1].Role == models.RoleAdmin {
		t.Fatalf("unexpected roles: %q %q", us[0].Role, us[1].Role)
	}
	return s, ctx, us
}

func enqueue(t *testing.T, s *Store, ctx context.Context, userID int64, kind string, interactive bool, key string) models.Job {
	t.Helper()
	j, created, err := s.EnqueueJob(ctx, models.NewJob{UserID: userID, Kind: kind, Origin: models.JobOriginUser,
		Interactive: interactive, DedupeKey: key, Payload: "secret-" + key, MaxAttempts: 3, Timeout: time.Minute})
	if err != nil || !created {
		t.Fatalf("enqueue: created=%v err=%v", created, err)
	}
	return j
}

func lease(t *testing.T, s *Store, ctx context.Context, at time.Time, owner string, kinds ...string) (models.Job, bool) {
	t.Helper()
	j, ok, err := s.LeaseJob(ctx, models.LeaseRequest{Kinds: kinds, Owner: owner, TokenHash: "tok-" + owner, LeaseFor: time.Minute}, at)
	if err != nil {
		t.Fatal(err)
	}
	return j, ok
}

func TestJobLeaseOrder(t *testing.T) {
	s, ctx, us := jobFixture(t, 3)
	admin, low, high, mid := us[0], us[1], us[2], us[3]
	for _, p := range []struct {
		u models.User
		n int
	}{{low, 20}, {high, 90}} {
		if err := s.SetUserJobPriority(ctx, p.u.ID, p.n); err != nil {
			t.Fatal(err)
		}
	}
	// The administrator has the lowest priority number of all and must still go first.
	if err := s.SetUserJobPriority(ctx, admin.ID, 1); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, ctx, low.ID, testKind, false, "low")
	enqueue(t, s, ctx, mid.ID, testKind, false, "mid")
	enqueue(t, s, ctx, mid.ID, testKind, true, "mid-interactive")
	enqueue(t, s, ctx, high.ID, testKind, false, "high")
	enqueue(t, s, ctx, admin.ID, testKind, false, "admin")

	var order []int64
	for i := 0; i < 5; i++ {
		j, ok := lease(t, s, ctx, time.Now(), "w", testKind)
		if !ok {
			t.Fatalf("lease %d: nothing", i)
		}
		order = append(order, j.UserID)
		if j.State != models.JobRunning || j.Attempts != 1 || j.Payload == "" || j.DeadlineAt == nil {
			t.Fatalf("leased job: %+v", j)
		}
	}
	want := []int64{admin.ID, high.ID, mid.ID, mid.ID, low.ID}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("lease order %v, want %v", order, want)
		}
	}
	if _, ok := lease(t, s, ctx, time.Now(), "w", testKind); ok {
		t.Fatal("the queue should be empty")
	}
}

func TestJobInteractiveBeforeBackground(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	enqueue(t, s, ctx, us[1].ID, testKind, false, "background")
	enqueue(t, s, ctx, us[1].ID, testKind, true, "waited-for")
	j, _ := lease(t, s, ctx, time.Now(), "w", testKind)
	if j.DedupeKey != "waited-for" {
		t.Fatalf("leased %q first", j.DedupeKey)
	}
}

func TestJobDedupe(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	first := enqueue(t, s, ctx, us[1].ID, testKind, false, "once")
	again, created, err := s.EnqueueJob(ctx, models.NewJob{UserID: us[1].ID, Kind: testKind, Origin: models.JobOriginSchedule,
		DedupeKey: "once", MaxAttempts: 3, Timeout: time.Minute})
	if err != nil || created || again.PublicID != first.PublicID {
		t.Fatalf("second enqueue: created=%v same=%v err=%v", created, again.PublicID == first.PublicID, err)
	}
	// Once it ended, the key is free again.
	j, _ := lease(t, s, ctx, time.Now(), "w", testKind)
	if _, err := s.CompleteJob(ctx, j.PublicID, "tok-w", models.Completion{Outcome: models.OutcomeOK, Result: "{}"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.EnqueueJob(ctx, models.NewJob{UserID: us[1].ID, Kind: testKind, Origin: models.JobOriginUser,
		DedupeKey: "once", MaxAttempts: 3, Timeout: time.Minute}); err != nil || !created {
		t.Fatalf("enqueue after the first ended: created=%v err=%v", created, err)
	}
	// An empty key never collides.
	enqueue(t, s, ctx, us[1].ID, testKind, false, "")
	enqueue(t, s, ctx, us[1].ID, testKind, false, "")
}

func TestJobEnqueueValidation(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	for _, n := range []models.NewJob{
		{UserID: us[1].ID, Kind: "", Origin: "user", MaxAttempts: 1, Timeout: time.Minute},
		{UserID: us[1].ID, Kind: testKind, Origin: "user", MaxAttempts: 0, Timeout: time.Minute},
		{UserID: us[1].ID, Kind: testKind, Origin: "user", MaxAttempts: 1, Timeout: 0},
	} {
		if _, _, err := s.EnqueueJob(ctx, n); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("%+v: %v", n, err)
		}
	}
	if _, _, err := s.EnqueueJob(ctx, models.NewJob{UserID: 9999, Kind: testKind, Origin: "user", MaxAttempts: 1, Timeout: time.Minute}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

// Many workers asking at once: every job goes to exactly one of them.
func TestJobLeaseRace(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	const jobs, workers = 6, 12
	for i := 0; i < jobs; i++ {
		enqueue(t, s, ctx, us[1].ID, testKind, false, "")
	}
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			j, ok, err := s.LeaseJob(ctx, models.LeaseRequest{Kinds: []string{testKind}, Owner: "w", TokenHash: "t", LeaseFor: time.Minute}, time.Now())
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				got[j.PublicID]++
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(got) != jobs {
		t.Fatalf("leased %d distinct jobs, want %d (%v)", len(got), jobs, got)
	}
	for id, n := range got {
		if n != 1 {
			t.Fatalf("job %s leased %d times", id, n)
		}
	}
}

func TestJobKindsPausedAndLimit(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	enqueue(t, s, ctx, us[1].ID, "a.job", false, "")
	enqueue(t, s, ctx, us[1].ID, "a.job", false, "")
	enqueue(t, s, ctx, us[1].ID, "b.job", false, "")
	req := func(extra func(*models.LeaseRequest)) models.LeaseRequest {
		r := models.LeaseRequest{Kinds: []string{"a.job", "b.job"}, Owner: "w", TokenHash: "t", LeaseFor: time.Minute}
		extra(&r)
		return r
	}
	// A paused kind is skipped.
	j, ok, err := s.LeaseJob(ctx, req(func(r *models.LeaseRequest) { r.Paused = []string{"a.job"} }), time.Now())
	if err != nil || !ok || j.Kind != "b.job" {
		t.Fatalf("paused: %v %v %v", j.Kind, ok, err)
	}
	// One a.job may run at a time.
	lim := func(r *models.LeaseRequest) { r.MaxRunning = map[string]int{"a.job": 1} }
	if j, ok, _ = s.LeaseJob(ctx, req(lim), time.Now()); !ok || j.Kind != "a.job" {
		t.Fatalf("first a.job: %v %v", j.Kind, ok)
	}
	if _, ok, _ = s.LeaseJob(ctx, req(lim), time.Now()); ok {
		t.Fatal("the limit of running a.job jobs was ignored")
	}
}

func TestJobFencingAndRetryBudget(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	enqueue(t, s, ctx, us[1].ID, testKind, false, "k")
	base := time.Now().UTC()

	for attempt := 1; attempt <= 3; attempt++ {
		j, ok := lease(t, s, ctx, base.Add(time.Duration(attempt)*time.Hour), "w", testKind)
		if !ok || j.Attempts != attempt {
			t.Fatalf("attempt %d: ok=%v job=%+v", attempt, ok, j)
		}
		// Somebody else's token gets nowhere.
		if _, err := s.CompleteJob(ctx, j.PublicID, "tok-other", models.Completion{Outcome: models.OutcomeOK}, base); !errors.Is(err, store.ErrLeaseLost) {
			t.Fatalf("wrong token: %v", err)
		}
		retryAt := base.Add(time.Duration(attempt)*time.Hour + 30*time.Minute)
		done, err := s.CompleteJob(ctx, j.PublicID, "tok-w", models.Completion{Outcome: models.OutcomeRetry, Error: "boom", RetryAt: retryAt}, base.Add(time.Duration(attempt)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		// The same attempt cannot be reported twice.
		if _, err := s.CompleteJob(ctx, j.PublicID, "tok-w", models.Completion{Outcome: models.OutcomeOK}, base); !errors.Is(err, store.ErrLeaseLost) {
			t.Fatalf("second completion: %v", err)
		}
		if attempt < 3 {
			if done.State != models.JobQueued || done.Error != "boom" {
				t.Fatalf("after attempt %d: %+v", attempt, done)
			}
			if _, ok := lease(t, s, ctx, retryAt.Add(-time.Minute), "w", testKind); ok {
				t.Fatal("a retry ran before its time")
			}
			continue
		}
		if done.State != models.JobFailed || done.Payload != "" || done.FinishedAt == nil {
			t.Fatalf("after the last attempt: %+v", done)
		}
	}
	if _, ok := lease(t, s, ctx, base.Add(24*time.Hour), "w", testKind); ok {
		t.Fatal("a failed job came back")
	}
}

func TestJobPermanentFailureAndResult(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	enqueue(t, s, ctx, us[1].ID, testKind, false, "a")
	enqueue(t, s, ctx, us[1].ID, testKind, false, "b")
	a, _ := lease(t, s, ctx, time.Now(), "w", testKind)
	failed, err := s.CompleteJob(ctx, a.PublicID, "tok-w", models.Completion{Outcome: models.OutcomeFail, Error: "bad login"}, time.Now())
	if err != nil || failed.State != models.JobFailed || failed.Attempts != 1 || failed.Payload != "" {
		t.Fatalf("fail: %+v %v", failed, err)
	}
	b, _ := lease(t, s, ctx, time.Now(), "w", testKind)
	ok, err := s.CompleteJob(ctx, b.PublicID, "tok-w", models.Completion{Outcome: models.OutcomeOK, Result: `{"n":1}`}, time.Now())
	if err != nil || ok.State != models.JobSucceeded || ok.Result != `{"n":1}` || ok.Payload != "" {
		t.Fatalf("ok: %+v %v", ok, err)
	}
	if _, err := s.CompleteJob(ctx, b.PublicID, "tok-w", models.Completion{Outcome: "nonsense"}, time.Now()); err == nil {
		t.Fatal("an unknown outcome was accepted")
	}
}

func TestJobReapExpiredLeases(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	enqueue(t, s, ctx, us[1].ID, testKind, false, "k")
	t0 := time.Now().UTC()
	backoff := func(j models.Job) time.Duration { return time.Duration(j.Attempts) * 10 * time.Minute }

	for attempt := 1; attempt <= 3; attempt++ {
		at := t0.Add(time.Duration(attempt) * 24 * time.Hour)
		j, ok := lease(t, s, ctx, at, "w", testKind)
		if !ok || j.Attempts != attempt {
			t.Fatalf("attempt %d: %v %+v", attempt, ok, j)
		}
		// A heartbeat in time keeps the lease; none ends it.
		if _, err := s.HeartbeatJob(ctx, j.PublicID, "tok-w", time.Minute, nil, at.Add(30*time.Second)); err != nil {
			t.Fatal(err)
		}
		if n, _ := s.ReapExpiredLeases(ctx, at.Add(time.Minute), backoff); len(n) != 0 {
			t.Fatalf("reaped a live lease: %v", n)
		}
		if _, err := s.HeartbeatJob(ctx, j.PublicID, "", time.Minute, nil, at.Add(30*time.Second)); !errors.Is(err, store.ErrLeaseLost) {
			t.Fatalf("heartbeat without a token: %v", err)
		}
		late := at.Add(10 * time.Minute)
		if _, err := s.HeartbeatJob(ctx, j.PublicID, "tok-w", time.Minute, nil, late); !errors.Is(err, store.ErrLeaseLost) {
			t.Fatalf("late heartbeat: %v", err)
		}
		reaped, err := s.ReapExpiredLeases(ctx, late, backoff)
		if err != nil || len(reaped) != 1 {
			t.Fatalf("reap: %v %v", reaped, err)
		}
		// The hung worker wakes up and tries to report: it is too late.
		if _, err := s.CompleteJob(ctx, j.PublicID, "tok-w", models.Completion{Outcome: models.OutcomeOK}, late); !errors.Is(err, store.ErrLeaseLost) {
			t.Fatalf("completion after reap: %v", err)
		}
		got, _ := s.GetJob(ctx, j.PublicID)
		if attempt < 3 {
			if got.State != models.JobQueued || !got.RunAfter.Equal(late.Add(backoff(models.Job{Attempts: attempt})).Truncate(time.Microsecond)) && got.RunAfter.Before(late) {
				t.Fatalf("after reap %d: %+v", attempt, got)
			}
			continue
		}
		if got.State != models.JobFailed || got.Error == "" {
			t.Fatalf("after the last reap: %+v", got)
		}
	}
}

func TestJobDeadline(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	enqueue(t, s, ctx, us[1].ID, testKind, false, "k") // time limit: one minute
	t0 := time.Now().UTC()
	j, _ := lease(t, s, ctx, t0, "w", testKind)
	// The lease is renewed all the time, but the attempt has run out of time.
	if _, err := s.HeartbeatJob(ctx, j.PublicID, "tok-w", time.Hour, nil, t0.Add(50*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HeartbeatJob(ctx, j.PublicID, "tok-w", time.Hour, nil, t0.Add(2*time.Minute)); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("heartbeat past the deadline: %v", err)
	}
	reaped, err := s.ReapExpiredLeases(ctx, t0.Add(2*time.Minute), func(models.Job) time.Duration { return time.Minute })
	if err != nil || len(reaped) != 1 || reaped[0].State != models.JobQueued {
		t.Fatalf("reap: %+v %v", reaped, err)
	}
}

func TestJobCancel(t *testing.T) {
	s, ctx, us := jobFixture(t, 2)
	q := enqueue(t, s, ctx, us[1].ID, testKind, false, "queued")
	if _, err := s.CancelJob(ctx, q.PublicID, us[2].ID, time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("canceling another user's job: %v", err)
	}
	c, err := s.CancelJob(ctx, q.PublicID, us[1].ID, time.Now())
	if err != nil || c.State != models.JobCanceled || c.Payload != "" {
		t.Fatalf("cancel queued: %+v %v", c, err)
	}

	enqueue(t, s, ctx, us[1].ID, testKind, false, "running")
	j, _ := lease(t, s, ctx, time.Now(), "w", testKind)
	if c, err := s.CancelJob(ctx, j.PublicID, 0, time.Now()); err != nil || c.State != models.JobRunning || !c.CancelRequested {
		t.Fatalf("cancel running: %+v %v", c, err)
	}
	cancel, err := s.HeartbeatJob(ctx, j.PublicID, "tok-w", time.Minute, nil, time.Now())
	if err != nil || !cancel {
		t.Fatalf("heartbeat should tell the worker to stop: %v %v", cancel, err)
	}
	// A worker that stops because of it reports a retry; the job ends as canceled and is not retried.
	done, err := s.CompleteJob(ctx, j.PublicID, "tok-w", models.Completion{Outcome: models.OutcomeRetry, Error: "stopped"}, time.Now())
	if err != nil || done.State != models.JobCanceled {
		t.Fatalf("complete after cancel: %+v %v", done, err)
	}

	// Canceling by kind for a user (a login was removed).
	enqueue(t, s, ctx, us[2].ID, "x.sync", false, "")
	if err := s.CancelJobs(ctx, us[2].ID, []string{"x.sync"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListJobs(ctx, models.JobFilter{UserID: us[2].ID, States: []string{models.JobCanceled}}); len(list) != 1 {
		t.Fatalf("canceled by kind: %v", list)
	}
}

func TestJobReleaseDoesNotCountAttempt(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	enqueue(t, s, ctx, us[1].ID, testKind, false, "k")
	j, _ := lease(t, s, ctx, time.Now(), "w", testKind)
	if err := s.ReleaseJob(ctx, j.PublicID, "tok-w", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseJob(ctx, j.PublicID, "tok-w", time.Now()); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("second release: %v", err)
	}
	again, ok := lease(t, s, ctx, time.Now().Add(time.Second), "w2", testKind)
	if !ok || again.Attempts != 1 || again.LeaseOwner != "w2" {
		t.Fatalf("after release: %v %+v", ok, again)
	}
}

func TestJobExpiryAndPurge(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	soon := time.Now().UTC().Add(time.Second)
	j, _, err := s.EnqueueJob(ctx, models.NewJob{UserID: us[1].ID, Kind: testKind, Origin: models.JobOriginUser,
		Payload: "code", MaxAttempts: 1, Timeout: time.Minute, ExpiresAt: &soon})
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if _, ok := lease(t, s, ctx, later, "w", testKind); ok {
		t.Fatal("an expired job was leased")
	}
	if n, err := s.ExpireQueuedJobs(ctx, later); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	got, _ := s.GetJob(ctx, j.PublicID)
	if got.State != models.JobExpired || got.Payload != "" {
		t.Fatalf("expired: %+v", got)
	}
	if n, _ := s.PurgeJobs(ctx, later.Add(-time.Hour)); n != 0 {
		t.Fatalf("purged a recent job: %d", n)
	}
	if n, _ := s.PurgeJobs(ctx, later.Add(time.Hour)); n != 1 {
		t.Fatalf("purge: %d", n)
	}
	if _, err := s.GetJob(ctx, j.PublicID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("purged job: %v", err)
	}
}

func TestJobProgressAndStates(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	enqueue(t, s, ctx, us[1].ID, testKind, false, "k")
	j, _ := lease(t, s, ctx, time.Now(), "w", testKind)
	p := `{"done":3}`
	if _, err := s.HeartbeatJob(ctx, j.PublicID, "tok-w", time.Minute, &p, time.Now()); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListJobs(ctx, models.JobFilter{UserID: us[1].ID, Kinds: []string{testKind}})
	if err != nil || len(list) != 1 || list[0].Progress != p || list[0].Payload != "" {
		t.Fatalf("list: %+v %v", list, err)
	}
	s.CompleteJob(ctx, j.PublicID, "tok-w", models.Completion{Outcome: models.OutcomeFail, Error: "x"}, time.Now())
	states, err := s.LastFinishedStates(ctx, testKind, 5, time.Now().Add(-time.Hour))
	if err != nil || len(states) != 1 || states[0] != models.JobFailed {
		t.Fatalf("states: %v %v", states, err)
	}
	if states, _ = s.LastFinishedStates(ctx, testKind, 5, time.Now().Add(time.Hour)); len(states) != 0 {
		t.Fatalf("states before the cutoff were counted: %v", states)
	}
}

func TestWorkersAndSchedules(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	now := time.Now().UTC()
	if err := s.TouchWorker(ctx, "w1", []string{"a.job", "b.job"}, "1.0", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchWorker(ctx, "w1", []string{"a.job"}, "1.1", now); err != nil {
		t.Fatal(err)
	}
	s.TouchWorker(ctx, "w2", []string{"b.job"}, "1.1", now.Add(-2*time.Hour))
	ws, err := s.ListWorkers(ctx, now.Add(-time.Minute))
	if err != nil || len(ws) != 1 || ws[0].ID != "w1" || len(ws[0].Kinds) != 1 || ws[0].Version != "1.1" {
		t.Fatalf("workers: %+v %v", ws, err)
	}
	if n, _ := s.PurgeWorkers(ctx, now.Add(-time.Hour)); n != 1 {
		t.Fatalf("purged %d workers", n)
	}

	at := now
	if err := s.PutJobSchedule(ctx, models.JobSchedule{UserID: us[1].ID, Kind: testKind, LastEnqueuedAt: &at, ConsecutiveFailures: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutJobSchedule(ctx, models.JobSchedule{UserID: us[1].ID, Kind: testKind, ConsecutiveFailures: 5, PausedReason: "too many failures"}); err != nil {
		t.Fatal(err)
	}
	sc, err := s.ListJobSchedules(ctx, testKind)
	if err != nil || len(sc) != 1 || sc[0].ConsecutiveFailures != 5 || sc[0].PausedReason == "" {
		t.Fatalf("schedules: %+v %v", sc, err)
	}
	if one, err := s.GetJobSchedule(ctx, us[1].ID, testKind); err != nil || one.ConsecutiveFailures != 5 {
		t.Fatalf("one schedule: %+v %v", one, err)
	}
	if _, err := s.GetJobSchedule(ctx, us[1].ID, "none.kind"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing schedule: %v", err)
	}
	if err := s.DeleteJobSchedule(ctx, us[1].ID, testKind); err != nil {
		t.Fatal(err)
	}
	if sc, _ := s.ListJobSchedules(ctx, testKind); len(sc) != 0 {
		t.Fatalf("schedule not deleted: %+v", sc)
	}
	if err := s.PutJobSchedule(ctx, models.JobSchedule{UserID: 9999, Kind: testKind}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("schedule for an unknown user: %v", err)
	}
}

func TestUserJobPriority(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	u, _ := s.GetUser(ctx, us[1].ID)
	if u.JobPriority != models.DefaultJobPriority {
		t.Fatalf("default priority %d", u.JobPriority)
	}
	for _, bad := range []int{0, -1, 101} {
		if err := s.SetUserJobPriority(ctx, u.ID, bad); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("priority %d: %v", bad, err)
		}
	}
	if err := s.SetUserJobPriority(ctx, 9999, 10); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	if err := s.SetUserJobPriority(ctx, u.ID, 100); err != nil {
		t.Fatal(err)
	}
	if u, _ = s.GetUser(ctx, u.ID); u.JobPriority != 100 {
		t.Fatalf("priority %d", u.JobPriority)
	}
}

func TestListUsersWithIntegration(t *testing.T) {
	s, ctx, us := jobFixture(t, 2)
	s.PutIntegration(ctx, us[2].ID, "demo", []byte("x"))
	s.PutIntegration(ctx, us[1].ID, "demo", []byte("x"))
	s.PutIntegration(ctx, us[1].ID, "other", []byte("x"))
	ids, err := s.ListUsersWithIntegration(ctx, "demo")
	if err != nil || len(ids) != 2 || ids[0] != us[1].ID || ids[1] != us[2].ID {
		t.Fatalf("ids: %v %v", ids, err)
	}
}

func TestRunningJobAndStats(t *testing.T) {
	s, ctx, us := jobFixture(t, 1)
	enqueue(t, s, ctx, us[1].ID, testKind, false, "a")
	enqueue(t, s, ctx, us[1].ID, testKind, false, "b")
	enqueue(t, s, ctx, us[1].ID, "other.job", false, "c")
	j, _ := lease(t, s, ctx, time.Now(), "w", testKind)
	if got, err := s.RunningJob(ctx, j.PublicID, "tok-w"); err != nil || got.PublicID != j.PublicID || got.Payload == "" {
		t.Fatalf("running job: %+v %v", got, err)
	}
	for _, tok := range []string{"tok-x", ""} {
		if _, err := s.RunningJob(ctx, j.PublicID, tok); !errors.Is(err, store.ErrLeaseLost) {
			t.Fatalf("token %q: %v", tok, err)
		}
	}
	if _, err := s.RunningJob(ctx, "nope", "tok-w"); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("unknown job: %v", err)
	}
	st, err := s.JobStats(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(st) != 2 {
		t.Fatalf("stats: %+v %v", st, err)
	}
	byKind := map[string]models.JobKindStats{}
	for _, x := range st {
		byKind[x.Kind] = x
	}
	if d := byKind[testKind]; d.Queued != 1 || d.Running != 1 || d.Failed != 0 {
		t.Fatalf("demo stats: %+v", d)
	}
	s.CompleteJob(ctx, j.PublicID, "tok-w", models.Completion{Outcome: models.OutcomeFail, Error: "x"}, time.Now())
	st, _ = s.JobStats(ctx, time.Now().Add(-time.Hour))
	for _, x := range st {
		if x.Kind == testKind && (x.Failed != 1 || x.Running != 0) {
			t.Fatalf("after failure: %+v", x)
		}
	}
}

func TestAnIdleWorkerIsNotWrittenOnEveryPoll(t *testing.T) {
	s, ctx, _ := jobFixture(t, 1)
	t0 := time.Now().UTC().Truncate(time.Second)
	lastSeen := func() time.Time {
		t.Helper()
		ws, err := s.ListWorkers(ctx, t0.Add(-time.Hour))
		if err != nil || len(ws) != 1 {
			t.Fatalf("workers: %+v %v", ws, err)
		}
		return ws[0].LastSeen
	}
	touch := func(at time.Time, kinds []string, version string) {
		t.Helper()
		if err := s.TouchWorker(ctx, "w", kinds, version, at); err != nil {
			t.Fatal(err)
		}
	}

	touch(t0, []string{"a.job"}, "1.0")
	touch(t0.Add(time.Second), []string{"a.job"}, "1.0")
	touch(t0.Add(5*time.Second), []string{"a.job"}, "1.0")
	if got := lastSeen(); !got.Equal(t0) {
		t.Fatalf("a poll within %v wrote the row again: last seen %v, want %v", workerTouchEvery, got, t0)
	}
	touch(t0.Add(6*time.Second), []string{"a.job", "b.job"}, "1.0")
	if got := lastSeen(); !got.Equal(t0.Add(6 * time.Second)) {
		t.Fatalf("new kinds were not written: last seen %v", got)
	}
	touch(t0.Add(7*time.Second), []string{"a.job", "b.job"}, "1.1")
	if got := lastSeen(); !got.Equal(t0.Add(7 * time.Second)) {
		t.Fatalf("a new version was not written: last seen %v", got)
	}
	late := t0.Add(7*time.Second + workerTouchEvery + time.Second)
	touch(late, []string{"a.job", "b.job"}, "1.1")
	if got := lastSeen(); !got.Equal(late) {
		t.Fatalf("a stale row was not refreshed: last seen %v, want %v", got, late)
	}
}
