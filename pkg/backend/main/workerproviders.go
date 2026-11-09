package mainbackend

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/workerapi"
)

// Work for the workers is described by providers. A provider (Withings, Strength Level, a future
// scale source) owns one or more kinds of job, "<provider>.<action>". The queue, the worker
// endpoints, the scheduler and the Admin page know nothing about any provider: they go through
// these descriptors, one file per provider (workerprovider_<name>.go). A provider's worker code
// lives in pkg/workers/<name>; the backend side, which holds the credentials and decides what is
// stored, is here.

// kindDefaults are what the Admin page starts from for a kind; an administrator can change each.
type kindDefaults struct {
	MaxRunning    int           // jobs of this kind running at once in all workers together; 0 is no limit
	MaxAttempts   int           // 1 to maxAttemptsLimit
	RetryBase     time.Duration // the wait after the first failed attempt; it doubles per attempt
	Timeout       time.Duration // hard limit of one attempt
	ScheduleHours int           // how often the scheduler enqueues the job for each eligible user; 0 is off
	PauseAfter    int           // scheduled failures in a row after which a user's schedule is paused; 0 never
	Breaker       int           // failures in a row, across all users, after which the kind pauses; 0 never
}

// optionField is a whole-number option a provider sends to its workers (a pause between requests).
type optionField struct {
	Name     string
	Label    string
	Default  int
	Min, Max int
	Millis   bool // shown in milliseconds
}

// scheduleTarget is a user the scheduler enqueues a job for, with the parameters of the job.
type scheduleTarget struct {
	UserID  int64
	Payload any
}

// kindSpec is the backend side of one kind of job.
type kindSpec struct {
	Kind     string
	Label    string
	Defaults kindDefaults
	Options  []optionField

	// ScheduleFor lists the users the scheduler enqueues the kind for. Nil: not schedulable.
	ScheduleFor func(ctx context.Context, s *Backend) ([]scheduleTarget, error)
	// Lease builds the payload a worker receives for a job: its parameters and the credentials it
	// needs, read and decrypted now so they leave the backend only inside a lease. An error made
	// with permanent() ends the job; any other releases it to try again.
	Lease func(ctx context.Context, s *Backend, j models.Job) (any, error)
	// Apply stores what the worker produced and returns the result to keep with the job. An error
	// that is errBadResult ends the job as failed; any other makes the worker report again.
	Apply func(ctx context.Context, s *Backend, j models.Job, result json.RawMessage) (stored string, err error)
	// Credentials stores credentials a worker renewed during the job.
	Credentials func(ctx context.Context, s *Backend, j models.Job, data json.RawMessage) error
}

// oauthSpec is how users connect to a provider by authorising access.
type oauthSpec struct {
	// Configured reports whether an administrator entered the application's credentials.
	Configured func(ctx context.Context, s *Backend) (bool, error)
	// AuthURL is where the user is sent to grant access; state is echoed back on the callback.
	AuthURL func(ctx context.Context, s *Backend, state string) (string, error)
}

// workerProvider groups the kinds of one provider.
type workerProvider struct {
	Name  string
	Label string
	Kinds []kindSpec
	// Integration is the name under which a user's connection is kept (user_integrations); empty
	// when users do not connect to the provider.
	Integration string
	// OAuth is set for providers users connect to by authorising access. The connect job is the
	// kind "<provider>.connect"; the page's callback queues it.
	OAuth *oauthSpec
}

// providerNamed finds a provider by name.
func (s *Backend) providerNamed(name string) (*workerProvider, bool) {
	for i := range s.providers {
		if s.providers[i].Name == name {
			return &s.providers[i], true
		}
	}
	return nil, false
}

// kind returns the provider's kind for an action ("import"), or nil.
func (p *workerProvider) kind(action string) *kindSpec {
	for i := range p.Kinds {
		if p.Kinds[i].Kind == workerapi.Kind(p.Name, action) {
			return &p.Kinds[i]
		}
	}
	return nil
}

// kindNames lists the provider's kinds.
func (p *workerProvider) kindNames() []string {
	out := make([]string, 0, len(p.Kinds))
	for _, k := range p.Kinds {
		out = append(out, k.Kind)
	}
	return out
}

// oauthProvider returns the named provider if users connect to it by OAuth and the server has the
// credentials for that.
func (s *Backend) oauthProvider(ctx context.Context, name string) (*workerProvider, bool) {
	p, ok := s.providerNamed(name)
	if !ok || p.OAuth == nil {
		return nil, false
	}
	if configured, err := p.OAuth.Configured(ctx, s); err != nil || !configured {
		return nil, false
	}
	return p, true
}

// connected reports whether the user has a stored connection to the provider.
func (s *Backend) connected(ctx context.Context, p *workerProvider, userID int64) bool {
	if p.Integration == "" {
		return false
	}
	_, err := s.store.GetIntegration(ctx, userID, p.Integration)
	return err == nil
}

var (
	// errBadResult: the result a worker reported cannot be applied, and no retry will change that.
	errBadResult = errors.New("the worker's result was not understood")
	// errKindPaused: an administrator (or the circuit breaker) paused the kind.
	errKindPaused = errors.New("paused")
	// errNoWorker: no worker that runs the kind has asked for work lately.
	errNoWorker = errors.New("no worker")
)

// permanentError marks a failure no retry can fix.
type permanentError struct{ error }

func (p permanentError) Unwrap() error { return p.error }

func permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// withWorkerProvider adds a provider when a backend is built. The providers that ship with the
// program are added by NewBackend itself; tests add their own.
func withWorkerProvider(p workerProvider) Option {
	return func(s *Backend) { s.addWorkerProvider(p) }
}

func (s *Backend) addWorkerProvider(p workerProvider) {
	if s.kinds == nil {
		s.kinds = map[string]*kindSpec{}
	}
	s.providers = append(s.providers, p)
	prov := &s.providers[len(s.providers)-1]
	for i := range prov.Kinds {
		s.kinds[prov.Kinds[i].Kind] = &prov.Kinds[i]
	}
}

// kindNames lists every registered kind.
func (s *Backend) kindNames() []string {
	var out []string
	for _, p := range s.providers {
		for _, k := range p.Kinds {
			out = append(out, k.Kind)
		}
	}
	return out
}

// providerLabelOf names the provider of a kind for messages ("Withings").
func (s *Backend) providerLabelOf(kind string) string {
	for _, p := range s.providers {
		for _, k := range p.Kinds {
			if k.Kind == kind {
				return p.Label
			}
		}
	}
	return kind
}
