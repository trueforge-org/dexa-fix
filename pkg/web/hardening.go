package web

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/shared/edge"
)

// The generic HTTP hardening (security headers, body limits, compression, the failure limiter)
// lives in pkg/shared/edge. What stays here is specific to the sign-in and page code.

const (
	maxLoginFail = 10 // per login name
	maxIPFail    = 60 // per client address: generous, because a proxy makes many users share one

	maxLimiterKeys = 50000
)

type failLimiter = edge.FailLimiter

func newFailLimiter() *failLimiter { return edge.NewFailLimiter() }

func clientIP(r *http.Request) string { return edge.ClientIP(r) }

func ipKey(r *http.Request) string { return edge.IPKey(r) }

// usedNonces remembers the nonces of sealed values that were already used, until the value would
// have expired anyway, so that a value which may work only once cannot be replayed. It lives in
// memory: with several replicas behind one database the guarantee holds per replica.
type usedNonces struct {
	mu    sync.Mutex
	seen_ map[string]time.Time // nonce -> when the sealed value expires
	now   func() time.Time
}

func newUsedNonces() *usedNonces {
	return &usedNonces{seen_: map[string]time.Time{}, now: time.Now}
}

// seen reports whether nonce was already used, without recording it. A flow checks it before it
// spends anything (a code) and records the nonce with use only once it has succeeded.
func (u *usedNonces) seen(nonce string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	exp, ok := u.seen_[nonce]
	return ok && !exp.Before(u.now())
}

// use records nonce and reports whether this is the first time it is seen. A nonce that was
// forgotten under memory pressure can only belong to a value that is about to expire.
func (u *usedNonces) use(nonce string, expires time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()
	if len(u.seen_) > maxLimiterKeys {
		for k, exp := range u.seen_ {
			if exp.Before(now) {
				delete(u.seen_, k)
			}
		}
		if len(u.seen_) > maxLimiterKeys { // still full: drop everything rather than grow
			u.seen_ = map[string]time.Time{}
		}
	}
	if exp, ok := u.seen_[nonce]; ok && !exp.Before(now) {
		return false
	}
	u.seen_[nonce] = expires
	return true
}

func loginKey(login string) string { return "login:" + strings.ToLower(strings.TrimSpace(login)) }

// throttled reports whether this client or this login name made too many failed attempts.
func (s *Backend) throttled(r *http.Request, login string) bool {
	return s.limiter.Blocked(ipKey(r), maxIPFail) || (login != "" && s.limiter.Blocked(loginKey(login), maxLoginFail))
}

func (s *Backend) recordFailure(r *http.Request, login string) {
	s.limiter.Fail(ipKey(r))
	if login != "" {
		s.limiter.Fail(loginKey(login))
	}
}

// ---- static assets ----

// nonceStore remembers which single-use sealed values were already spent. The in-memory
// implementation (usedNonces) guarantees single use per process; a shared implementation is needed
// before several backend processes serve the same sign-in flows.
type nonceStore interface {
	seen(nonce string) bool
	use(nonce string, expires time.Time) bool
}
