package mainbackend

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/trueforge-org/dexa-fix/pkg/shared/edge"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// The generic HTTP hardening (security headers, body limits, compression, the failure limiter)
// lives in pkg/shared/edge. What stays here is specific to the sign-in and page code.

const (
	maxLoginFail = 10 // per login name
	maxIPFail    = 60 // per client address: generous, because a proxy makes many users share one
)

// failWindow is the sliding window in which failed attempts count towards a lock-out.
const failWindow = 15 * time.Minute

// dbLimiter counts failed attempts per key in a sliding window. The attempts are rows in the
// database, so every backend process on it sees the same count. It only counts failures, so people
// who type the right password never notice it. A database error blocks (fails closed).
type dbLimiter struct {
	st  store.Store
	now func() time.Time
}

func newDBLimiter(st store.Store) *dbLimiter { return &dbLimiter{st: st, now: time.Now} }

// Blocked reports whether key already used up its attempts.
func (l *dbLimiter) Blocked(ctx context.Context, key string, max int) bool {
	n, err := l.st.CountFailures(ctx, key, l.now().Add(-failWindow))
	if err != nil {
		log.Error().Err(err).Msg("count failed attempts: treating the key as blocked")
		return true
	}
	return n >= max
}

// Fail notes a failed attempt. If it cannot be stored the attempt is not counted; that is logged.
func (l *dbLimiter) Fail(ctx context.Context, key string) {
	if err := l.st.RecordFailure(ctx, key, l.now()); err != nil {
		log.Error().Err(err).Msg("record a failed attempt")
	}
}

// Reset forgets the attempts of key, after a success.
func (l *dbLimiter) Reset(ctx context.Context, key string) {
	if err := l.st.ClearFailures(ctx, key); err != nil {
		log.Error().Err(err).Msg("clear failed attempts")
	}
}

func clientIP(r *http.Request) string { return edge.ClientIP(r) }

func ipKey(r *http.Request) string { return edge.IPKey(r) }

// dbNonces remembers which single-use sealed values were already spent, until the value would have
// expired anyway, so that a value which may work only once cannot be replayed. The nonces are rows
// in the database, so a value spent on one backend process is spent on all of them.
type dbNonces struct{ st store.Store }

// nonceStore is what the flows use; dbNonces is the implementation.
type nonceStore interface {
	seen(ctx context.Context, nonce string) bool
	use(ctx context.Context, nonce string, expires time.Time) bool
}

// seen reports whether nonce was already used, without recording it. A flow checks it before it
// spends anything (a code) and records the nonce with use only once it has succeeded. On a
// database error it reports true: the value is refused.
func (n dbNonces) seen(ctx context.Context, nonce string) bool {
	used, err := n.st.NonceSeen(ctx, nonce)
	if err != nil {
		log.Error().Err(err).Msg("look up a used nonce: refusing the value")
		return true
	}
	return used
}

// use records nonce and reports whether this is the first time it is seen. On a database error it
// reports false: the value counts as already used.
func (n dbNonces) use(ctx context.Context, nonce string, expires time.Time) bool {
	first, err := n.st.UseNonce(ctx, nonce, expires)
	if err != nil {
		log.Error().Err(err).Msg("record a used nonce: refusing the value")
		return false
	}
	return first
}

func loginKey(login string) string { return "login:" + strings.ToLower(strings.TrimSpace(login)) }

// throttled reports whether this client or this login name made too many failed attempts.
func (s *Backend) throttled(r *http.Request, login string) bool {
	ctx := r.Context()
	return s.limiter.Blocked(ctx, ipKey(r), maxIPFail) || (login != "" && s.limiter.Blocked(ctx, loginKey(login), maxLoginFail))
}

func (s *Backend) recordFailure(r *http.Request, login string) {
	s.limiter.Fail(r.Context(), ipKey(r))
	if login != "" {
		s.limiter.Fail(r.Context(), loginKey(login))
	}
}
