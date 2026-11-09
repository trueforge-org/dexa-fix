// Package bodybackend is the body service: a user's scale readings and DEXA scans, the estimates
// computed from them and the rules for importing readings from a source such as Withings.
//
// It knows nothing about who a user is. The main backend authenticates people and calls this
// service with a short-lived signed token for one user and one scope (see pkg/shared/bodyapi); the
// service checks the token and acts on exactly that user's data. It never imports the main
// backend, the frontend, the workers or anything that signs people in.
package bodybackend

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// Service is the body service. Use Handler to serve it.
type Service struct {
	store   store.BodyStore
	secret  string   // the secret a call carries
	spare   []string // others a call may carry too, so a secret can be rotated
	verify  *bodyapi.Verifier
	version string
	est     *estimateCache
	now     func() time.Time

	// Separate slots keep one kind of work from taking every slot of another: estimates are
	// CPU-bound, imports and exports are long, reads are short.
	estimateSlots chan struct{}
	bulkSlots     chan struct{}
}

// Option changes how New builds a Service.
type Option func(*Service)

// WithVersion names the running program. It is part of the estimates' ETag, so a new version never
// answers "not modified" for a result an older algorithm made.
func WithVersion(v string) Option { return func(s *Service) { s.version = v } }

// WithSpareSecrets makes the service accept these secrets too, next to the first one, so that the
// secret can be rotated without downtime: the main backend sends the first line of its file, and the
// service accepts every line of its own until the old one is removed.
func WithSpareSecrets(secrets ...string) Option {
	return func(s *Service) { s.spare = append(s.spare, secrets...) }
}

// secretOK compares the presented secret with every accepted one, in constant time, so the time
// does not say which one matched.
func (s *Service) secretOK(presented string) bool {
	ok := subtle.ConstantTimeCompare([]byte(presented), []byte(s.secret))
	for _, sec := range s.spare {
		ok |= subtle.ConstantTimeCompare([]byte(presented), []byte(sec))
	}
	return ok == 1
}

// bulkConcurrency is how many imports and exports may run at once.
const bulkConcurrency = 2

// New returns a Service over a store. secret is the shared secret every call must carry, and
// verifier holds the public keys of the main backend.
func New(st store.BodyStore, secret string, verifier *bodyapi.Verifier, opts ...Option) (*Service, error) {
	if st == nil || secret == "" || verifier == nil {
		return nil, errors.New("the body service needs a store, a secret and a verifier")
	}
	s := &Service{
		store: st, secret: secret, verify: verifier, version: "dev",
		est: newEstimateCache(), now: time.Now,
		estimateSlots: make(chan struct{}, max(1, runtime.GOMAXPROCS(0))),
		bulkSlots:     make(chan struct{}, bulkConcurrency),
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// CacheStats reports how often an estimate came from the cache, for tests and diagnostics.
func (s *Service) CacheStats() (hits, misses int64) {
	s.est.mu.Lock()
	defer s.est.mu.Unlock()
	return s.est.hits, s.est.misses
}

// Handler serves the body API.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	p := bodyapi.Prefix
	user := func(scope bodyapi.Scope, h func(http.ResponseWriter, *http.Request, bodyapi.Claims)) http.HandlerFunc {
		return s.guard(scope, true, h)
	}
	global := func(h func(http.ResponseWriter, *http.Request, bodyapi.Claims)) http.HandlerFunc {
		return s.guard(bodyapi.ScopeAdmin, false, h)
	}
	mux.HandleFunc("GET "+p+"/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET "+p+"/users/{id}/scans", user(bodyapi.ScopeRead, s.listScans))
	mux.HandleFunc("POST "+p+"/users/{id}/scans", user(bodyapi.ScopeWrite, s.createScan))
	mux.HandleFunc("DELETE "+p+"/users/{id}/scans/{item}", user(bodyapi.ScopeWrite, s.deleteScan))
	mux.HandleFunc("GET "+p+"/users/{id}/readings", user(bodyapi.ScopeRead, s.listReadings))
	mux.HandleFunc("POST "+p+"/users/{id}/readings", user(bodyapi.ScopeWrite, s.upsertReadings))
	mux.HandleFunc("DELETE "+p+"/users/{id}/readings/{item}", user(bodyapi.ScopeWrite, s.deleteReading))
	mux.HandleFunc("POST "+p+"/users/{id}/readings/reconcile", user(bodyapi.ScopeWrite, s.reconcileHandler))
	mux.HandleFunc("GET "+p+"/users/{id}/estimates", user(bodyapi.ScopeRead, s.estimatesHandler))
	mux.HandleFunc("GET "+p+"/users/{id}/export", user(bodyapi.ScopeRead, s.exportHandler))
	mux.HandleFunc("DELETE "+p+"/users/{id}", user(bodyapi.ScopeAdmin, s.deleteUserHandler))
	mux.HandleFunc("GET "+p+"/stats", global(s.statsHandler))
	mux.HandleFunc("GET "+p+"/users", global(s.userIDsHandler))
	return mux
}

// guard lets a call through only with the shared secret and a valid token whose scope is enough.
// A call about one user needs the token's subject to be that user; a call about nobody in
// particular needs subject 0. Every failure of the secret or the token answers the same way.
func (s *Service) guard(scope bodyapi.Scope, perUser bool, h func(http.ResponseWriter, *http.Request, bodyapi.Claims)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.secretOK(r.Header.Get(bodyapi.HeaderSecret)) {
			fail(w, bodyapi.CodeUnauthorized, "unauthorized")
			return
		}
		c, err := s.verify.Verify(r.Header.Get(bodyapi.HeaderToken), s.now())
		if err != nil {
			fail(w, bodyapi.CodeUnauthorized, "unauthorized")
			return
		}
		if !c.Scope.Allows(scope) {
			fail(w, bodyapi.CodeForbidden, "forbidden")
			return
		}
		if perUser {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil || id <= 0 || id != c.Subject {
				fail(w, bodyapi.CodeForbidden, "forbidden")
				return
			}
		} else if c.Subject != 0 {
			fail(w, bodyapi.CodeForbidden, "forbidden")
			return
		}
		h(w, r, c)
	}
}

// acquire takes a slot, waiting for one until the request ends. It answers "unavailable" and
// returns false when the request ended first.
func acquire(w http.ResponseWriter, r *http.Request, slots chan struct{}) (release func(), ok bool) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	case <-r.Context().Done():
		fail(w, bodyapi.CodeUnavailable, "the body service is busy")
		return nil, false
	}
}

// digest shortens a SHA-256 to what fits in a cache key or an ETag.
func digest(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// storeError answers for an error from the store.
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, bodyapi.CodeNotFound, "not found")
	case errors.Is(err, store.ErrInvalid):
		fail(w, bodyapi.CodeInvalid, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		fail(w, bodyapi.CodeUnavailable, "the request was cancelled")
	default:
		log.Error().Err(err).Msg("body request failed")
		fail(w, bodyapi.CodeInternal, "internal error")
	}
}
