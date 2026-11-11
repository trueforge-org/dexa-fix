package mainbackend

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/edge"
)

// The generic HTTP hardening (security headers, body limits, compression, the failure limiter)
// lives in pkg/shared/edge. What stays here is specific to the sign-in and page code.

const (
	maxLoginFail = 10 // per login name
	maxIPFail    = 60 // per client address: generous, because a proxy makes many users share one
)

// failWindow is the sliding window in which failed attempts count towards a lock-out.
const failWindow = 15 * time.Minute

// StateStore holds the state that must agree across backend processes and is touched on every
// sign-in: the failed-attempt counters and the single-use nonces. The database holds it unless a
// Valkey server is configured (WithState), which keeps a flood of wrong passwords off the database.
// store.Store and valkeystate.Client both satisfy it.
type StateStore interface {
	RecordFailure(ctx context.Context, key string, at time.Time) error
	CountFailures(ctx context.Context, key string, since time.Time) (int, error)
	ClearFailures(ctx context.Context, key string) error
	UseNonce(ctx context.Context, nonce string, expires time.Time) (first bool, err error)
	NonceSeen(ctx context.Context, nonce string) (bool, error)
}

// SessionState holds the sessions on a server of its own (Valkey), in place of the sessions table.
// It is what store.Store offers for sessions, except that it knows nothing about users: the backend
// reads the user (name, role, whether they exist) from the database on every request.
type SessionState interface {
	CreateSession(ctx context.Context, tokenHash string, userID int64, method string, expires time.Time) error
	GetSession(ctx context.Context, tokenHash string) (models.Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteUserSessions(ctx context.Context, userID int64) error
	CountSessions(ctx context.Context) (int64, error)
}

// StateServer is a StateStore and a SessionState on a server of its own, which can also be asked
// whether it answers.
type StateServer interface {
	StateStore
	SessionState
	Ping(ctx context.Context) error
}

// failureLimiter counts failed attempts per key in a sliding window. The attempts are shared state
// (rows in the database, or a Valkey server), so every backend process sees the same count. It only
// counts failures, so people who type the right password never notice it. An error from the state
// store blocks (fails closed).
type failureLimiter struct {
	st  StateStore
	now func() time.Time
}

func newFailureLimiter(st StateStore) *failureLimiter { return &failureLimiter{st: st, now: time.Now} }

// Blocked reports whether key already used up its attempts.
func (l *failureLimiter) Blocked(ctx context.Context, key string, max int) bool {
	n, err := l.st.CountFailures(ctx, key, l.now().Add(-failWindow))
	if err != nil {
		log.Error().Err(err).Msg("count failed attempts: treating the key as blocked")
		return true
	}
	return n >= max
}

// Attempt notes an attempt for key before it is checked, and reports whether it may go on: at most
// max attempts in the window do. Counting first and deciding after is what keeps a burst of
// parallel guesses from all passing a check that none of them has been counted for yet. The caller
// resets the key when the attempt turns out to be right, so a correct code or password costs
// nothing. An error from the state store refuses (fails closed).
func (l *failureLimiter) Attempt(ctx context.Context, key string, max int) bool {
	if err := l.st.RecordFailure(ctx, key, l.now()); err != nil {
		log.Error().Err(err).Msg("record an attempt: refusing it")
		return false
	}
	n, err := l.st.CountFailures(ctx, key, l.now().Add(-failWindow))
	if err != nil {
		log.Error().Err(err).Msg("count attempts: refusing the attempt")
		return false
	}
	return n <= max
}

// Fail notes a failed attempt. If it cannot be stored the attempt is not counted; that is logged.
func (l *failureLimiter) Fail(ctx context.Context, key string) {
	if err := l.st.RecordFailure(ctx, key, l.now()); err != nil {
		log.Error().Err(err).Msg("record a failed attempt")
	}
}

// Reset forgets the attempts of key, after a success.
func (l *failureLimiter) Reset(ctx context.Context, key string) {
	if err := l.st.ClearFailures(ctx, key); err != nil {
		log.Error().Err(err).Msg("clear failed attempts")
	}
}

func clientIP(r *http.Request) string { return edge.ClientIP(r) }

func ipKey(r *http.Request) string { return edge.IPKey(r) }

// spentNonces remembers which single-use sealed values were already spent, until the value would have
// expired anyway, so that a value which may work only once cannot be replayed. The nonces are shared
// state (rows in the database, or keys in a Valkey server), so a value spent on one backend process
// is spent on all of them.
type spentNonces struct{ st StateStore }

// nonceStore is what the flows use; spentNonces is the implementation.
type nonceStore interface {
	seen(ctx context.Context, nonce string) bool
	use(ctx context.Context, nonce string, expires time.Time) bool
}

// seen reports whether nonce was already used, without recording it. A flow checks it before it
// spends anything (a code) and records the nonce with use only once it has succeeded. On an
// error it reports true: the value is refused.
func (n spentNonces) seen(ctx context.Context, nonce string) bool {
	used, err := n.st.NonceSeen(ctx, nonce)
	if err != nil {
		log.Error().Err(err).Msg("look up a used nonce: refusing the value")
		return true
	}
	return used
}

// use records nonce and reports whether this is the first time it is seen. On an error it reports
// false: the value counts as already used.
func (n spentNonces) use(ctx context.Context, nonce string, expires time.Time) bool {
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

// recordFailure counts a wrong password against the client address. The login name was counted
// before the password was checked (Attempt in throttledVerify).
func (s *Backend) recordFailure(r *http.Request) {
	s.limiter.Fail(r.Context(), ipKey(r))
}
