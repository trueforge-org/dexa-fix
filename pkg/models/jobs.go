package models

import "time"

// The states of a worker job. A job is queued until a worker leases it, then running; it ends as
// succeeded, failed (given up, kept for the administrator), canceled or expired (never started in time).
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCanceled  = "canceled"
	JobExpired   = "expired"
)

// JobActive reports whether a job in this state can still run.
func JobActive(state string) bool { return state == JobQueued || state == JobRunning }

// Where a job came from.
const (
	JobOriginUser     = "user"     // a button in the pages
	JobOriginAPI      = "api"      // the public API
	JobOriginSchedule = "schedule" // the scheduler
	JobOriginAdmin    = "admin"    // an administrator
)

// How a worker ended an attempt.
const (
	OutcomeOK    = "ok"    // done
	OutcomeRetry = "retry" // a temporary problem: try again later, while attempts are left
	OutcomeFail  = "fail"  // a permanent problem: do not try again
)

// Job priorities are 1 (lowest) to 100 (highest); administrators' jobs always go first.
const (
	MinJobPriority     = 1
	MaxJobPriority     = 100
	DefaultJobPriority = 50
)

// Job is one unit of work for a worker, such as importing a user's Withings measurements. The
// backend owns the queue; a worker only ever sees a job inside a lease.
type Job struct {
	ID       int64
	PublicID string // the identifier used outside the database
	UserID   int64
	Kind     string // "<provider>.<action>", such as "withings.import"
	Origin   string
	// Interactive jobs are waited for by a person, so they go before background ones of the same priority.
	Interactive bool
	// DedupeKey allows one active (queued or running) job per key; empty means no limit.
	DedupeKey string
	State     string
	// Payload holds the job's parameters, and Result what it produced. The store encrypts both.
	// Payload is cleared when the job ends, because it can hold a credential.
	Payload     string
	Progress    string
	Result      string
	Error       string
	Attempts    int
	MaxAttempts int
	Timeout     time.Duration // the hard limit of one attempt

	CreatedAt  time.Time
	RunAfter   time.Time
	ExpiresAt  *time.Time // a queued job still waiting after this is expired instead of run late
	StartedAt  *time.Time
	FinishedAt *time.Time
	UpdatedAt  time.Time

	LeaseOwner      string
	LeaseUntil      *time.Time
	DeadlineAt      *time.Time
	CancelRequested bool
}

// NewJob describes a job to queue.
type NewJob struct {
	UserID      int64
	Kind        string
	Origin      string
	Interactive bool
	DedupeKey   string
	Payload     string
	MaxAttempts int
	Timeout     time.Duration
	RunAfter    time.Time  // zero means now
	ExpiresAt   *time.Time // nil means never
}

// JobFilter selects jobs for a listing, newest first.
type JobFilter struct {
	UserID int64    // 0: every user
	Kinds  []string // empty: every kind
	States []string // empty: every state
	Limit  int      // 0: 100
}

// Completion is how a worker ends an attempt.
type Completion struct {
	Outcome string // OutcomeOK, OutcomeRetry or OutcomeFail
	Result  string
	Error   string
	// RetryAt is when a retry may run; the backend computes it from its backoff settings.
	RetryAt time.Time
}

// LeaseRequest asks for the next job a worker may run.
type LeaseRequest struct {
	Kinds     []string
	Owner     string        // the worker's name, for the administrator
	TokenHash string        // hash of the secret that proves later calls come from the leaseholder
	LeaseFor  time.Duration // how long the lease lasts without a heartbeat
	// MaxRunning limits how many jobs of a kind run at once, in all workers together; a kind that is
	// missing is not limited. Paused kinds are never leased.
	MaxRunning map[string]int
	Paused     []string
}

// WorkerInstance is a worker process that recently asked for work.
type WorkerInstance struct {
	ID       string
	Kinds    []string
	Version  string
	LastSeen time.Time
}

// JobSchedule is the scheduler's memory about one user and kind.
type JobSchedule struct {
	UserID              int64
	Kind                string
	LastEnqueuedAt      *time.Time
	ConsecutiveFailures int
	// PausedReason is set when the scheduler stopped enqueuing for the user after repeated failures.
	PausedReason string
}

// JobKindStats counts the jobs of one kind.
type JobKindStats struct {
	Kind    string
	Queued  int
	Running int
	Failed  int // that ended as failed in the period asked for
}
