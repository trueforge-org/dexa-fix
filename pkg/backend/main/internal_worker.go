package mainbackend

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/shared/edge"
	"github.com/trueforge-org/dexa-fix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// The worker endpoints, /internal/v1/worker. Workers are processes without a database that talk to
// outside services for the backend (see pkg/workers). They are authenticated with a worker secret
// that opens nothing else; the web frontend's service secret does not open these, and the worker
// secret does not open the pages' endpoints. What a worker may do is narrow: lease a job, renew
// the lease, hand over renewed credentials, and report how the attempt ended. Every call about a
// job carries its lease token, so a worker that lost its job cannot change anything.
//
// The backend never trusts what a worker says: results are validated by the kind's Apply, the
// error text is clipped, and nothing a worker sends picks the user, the kind or the credentials.

const (
	workerMaxBody       = 16 << 20 // an import result can hold years of readings
	workerMaxProgress   = 16 << 10
	workerMaxCredential = 64 << 10
	workerMaxErrorText  = 500

	workerMaxInFlight = 64
	workerMaxQueue    = 128
	workerRate        = 100
	workerBurst       = 200
	maxLeasePolls     = 64 // worker requests held open waiting for a job
)

// SetWorkerSecrets sets the secrets the worker endpoints accept. The first is the one workers
// send; the others are accepted so that a secret can be rotated without downtime. Without any, the
// worker endpoints refuse every call.
func (s *Backend) SetWorkerSecrets(secrets ...string) {
	var hashes [][32]byte
	for _, sec := range secrets {
		if sec = strings.TrimSpace(sec); sec != "" {
			hashes = append(hashes, sha256.Sum256([]byte(sec)))
		}
	}
	s.workerSecrets = hashes
}

// matchSecret compares a presented secret with all accepted ones, in constant time.
func matchSecret(accepted [][32]byte, presented string) bool {
	if presented == "" {
		return false
	}
	sum := sha256.Sum256([]byte(presented))
	ok := 0
	for _, h := range accepted { // all of them, so timing does not say which one matched
		ok |= subtle.ConstantTimeCompare(sum[:], h[:])
	}
	return ok == 1
}

func (s *Backend) workerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !matchSecret(s.workerSecrets, r.Header.Get(workerapi.HeaderWorker)) {
			internalFail(w, http.StatusUnauthorized, apiclient.CodeUnauthorized, "worker authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// workerHandler serves the worker endpoints.
func (s *Backend) workerHandler() http.Handler {
	p := workerapi.Prefix
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+p+"/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST "+p+"/register", s.workerRegister)
	mux.HandleFunc("GET "+p+"/config", s.workerConfig)
	mux.HandleFunc("POST "+p+"/lease", s.workerLease)
	mux.HandleFunc("POST "+p+"/jobs/{id}/heartbeat", s.workerHeartbeat)
	mux.HandleFunc("POST "+p+"/jobs/{id}/credentials", s.workerCredentials)
	mux.HandleFunc("POST "+p+"/jobs/{id}/complete", s.workerComplete)
	mux.HandleFunc("POST "+p+"/jobs/{id}/release", s.workerRelease)
	mux.HandleFunc(p+"/", func(w http.ResponseWriter, r *http.Request) {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
	})
	var h http.Handler = mux
	if s.workerBudget != nil {
		h = s.workerBudget.Middleware(h)
	}
	return s.workerAuth(h)
}

func newWorkerBudget() *edge.Limiter {
	return &edge.Limiter{
		MaxInFlight: workerMaxInFlight, MaxQueue: workerMaxQueue, MaxWait: budgetMaxWait,
		Rate: workerRate, Burst: workerBurst,
		// A request that waits for a job would hold a slot for up to 20 seconds; leases have their own cap.
		Skip: func(r *http.Request) bool {
			return strings.HasSuffix(r.URL.Path, "/lease") || strings.HasSuffix(r.URL.Path, "/healthz")
		},
	}
}

func workerDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, workerMaxBody)).Decode(v); err != nil {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid request: "+err.Error())
		return false
	}
	return true
}

// encodeJSON turns a payload into the text kept with a job; nil is empty.
func encodeJSON(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return string(raw), nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}

func newLeaseToken() (token, hash string, err error) {
	if token, err = randomHex(32); err != nil {
		return "", "", err
	}
	return token, hashToken(token), nil
}

// ---- configuration ----

func (s *Backend) workerConfigNow() workerapi.Config {
	live := s.cfg()
	cfg := workerapi.Config{Generation: live.generation, LeaseSeconds: int(workerLease.Get() / time.Second), Kinds: map[string]workerapi.KindConfig{}}
	for kind, ks := range live.workers.Kinds {
		cfg.Kinds[kind] = workerapi.KindConfig{Enabled: ks.Enabled, Options: ks.optionStrings()}
	}
	return cfg
}

func (s *Backend) workerConfig(w http.ResponseWriter, r *http.Request) {
	internalJSON(w, http.StatusOK, s.workerConfigNow())
}

func (s *Backend) knownKinds(kinds []string) []string {
	var out []string
	for _, k := range kinds {
		if _, ok := s.kinds[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

func (s *Backend) workerRegister(w http.ResponseWriter, r *http.Request) {
	var in workerapi.RegisterRequest
	if !workerDecode(w, r, &in) {
		return
	}
	if in.ID == "" || len(in.ID) > 64 || len(in.Version) > 64 {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid worker id")
		return
	}
	if err := s.store.TouchWorker(r.Context(), in.ID, s.knownKinds(in.Kinds), in.Version, time.Now()); err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusOK, s.workerConfigNow())
}

// ---- leasing ----

func (s *Backend) workerLease(w http.ResponseWriter, r *http.Request) {
	var in workerapi.LeaseRequest
	if !workerDecode(w, r, &in) {
		return
	}
	kinds := s.knownKinds(in.Kinds)
	if len(kinds) == 0 || in.Worker == "" || len(in.Worker) > 64 {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "name the worker and the kinds of job it runs")
		return
	}
	select {
	case s.leasePolls <- struct{}{}:
		defer func() { <-s.leasePolls }()
	default:
		internalFail(w, http.StatusServiceUnavailable, apiclient.CodeUnavailable, "too many workers are waiting for jobs")
		return
	}
	wait := min(max(in.WaitSeconds, 0), workerapi.MaxWaitSeconds)
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	ctx := r.Context()

	for {
		live := s.cfg().workers
		req := models.LeaseRequest{Kinds: kinds, Owner: in.Worker, LeaseFor: workerLease.Get(), MaxRunning: map[string]int{}}
		for _, k := range kinds {
			if !live.Kinds[k].Enabled {
				req.Paused = append(req.Paused, k)
			}
			if n := live.Kinds[k].MaxRunning; n > 0 {
				req.MaxRunning[k] = n
			}
		}
		token, hash, err := newLeaseToken()
		if err != nil {
			internalError(w, err)
			return
		}
		req.TokenHash = hash
		if err := s.store.TouchWorker(ctx, in.Worker, kinds, "", time.Now()); err != nil && ctx.Err() == nil {
			log.Warn().Err(err).Msg("record the worker")
		}
		s.maintainQueue(ctx)
		job, ok, err := s.store.LeaseJob(ctx, req, time.Now())
		if err != nil {
			if ctx.Err() == nil {
				internalError(w, err)
			}
			return
		}
		if ok {
			s.answerLease(ctx, w, job, token, hash)
			return
		}
		if !time.Now().Before(deadline) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(min(leasePollEvery.Get(), time.Until(deadline))):
		}
	}
}

// answerLease builds the payload of a leased job and sends it. When that is impossible the job is
// ended or given back here, so it never sits leased to a worker that does not know about it.
func (s *Backend) answerLease(ctx context.Context, w http.ResponseWriter, job models.Job, token, hash string) {
	spec := s.kinds[job.Kind]
	payload, err := spec.Lease(ctx, s, job)
	if err == nil {
		var text string
		if text, err = encodeJSON(payload); err == nil {
			if text == "" {
				text = "{}"
			}
			internalJSON(w, http.StatusOK, workerapi.Lease{
				Job: workerapi.Job{ID: job.PublicID, Kind: job.Kind, UserID: job.UserID, Attempt: job.Attempts,
					MaxAttempts: job.MaxAttempts, Payload: json.RawMessage(text)},
				Token: token, TimeoutSeconds: int(job.Timeout / time.Second), LeaseSeconds: int(workerLease.Get() / time.Second),
			})
			return
		}
	}
	rctx := context.WithoutCancel(ctx)
	var p permanentError
	if errors.As(err, &p) {
		done, cerr := s.store.CompleteJob(rctx, job.PublicID, hash, models.Completion{Outcome: models.OutcomeFail, Error: clip(p.Error(), workerMaxErrorText)}, time.Now())
		if cerr == nil {
			s.afterJob(rctx, done)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	log.Error().Err(err).Str("job", job.PublicID).Str("kind", job.Kind).Msg("could not prepare the job for a worker")
	if rerr := s.store.ReleaseJob(rctx, job.PublicID, hash, time.Now()); rerr != nil {
		log.Error().Err(rerr).Msg("could not give the job back")
	}
	internalFail(w, http.StatusServiceUnavailable, apiclient.CodeUnavailable, "could not prepare the job")
}

// ---- calls about a job ----

// leaseOf returns the hash of the token a call carries.
func leaseOf(r *http.Request) string {
	t := r.Header.Get(workerapi.HeaderLease)
	if t == "" {
		return ""
	}
	return hashToken(t)
}

func leaseLost(w http.ResponseWriter) {
	internalFail(w, http.StatusConflict, apiclient.CodeExists, "the job is no longer leased to this worker")
}

func (s *Backend) workerHeartbeat(w http.ResponseWriter, r *http.Request) {
	var in workerapi.Heartbeat
	if !workerDecode(w, r, &in) {
		return
	}
	var progress *string
	if len(in.Progress) > 0 {
		if len(in.Progress) > workerMaxProgress || !json.Valid(in.Progress) {
			internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid progress")
			return
		}
		p := string(in.Progress)
		progress = &p
	}
	cancel, err := s.store.HeartbeatJob(r.Context(), r.PathValue("id"), leaseOf(r), workerLease.Get(), progress, time.Now())
	switch {
	case errors.Is(err, store.ErrLeaseLost), errors.Is(err, store.ErrNotFound):
		leaseLost(w)
	case err != nil:
		internalError(w, err)
	default:
		internalJSON(w, http.StatusOK, workerapi.HeartbeatAnswer{Cancel: cancel})
	}
}

func (s *Backend) workerCredentials(w http.ResponseWriter, r *http.Request) {
	var in workerapi.Credentials
	if !workerDecode(w, r, &in) {
		return
	}
	if len(in.Data) == 0 || len(in.Data) > workerMaxCredential || !json.Valid(in.Data) {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid credentials")
		return
	}
	job, err := s.store.RunningJob(r.Context(), r.PathValue("id"), leaseOf(r))
	if errors.Is(err, store.ErrLeaseLost) {
		leaseLost(w)
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	spec := s.kinds[job.Kind]
	if spec == nil || spec.Credentials == nil {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "this kind of job does not take credentials")
		return
	}
	if err := spec.Credentials(r.Context(), s, job, in.Data); err != nil {
		if errors.Is(err, errBadResult) {
			internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid credentials")
			return
		}
		internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Backend) workerComplete(w http.ResponseWriter, r *http.Request) {
	var in workerapi.Completion
	if !workerDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	id, hash := r.PathValue("id"), leaseOf(r)
	job, err := s.store.RunningJob(ctx, id, hash)
	if errors.Is(err, store.ErrLeaseLost) {
		leaseLost(w)
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	spec := s.kinds[job.Kind]
	comp := models.Completion{Outcome: in.Outcome, Error: clip(in.Error, workerMaxErrorText)}
	switch in.Outcome {
	case models.OutcomeOK:
		stored, err := spec.Apply(ctx, s, job, in.Result)
		switch {
		case errors.Is(err, errBadResult):
			log.Warn().Err(err).Str("job", job.PublicID).Str("kind", job.Kind).Msg("a worker's result was refused")
			comp = models.Completion{Outcome: models.OutcomeFail, Error: "the worker's result could not be used"}
		case err != nil:
			// Our own trouble (the database): the worker reports again, and the job is not lost.
			log.Error().Err(err).Str("job", job.PublicID).Str("kind", job.Kind).Msg("could not apply a worker's result")
			internalFail(w, http.StatusServiceUnavailable, apiclient.CodeUnavailable, "could not store the result")
			return
		default:
			comp.Result = stored
		}
	case models.OutcomeRetry:
		delay := s.kindBackoff(job)
		if hint := time.Duration(max(in.RetryAfterSeconds, 0)) * time.Second; hint > delay {
			delay = min(hint, maxRetryDelay)
		}
		comp.RetryAt = time.Now().Add(delay)
		if comp.Error == "" {
			comp.Error = "a temporary problem"
		}
	case models.OutcomeFail:
		if comp.Error == "" {
			comp.Error = "failed"
		}
	default:
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "unknown outcome")
		return
	}
	done, err := s.store.CompleteJob(ctx, id, hash, comp, time.Now())
	if errors.Is(err, store.ErrLeaseLost) {
		leaseLost(w)
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	s.afterJob(context.WithoutCancel(ctx), done)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Backend) workerRelease(w http.ResponseWriter, r *http.Request) {
	err := s.store.ReleaseJob(r.Context(), r.PathValue("id"), leaseOf(r), time.Now())
	switch {
	case errors.Is(err, store.ErrLeaseLost), errors.Is(err, store.ErrNotFound):
		leaseLost(w)
	case err != nil:
		internalError(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
