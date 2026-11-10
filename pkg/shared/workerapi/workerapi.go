// Package workerapi is the contract between the backend and the workers: processes that talk to
// outside services (Withings, Strength Level, ...) for the backend and have no database. A worker
// asks the backend for a job, works on it while renewing its lease, and reports the result; the
// backend owns the queue, the credentials and every decision about what is stored.
//
// The wire protocol is /internal/v1/worker, authenticated with a worker secret that opens nothing
// else (and the service secret of the web frontend does not open it). Like the rest of the internal
// API it is not a public interface: deploy the workers from the same release as the backend.
//
// A job's kind is "<provider>.<action>", for example "withings.import". Nothing here names a
// provider: each provider package registers handlers for its kinds, and the backend has a
// descriptor for each provider (see pkg/backend/main). The package must not import the store, the
// backend or the frontend.
package workerapi

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// Prefix of the worker endpoints, below the internal API's.
const Prefix = "/internal/v1/worker"

// Header names.
const (
	// HeaderWorker carries the worker secret.
	HeaderWorker = "X-Dexafix-Worker"
	// HeaderLease carries the lease token of the job a call is about: the secret only the worker
	// that leased the job knows. A call with another token fails with ErrLeaseLost.
	HeaderLease = "X-Dexafix-Lease"
)

// ErrLeaseLost: the worker no longer holds the job (its lease ran out and the job was handed on,
// canceled or finished). The worker must stop working on it and report nothing more.
var ErrLeaseLost = errors.New("the job is no longer leased to this worker")

// RegisterRequest introduces a worker process and the kinds of job it can run.
type RegisterRequest struct {
	ID      string   `json:"id"` // unique per process, chosen by the worker
	Version string   `json:"version"`
	Kinds   []string `json:"kinds"`
}

// Config is what the administrator set for the workers. The backend serves it; Generation grows
// whenever any of it changes, so a worker re-reads it cheaply and applies it without a restart.
type Config struct {
	Generation int64 `json:"generation"`
	// LeaseSeconds is how long a lease lasts without a heartbeat; the worker renews at a third of it.
	LeaseSeconds int `json:"lease_seconds"`
	// Kinds holds the settings of each kind. A kind that is missing is disabled.
	Kinds map[string]KindConfig `json:"kinds"`
}

// KindConfig is the configuration of one kind of job.
type KindConfig struct {
	Enabled bool `json:"enabled"`
	// Options are provider specific values, such as a pause between requests. See each provider.
	Options map[string]string `json:"options,omitempty"`
}

// Option returns an option as a duration in milliseconds, or def when it is missing or invalid.
func (k KindConfig) OptionMillis(name string, def time.Duration) time.Duration {
	if n, err := strconv.Atoi(k.Options[name]); err == nil && n >= 0 {
		return time.Duration(n) * time.Millisecond
	}
	return def
}

// LeaseRequest asks for the next job of one of the kinds. The call waits up to WaitSeconds for one.
type LeaseRequest struct {
	Worker      string   `json:"worker"`
	Kinds       []string `json:"kinds"`
	WaitSeconds int      `json:"wait_seconds"`
}

// MaxWaitSeconds is the longest the backend holds a lease request open.
const MaxWaitSeconds = 20

// Job is a unit of work, as a worker sees it.
type Job struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	UserID      int64  `json:"user_id"`
	Attempt     int    `json:"attempt"`
	MaxAttempts int    `json:"max_attempts"`
	// Payload is what the handler of the kind needs: its parameters and the credentials the
	// backend decided to hand over for this one job. Never log it.
	Payload json.RawMessage `json:"payload"`
}

// Lease is a job together with the proof of holding it.
type Lease struct {
	Job Job `json:"job"`
	// Token goes into HeaderLease of every later call about the job.
	Token string `json:"token"`
	// TimeoutSeconds is the hard limit of this attempt. The backend ends the attempt itself when
	// it passes, whatever the worker does.
	TimeoutSeconds int `json:"timeout_seconds"`
	LeaseSeconds   int `json:"lease_seconds"`
}

// Heartbeat renews the lease and optionally reports progress (a JSON value for the person to see).
type Heartbeat struct {
	Progress json.RawMessage `json:"progress,omitempty"`
}

// HeartbeatAnswer tells the worker whether to go on.
type HeartbeatAnswer struct {
	// Cancel: somebody canceled the job. Stop, and report the attempt as retry.
	Cancel bool `json:"cancel"`
}

// Credentials hands the backend credentials the worker renewed during the job (a refreshed OAuth
// token pair, for instance) so they are stored at once, before they are used. Refresh tokens work
// once: losing the new one would disconnect the user.
type Credentials struct {
	Data json.RawMessage `json:"data"`
}

// Completion ends an attempt.
type Completion struct {
	// Outcome is "ok", "retry" (a temporary problem) or "fail" (a permanent one).
	Outcome string          `json:"outcome"`
	Result  json.RawMessage `json:"result,omitempty"`
	// Error is a short sentence for the administrator and, for some kinds, the person. It must not
	// contain a credential.
	Error string `json:"error,omitempty"`
	// RetryAfterSeconds is the provider's own request to wait (Retry-After); the backend never
	// retries sooner than its own backoff, and never later than its cap.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}
