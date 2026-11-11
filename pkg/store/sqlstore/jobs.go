package sqlstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// The queue of work for the workers. Every state change is one conditional UPDATE whose WHERE
// clause names the state (and, for a worker's calls, the lease token) it expects, and the number of
// rows it changed says who won. That works the same on SQLite and PostgreSQL, and two backends
// racing for a job cannot both get it.

type jobRow struct {
	bun.BaseModel `bun:"table:worker_jobs"`

	ID             int64  `bun:"id,pk,autoincrement"`
	PublicID       string `bun:"public_id"`
	UserID         int64  `bun:"user_id"`
	Kind           string `bun:"kind"`
	Origin         string `bun:"origin"`
	Interactive    bool   `bun:"interactive"`
	DedupeKey      string `bun:"dedupe_key"`
	State          string `bun:"state"`
	Payload        string `bun:"payload"`
	Progress       string `bun:"progress"`
	Result         string `bun:"result"`
	Error          string `bun:"error"`
	Attempts       int    `bun:"attempts"`
	MaxAttempts    int    `bun:"max_attempts"`
	TimeoutSeconds int    `bun:"timeout_seconds"`

	CreatedAt  time.Time  `bun:"created_at"`
	RunAfter   time.Time  `bun:"run_after"`
	ExpiresAt  *time.Time `bun:"expires_at"`
	StartedAt  *time.Time `bun:"started_at"`
	FinishedAt *time.Time `bun:"finished_at"`
	UpdatedAt  time.Time  `bun:"updated_at"`

	LeaseOwner      string     `bun:"lease_owner"`
	LeaseTokenHash  string     `bun:"lease_token_hash"`
	LeaseUntil      *time.Time `bun:"lease_until"`
	DeadlineAt      *time.Time `bun:"deadline_at"`
	CancelRequested bool       `bun:"cancel_requested"`
}

type workerRow struct {
	bun.BaseModel `bun:"table:worker_instances"`

	ID       string    `bun:"id,pk"`
	Kinds    string    `bun:"kinds"`
	Version  string    `bun:"version"`
	LastSeen time.Time `bun:"last_seen"`
}

type scheduleRow struct {
	bun.BaseModel `bun:"table:worker_schedules"`

	UserID              int64      `bun:"user_id,pk"`
	Kind                string     `bun:"kind,pk"`
	LastEnqueuedAt      *time.Time `bun:"last_enqueued_at"`
	ConsecutiveFailures int        `bun:"consecutive_failures"`
	PausedReason        string     `bun:"paused_reason"`
}

// The ciphertext of a job is bound to the job, so it cannot be moved to another.
func jobPayloadContext(publicID string) string { return "job:" + publicID + ":payload" }
func jobResultContext(publicID string) string  { return "job:" + publicID + ":result" }

func utcPtr(t time.Time) *time.Time {
	t = t.UTC()
	return &t
}

func inUTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return utcPtr(*t)
}

// toModel converts a row; withSecrets also decrypts the payload and the result.
func (s *Store) jobModel(r jobRow, withSecrets bool) (models.Job, error) {
	j := models.Job{
		ID: r.ID, PublicID: r.PublicID, UserID: r.UserID, Kind: r.Kind, Origin: r.Origin,
		Interactive: r.Interactive, DedupeKey: r.DedupeKey, State: r.State,
		Progress: r.Progress, Error: r.Error, Attempts: r.Attempts, MaxAttempts: r.MaxAttempts,
		Timeout:   time.Duration(r.TimeoutSeconds) * time.Second,
		CreatedAt: r.CreatedAt.UTC(), RunAfter: r.RunAfter.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
		ExpiresAt: inUTC(r.ExpiresAt), StartedAt: inUTC(r.StartedAt), FinishedAt: inUTC(r.FinishedAt),
		LeaseOwner: r.LeaseOwner, LeaseUntil: inUTC(r.LeaseUntil), DeadlineAt: inUTC(r.DeadlineAt),
		CancelRequested: r.CancelRequested,
	}
	if !withSecrets {
		return j, nil
	}
	var err error
	if j.Payload, err = s.decrypt(r.Payload, jobPayloadContext(r.PublicID)); err != nil {
		return j, fmt.Errorf("payload of job %s: %w", r.PublicID, err)
	}
	if j.Result, err = s.decrypt(r.Result, jobResultContext(r.PublicID)); err != nil {
		return j, fmt.Errorf("result of job %s: %w", r.PublicID, err)
	}
	return j, nil
}

func (s *Store) loadJob(ctx context.Context, where string, args ...any) (jobRow, error) {
	var row jobRow
	if err := s.db.NewSelect().Model(&row).Where(where, args...).Limit(1).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return row, store.ErrNotFound
		}
		return row, err
	}
	return row, nil
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails: since Go 1.24 a broken random source ends the program
	return hex.EncodeToString(b)
}

// EnqueueJob queues a job. When the job has a DedupeKey and one with that key is already queued or
// running, nothing is added and that job is returned with created false.
func (s *Store) EnqueueJob(ctx context.Context, n models.NewJob) (models.Job, bool, error) {
	if n.UserID <= 0 || n.Kind == "" || n.Origin == "" || n.MaxAttempts < 1 || n.Timeout < time.Second {
		return models.Job{}, false, fmt.Errorf("%w: incomplete job", store.ErrInvalid)
	}
	id := randomID()
	payload := s.encrypt(n.Payload, jobPayloadContext(id))
	at := now()
	runAfter := at
	if !n.RunAfter.IsZero() {
		runAfter = n.RunAfter.UTC()
	}
	row := jobRow{
		PublicID: id, UserID: n.UserID, Kind: n.Kind, Origin: n.Origin, Interactive: n.Interactive,
		DedupeKey: n.DedupeKey, State: models.JobQueued, Payload: payload,
		MaxAttempts: n.MaxAttempts, TimeoutSeconds: int(n.Timeout / time.Second),
		CreatedAt: at, RunAfter: runAfter, ExpiresAt: inUTC(n.ExpiresAt), UpdatedAt: at,
	}
	for try := 0; try < 3; try++ {
		res, err := s.db.NewInsert().Model(&row).On("CONFLICT DO NOTHING").Returning("NULL").Exec(ctx)
		if isFK(err) {
			return models.Job{}, false, store.ErrNotFound
		}
		if err != nil {
			return models.Job{}, false, err
		}
		if c, _ := res.RowsAffected(); c > 0 {
			stored, err := s.loadJob(ctx, "public_id = ?", id)
			if err != nil {
				return models.Job{}, false, err
			}
			j, err := s.jobModel(stored, true)
			return j, true, err
		}
		if n.DedupeKey == "" {
			return models.Job{}, false, errors.New("job was not stored")
		}
		// The key is taken. The holder may have finished meanwhile, in which case try again.
		held, err := s.loadJob(ctx, "dedupe_key = ? AND state IN (?)", n.DedupeKey, bun.In([]string{models.JobQueued, models.JobRunning}))
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return models.Job{}, false, err
		}
		j, err := s.jobModel(held, true)
		return j, false, err
	}
	return models.Job{}, false, errors.New("could not queue the job: the key kept changing hands")
}

func (s *Store) GetJob(ctx context.Context, publicID string) (models.Job, error) {
	row, err := s.loadJob(ctx, "public_id = ?", publicID)
	if err != nil {
		return models.Job{}, err
	}
	return s.jobModel(row, true)
}

// ListJobs returns jobs newest first, without their payload and result.
func (s *Store) ListJobs(ctx context.Context, f models.JobFilter) ([]models.Job, error) {
	var rows []jobRow
	q := s.db.NewSelect().Model(&rows).ExcludeColumn("payload", "result")
	if f.UserID != 0 {
		q = q.Where("user_id = ?", f.UserID)
	}
	if len(f.Kinds) > 0 {
		q = q.Where("kind IN (?)", bun.In(f.Kinds))
	}
	if len(f.States) > 0 {
		q = q.Where("state IN (?)", bun.In(f.States))
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if err := q.OrderExpr("id DESC").Limit(int64(limit)).Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]models.Job, 0, len(rows))
	for _, r := range rows {
		j, _ := s.jobModel(r, false)
		out = append(out, j)
	}
	return out, nil
}

// LastFinishedStates returns the states of the latest n jobs of a kind that succeeded or failed
// since a time, newest first. The backend watches it to notice that a provider keeps failing.
func (s *Store) LastFinishedStates(ctx context.Context, kind string, n int, since time.Time) ([]string, error) {
	var states []string
	err := s.db.NewSelect().TableExpr("worker_jobs").Column("state").
		Where("kind = ? AND state IN (?) AND finished_at >= ?", kind, bun.In([]string{models.JobSucceeded, models.JobFailed}), since.UTC()).
		OrderExpr("finished_at DESC, id DESC").Limit(int64(n)).Scan(ctx, &states)
	return states, err
}

// LeaseJob hands the best waiting job to a worker, or reports that there is none. The order is:
// administrators' jobs first, then the user's priority (higher first), then jobs somebody waits for,
// then the oldest. Role and priority are read now, so a change applies to jobs already queued.
// MaxRunning is checked when leasing, so it can be exceeded by the number of leases that happen at
// the very same moment; the lease itself is exact.
func (s *Store) LeaseJob(ctx context.Context, req models.LeaseRequest, at time.Time) (models.Job, bool, error) {
	at = at.UTC()
	paused := map[string]bool{}
	for _, k := range req.Paused {
		paused[k] = true
	}
	var running []struct {
		Kind string
		N    int
	}
	if err := s.db.NewSelect().TableExpr("worker_jobs").ColumnExpr("kind, COUNT(*) AS n").
		Where("state = ?", models.JobRunning).GroupExpr("kind").Scan(ctx, &running); err != nil {
		return models.Job{}, false, err
	}
	active := map[string]int{}
	for _, r := range running {
		active[r.Kind] = r.N
	}
	var kinds []string
	for _, k := range req.Kinds {
		if paused[k] {
			continue
		}
		if max, ok := req.MaxRunning[k]; ok && max > 0 && active[k] >= max {
			continue
		}
		kinds = append(kinds, k)
	}
	if len(kinds) == 0 {
		return models.Job{}, false, nil
	}

	for try := 0; try < 5; try++ {
		var cands []struct {
			ID      int64
			Timeout int `bun:"timeout_seconds"`
		}
		err := s.db.NewSelect().TableExpr("worker_jobs AS j").ColumnExpr("j.id, j.timeout_seconds").
			Join("JOIN users AS u ON u.id = j.user_id").
			Where("j.state = ?", models.JobQueued).Where("j.run_after <= ?", at).
			Where("j.kind IN (?)", bun.In(kinds)).
			Where("(j.expires_at IS NULL OR j.expires_at > ?)", at).
			OrderExpr("CASE WHEN u.role = ? THEN 1 ELSE 0 END DESC, u.job_priority DESC, j.interactive DESC, j.run_after, j.id", models.RoleAdmin).
			Limit(10).Scan(ctx, &cands)
		if err != nil {
			return models.Job{}, false, err
		}
		if len(cands) == 0 {
			return models.Job{}, false, nil
		}
		for _, c := range cands {
			res, err := s.db.NewUpdate().Model((*jobRow)(nil)).
				Set("state = ?", models.JobRunning).Set("attempts = attempts + 1").Set("error = ''").
				Set("lease_owner = ?", req.Owner).Set("lease_token_hash = ?", req.TokenHash).
				Set("lease_until = ?", at.Add(req.LeaseFor)).
				Set("deadline_at = ?", at.Add(time.Duration(c.Timeout)*time.Second)).
				Set("started_at = COALESCE(started_at, ?)", at).Set("updated_at = ?", at).
				Where("id = ? AND state = ?", c.ID, models.JobQueued).Exec(ctx)
			if err != nil {
				return models.Job{}, false, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue // another leaser was faster
			}
			row, err := s.loadJob(ctx, "id = ?", c.ID)
			if err != nil {
				return models.Job{}, false, err
			}
			j, err := s.jobModel(row, true)
			return j, err == nil, err
		}
		// Every candidate was taken meanwhile: look again.
	}
	return models.Job{}, false, nil
}

// HeartbeatJob extends the lease of a running job and stores its progress (nil leaves it). It
// fails with ErrLeaseLost when the token is not the leaseholder's, or the lease or the job's hard
// deadline has passed: the job then belongs to the reaper. cancel tells the worker to stop.
func (s *Store) HeartbeatJob(ctx context.Context, publicID, tokenHash string, extend time.Duration, progress *string, at time.Time) (cancel bool, err error) {
	if tokenHash == "" {
		return false, store.ErrLeaseLost
	}
	at = at.UTC()
	q := s.db.NewUpdate().Model((*jobRow)(nil)).Set("lease_until = ?", at.Add(extend)).Set("updated_at = ?", at)
	if progress != nil {
		q = q.Set("progress = ?", *progress)
	}
	res, err := q.Where("public_id = ? AND state = ? AND lease_token_hash = ?", publicID, models.JobRunning, tokenHash).
		Where("lease_until >= ? AND (deadline_at IS NULL OR deadline_at >= ?)", at, at).Exec(ctx)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, store.ErrLeaseLost
	}
	row, err := s.loadJob(ctx, "public_id = ?", publicID)
	if err != nil {
		return false, err
	}
	return row.CancelRequested, nil
}

// finishUpdate ends or requeues the attempt of row, guarded by the state and token it was read with.
// It returns whether this call changed the job.
func (s *Store) finishUpdate(ctx context.Context, row jobRow, state string, at time.Time, set func(*bun.UpdateQuery) *bun.UpdateQuery) (bool, error) {
	q := s.db.NewUpdate().Model((*jobRow)(nil)).Set("state = ?", state).Set("updated_at = ?", at).
		Set("lease_owner = ''").Set("lease_token_hash = ''").Set("lease_until = NULL").Set("deadline_at = NULL")
	if !models.JobActive(state) {
		q = q.Set("finished_at = ?", at).Set("payload = ''") // the payload can hold a credential
	}
	res, err := set(q).Where("id = ? AND state = ? AND lease_token_hash = ?", row.ID, models.JobRunning, row.LeaseTokenHash).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// afterAttempt decides what becomes of a job whose attempt ended without success: it is requeued
// for retryAt while attempts are left, and ended otherwise.
func afterAttempt(row jobRow, reason string, retryAt time.Time) (state, message string) {
	switch {
	case row.CancelRequested:
		return models.JobCanceled, "canceled"
	case row.Attempts >= row.MaxAttempts:
		return models.JobFailed, fmt.Sprintf("gave up after %d attempts: %s", row.Attempts, reason)
	default:
		return models.JobQueued, reason
	}
}

// CompleteJob ends the attempt as the worker reports it. An OutcomeRetry requeues the job for
// c.RetryAt unless it ran out of attempts (it then fails for good) or was canceled. It fails with
// ErrLeaseLost when the job is no longer running under this token, which also makes a second
// completion of the same attempt harmless.
func (s *Store) CompleteJob(ctx context.Context, publicID, tokenHash string, c models.Completion, at time.Time) (models.Job, error) {
	at = at.UTC()
	row, err := s.loadJob(ctx, "public_id = ?", publicID)
	if err != nil {
		return models.Job{}, err
	}
	if row.State != models.JobRunning || row.LeaseTokenHash != tokenHash || tokenHash == "" {
		return models.Job{}, store.ErrLeaseLost
	}
	var ok bool
	switch c.Outcome {
	case models.OutcomeOK:
		result := s.encrypt(c.Result, jobResultContext(row.PublicID))
		var err error
		ok, err = s.finishUpdate(ctx, row, models.JobSucceeded, at, func(q *bun.UpdateQuery) *bun.UpdateQuery {
			return q.Set("result = ?", result).Set("error = ''")
		})
		if err != nil {
			return models.Job{}, err
		}
	case models.OutcomeFail:
		ok, err = s.finishUpdate(ctx, row, models.JobFailed, at, func(q *bun.UpdateQuery) *bun.UpdateQuery {
			return q.Set("error = ?", c.Error)
		})
		if err != nil {
			return models.Job{}, err
		}
	case models.OutcomeRetry:
		state, msg := afterAttempt(row, c.Error, c.RetryAt)
		if state == models.JobQueued && c.Error == "" {
			msg = "temporary failure"
		}
		retryAt := c.RetryAt.UTC()
		if c.RetryAt.IsZero() {
			retryAt = at
		}
		ok, err = s.finishUpdate(ctx, row, state, at, func(q *bun.UpdateQuery) *bun.UpdateQuery {
			return q.Set("error = ?", msg).Set("run_after = ?", retryAt)
		})
		if err != nil {
			return models.Job{}, err
		}
	default:
		return models.Job{}, fmt.Errorf("%w: unknown outcome %q", store.ErrInvalid, c.Outcome)
	}
	if !ok {
		return models.Job{}, store.ErrLeaseLost
	}
	return s.GetJob(ctx, publicID)
}

// ReleaseJob gives a job back without counting the attempt, for a worker that is shutting down.
func (s *Store) ReleaseJob(ctx context.Context, publicID, tokenHash string, at time.Time) error {
	at = at.UTC()
	row, err := s.loadJob(ctx, "public_id = ?", publicID)
	if err != nil {
		return err
	}
	if row.State != models.JobRunning || row.LeaseTokenHash != tokenHash || tokenHash == "" {
		return store.ErrLeaseLost
	}
	state := models.JobQueued
	if row.CancelRequested {
		state = models.JobCanceled
	}
	ok, err := s.finishUpdate(ctx, row, state, at, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END").Set("run_after = ?", at)
	})
	if err != nil {
		return err
	}
	if !ok {
		return store.ErrLeaseLost
	}
	return nil
}

// CancelJob cancels a queued job at once and asks the worker of a running one to stop. userID
// limits it to that user's jobs (0: any). A job that already ended is returned as it is.
func (s *Store) CancelJob(ctx context.Context, publicID string, userID int64, at time.Time) (models.Job, error) {
	at = at.UTC()
	row, err := s.loadJob(ctx, "public_id = ?", publicID)
	if err != nil || (userID != 0 && row.UserID != userID) {
		return models.Job{}, store.ErrNotFound
	}
	if err := s.cancelRows(ctx, at, "id = ?", row.ID); err != nil {
		return models.Job{}, err
	}
	return s.GetJob(ctx, publicID)
}

// CancelJobs cancels the active jobs of a user and kinds (for example when a login is removed).
func (s *Store) CancelJobs(ctx context.Context, userID int64, kinds []string, at time.Time) error {
	if len(kinds) == 0 {
		return nil
	}
	return s.cancelRows(ctx, at.UTC(), "user_id = ? AND kind IN (?)", userID, bun.In(kinds))
}

func (s *Store) cancelRows(ctx context.Context, at time.Time, where string, args ...any) error {
	if _, err := s.db.NewUpdate().Model((*jobRow)(nil)).
		Set("state = ?", models.JobCanceled).Set("error = 'canceled'").Set("payload = ''").Set("finished_at = ?", at).Set("updated_at = ?", at).
		Where(where, args...).Where("state = ?", models.JobQueued).Exec(ctx); err != nil {
		return err
	}
	_, err := s.db.NewUpdate().Model((*jobRow)(nil)).Set("cancel_requested = ?", true).Set("updated_at = ?", at).
		Where(where, args...).Where("state = ?", models.JobRunning).Exec(ctx)
	return err
}

// ReapExpiredLeases deals with running jobs whose lease or hard deadline has passed, because the
// worker died, hung or lost the network: each such attempt counts as failed, and the job is requeued
// after backoff(job) or, when it has no attempts left, ended. It returns the jobs it changed.
func (s *Store) ReapExpiredLeases(ctx context.Context, at time.Time, backoff func(models.Job) time.Duration) ([]models.Job, error) {
	at = at.UTC()
	var rows []jobRow
	if err := s.db.NewSelect().Model(&rows).ExcludeColumn("payload", "result").
		Where("state = ?", models.JobRunning).Where("(lease_until < ? OR deadline_at < ?)", at, at).
		OrderExpr("id").Limit(200).Scan(ctx); err != nil {
		return nil, err
	}
	var changed []models.Job
	for _, row := range rows {
		reason := "the worker stopped responding"
		if row.DeadlineAt != nil && row.DeadlineAt.Before(at) {
			reason = "the attempt took longer than its time limit"
		}
		state, msg := afterAttempt(row, reason, at)
		j, _ := s.jobModel(row, false)
		retryAt := at
		if state == models.JobQueued && backoff != nil {
			retryAt = at.Add(backoff(j))
		}
		ok, err := s.finishUpdate(ctx, row, state, at, func(q *bun.UpdateQuery) *bun.UpdateQuery {
			return q.Set("error = ?", msg).Set("run_after = ?", retryAt)
		})
		if err != nil {
			return changed, err
		}
		if !ok {
			continue // the worker reported in time after all
		}
		j.State, j.Error = state, msg
		changed = append(changed, j)
	}
	return changed, nil
}

// ExpireQueuedJobs ends queued jobs that were not started before their expiry.
func (s *Store) ExpireQueuedJobs(ctx context.Context, at time.Time) (int64, error) {
	at = at.UTC()
	res, err := s.db.NewUpdate().Model((*jobRow)(nil)).
		Set("state = ?", models.JobExpired).Set("error = 'not started in time'").Set("payload = ''").
		Set("finished_at = ?", at).Set("updated_at = ?", at).
		Where("state = ? AND expires_at IS NOT NULL AND expires_at < ?", models.JobQueued, at).Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeJobs deletes jobs that ended before the cutoff.
func (s *Store) PurgeJobs(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.NewDelete().Model((*jobRow)(nil)).
		Where("state NOT IN (?) AND finished_at < ?", bun.In([]string{models.JobQueued, models.JobRunning}), before.UTC()).Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- workers ----

// TouchWorker records that a worker process asked for work.
func (s *Store) TouchWorker(ctx context.Context, id string, kinds []string, version string, at time.Time) error {
	row := workerRow{ID: id, Kinds: strings.Join(kinds, ","), Version: version, LastSeen: at.UTC()}
	// A worker asks for work every second or so. A known worker is only written again when its row is
	// older than workerTouchEvery (a worker counts as alive for far longer) or says something new, so
	// an idle worker does not write to the database all day.
	_, err := s.db.NewInsert().Model(&row).On("CONFLICT (id) DO UPDATE").
		Set("kinds = EXCLUDED.kinds").Set("version = EXCLUDED.version").Set("last_seen = EXCLUDED.last_seen").
		Where("worker_row.last_seen < ? OR worker_row.kinds <> EXCLUDED.kinds OR worker_row.version <> EXCLUDED.version", at.UTC().Add(-workerTouchEvery)).
		Exec(ctx)
	return err
}

// workerTouchEvery is how often the row of a worker that has nothing new to say is written.
const workerTouchEvery = 20 * time.Second

// ListWorkers returns the workers seen since a time, newest first.
func (s *Store) ListWorkers(ctx context.Context, since time.Time) ([]models.WorkerInstance, error) {
	var rows []workerRow
	if err := s.db.NewSelect().Model(&rows).Where("last_seen >= ?", since.UTC()).OrderExpr("last_seen DESC, id").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]models.WorkerInstance, 0, len(rows))
	for _, r := range rows {
		w := models.WorkerInstance{ID: r.ID, Version: r.Version, LastSeen: r.LastSeen.UTC()}
		if r.Kinds != "" {
			w.Kinds = strings.Split(r.Kinds, ",")
		}
		out = append(out, w)
	}
	return out, nil
}

// PurgeWorkers forgets workers not seen since the cutoff.
func (s *Store) PurgeWorkers(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.NewDelete().Model((*workerRow)(nil)).Where("last_seen < ?", before.UTC()).Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- schedules and priority ----

func (s *Store) ListJobSchedules(ctx context.Context, kind string) ([]models.JobSchedule, error) {
	var rows []scheduleRow
	if err := s.db.NewSelect().Model(&rows).Where("kind = ?", kind).OrderExpr("user_id").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]models.JobSchedule, 0, len(rows))
	for _, r := range rows {
		out = append(out, models.JobSchedule{UserID: r.UserID, Kind: r.Kind, LastEnqueuedAt: inUTC(r.LastEnqueuedAt),
			ConsecutiveFailures: r.ConsecutiveFailures, PausedReason: r.PausedReason})
	}
	return out, nil
}

// GetJobSchedule returns what the scheduler remembers about a user and kind, or ErrNotFound.
func (s *Store) GetJobSchedule(ctx context.Context, userID int64, kind string) (models.JobSchedule, error) {
	var r scheduleRow
	if err := s.db.NewSelect().Model(&r).Where("user_id = ? AND kind = ?", userID, kind).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return models.JobSchedule{}, store.ErrNotFound
		}
		return models.JobSchedule{}, err
	}
	return models.JobSchedule{UserID: r.UserID, Kind: r.Kind, LastEnqueuedAt: inUTC(r.LastEnqueuedAt),
		ConsecutiveFailures: r.ConsecutiveFailures, PausedReason: r.PausedReason}, nil
}

func (s *Store) PutJobSchedule(ctx context.Context, j models.JobSchedule) error {
	row := scheduleRow{UserID: j.UserID, Kind: j.Kind, LastEnqueuedAt: inUTC(j.LastEnqueuedAt),
		ConsecutiveFailures: j.ConsecutiveFailures, PausedReason: j.PausedReason}
	_, err := s.db.NewInsert().Model(&row).On("CONFLICT (user_id, kind) DO UPDATE").
		Set("last_enqueued_at = EXCLUDED.last_enqueued_at").Set("consecutive_failures = EXCLUDED.consecutive_failures").
		Set("paused_reason = EXCLUDED.paused_reason").Exec(ctx)
	if isFK(err) {
		return store.ErrNotFound
	}
	return err
}

// DeleteJobSchedule forgets a schedule, which also resumes one that was paused.
func (s *Store) DeleteJobSchedule(ctx context.Context, userID int64, kind string) error {
	_, err := s.db.NewDelete().Model((*scheduleRow)(nil)).Where("user_id = ? AND kind = ?", userID, kind).Exec(ctx)
	return err
}

// ListUsersWithIntegration returns the ids of users who have stored data for a provider.
func (s *Store) ListUsersWithIntegration(ctx context.Context, provider string) ([]int64, error) {
	var ids []int64
	err := s.db.NewSelect().TableExpr("user_integrations").Column("user_id").Where("provider = ?", provider).OrderExpr("user_id").Scan(ctx, &ids)
	return ids, err
}

// SetUserJobPriority sets the priority (1-100) that orders a user's jobs among other users'.
func (s *Store) SetUserJobPriority(ctx context.Context, userID int64, priority int) error {
	if priority < models.MinJobPriority || priority > models.MaxJobPriority {
		return fmt.Errorf("%w: the priority is a number from %d to %d", store.ErrInvalid, models.MinJobPriority, models.MaxJobPriority)
	}
	res, err := s.db.NewUpdate().Model((*userRow)(nil)).Set("job_priority = ?", priority).Where("id = ?", userID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// RunningJob returns a job that is running under this lease token, or ErrLeaseLost. It changes
// nothing; the backend uses it to find out whose call it is serving.
func (s *Store) RunningJob(ctx context.Context, publicID, tokenHash string) (models.Job, error) {
	row, err := s.loadJob(ctx, "public_id = ?", publicID)
	if errors.Is(err, store.ErrNotFound) {
		return models.Job{}, store.ErrLeaseLost
	}
	if err != nil {
		return models.Job{}, err
	}
	if row.State != models.JobRunning || row.LeaseTokenHash != tokenHash || tokenHash == "" {
		return models.Job{}, store.ErrLeaseLost
	}
	return s.jobModel(row, true)
}

// JobStats counts the jobs of every kind that are waiting or running, and those that failed since a time.
func (s *Store) JobStats(ctx context.Context, failedSince time.Time) ([]models.JobKindStats, error) {
	var rows []struct {
		Kind    string
		Queued  int
		Running int
		Failed  int
	}
	err := s.db.NewSelect().TableExpr("worker_jobs").
		ColumnExpr("kind").
		ColumnExpr("COUNT(CASE WHEN state = ? THEN 1 END) AS queued", models.JobQueued).
		ColumnExpr("COUNT(CASE WHEN state = ? THEN 1 END) AS running", models.JobRunning).
		ColumnExpr("COUNT(CASE WHEN state = ? AND finished_at >= ? THEN 1 END) AS failed", models.JobFailed, failedSince.UTC()).
		GroupExpr("kind").OrderExpr("kind").Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]models.JobKindStats, 0, len(rows))
	for _, r := range rows {
		out = append(out, models.JobKindStats{Kind: r.Kind, Queued: r.Queued, Running: r.Running, Failed: r.Failed})
	}
	return out, nil
}
