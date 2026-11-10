package valkeystate

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// Sessions are the same thing as the sessions table, kept on this server when one is configured, so
// that every page of a signed-in visitor costs the database no session lookup. Three keys hold them:
//
//	dexafix:sess:<token hash>   the session (JSON), expiring with it: the only place a session is read
//	dexafix:usess:<user id>     a sorted set of that user's token hashes scored by expiry, so that
//	                            "sign out everywhere" can find them
//	dexafix:sessions            a sorted set of every token hash scored by expiry, for the count
//
// A session ends in exactly three ways, and all three are explicit: it expires (the key has a life),
// DeleteSession (sign-out) and DeleteUserSessions (password change, reset, deleting the account).
// Nothing here is a cache of the database: there is no copy to go stale, and what the user is (name,
// role, whether they exist) is never kept here, the backend reads it from the database on every
// request.
const (
	sessPrefix  = "dexafix:sess:"
	usessPrefix = "dexafix:usess:"
	sessIndex   = "dexafix:sessions"
)

type sessionValue struct {
	UserID  int64     `json:"u"`
	Method  string    `json:"m"`
	Created time.Time `json:"c"`
	Expires time.Time `json:"e"`
}

func userKey(userID int64) string { return usessPrefix + strconv.FormatInt(userID, 10) }

// CreateSession stores a session until expires.
func (v *Client) CreateSession(ctx context.Context, tokenHash string, userID int64, method string, expires time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	now := v.now()
	val, err := json.Marshal(sessionValue{UserID: userID, Method: method, Created: now.UTC(), Expires: expires.UTC()})
	if err != nil {
		return err
	}
	exp := expires.UnixMilli()
	uk := userKey(userID)
	cutoff := strconv.FormatInt(now.UnixMilli(), 10)
	return firstError(v.c.DoMulti(ctx,
		v.c.B().Set().Key(sessPrefix+tokenHash).Value(string(val)).PxatMillisecondsTimestamp(exp).Build(),
		v.c.B().Zadd().Key(uk).ScoreMember().ScoreMember(float64(exp), tokenHash).Build(),
		v.c.B().Pexpireat().Key(uk).MillisecondsTimestamp(exp).Build(),
		v.c.B().Zadd().Key(sessIndex).ScoreMember().ScoreMember(float64(exp), tokenHash).Build(),
		// Expired ones are no use to anybody: dropping them keeps the sets small.
		v.c.B().Zremrangebyscore().Key(uk).Min("-inf").Max(cutoff).Build(),
		v.c.B().Zremrangebyscore().Key(sessIndex).Min("-inf").Max(cutoff).Build(),
	))
}

// GetSession returns an unexpired session, or store.ErrNotFound.
func (v *Client) GetSession(ctx context.Context, tokenHash string) (models.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	raw, err := v.c.Do(ctx, v.c.B().Get().Key(sessPrefix+tokenHash).Build()).ToString()
	if valkey.IsValkeyNil(err) {
		return models.Session{}, store.ErrNotFound
	}
	if err != nil {
		return models.Session{}, err
	}
	var val sessionValue
	if err := json.Unmarshal([]byte(raw), &val); err != nil {
		return models.Session{}, errors.New("a session in Valkey cannot be read") // never echo it
	}
	if !val.Expires.After(v.now()) {
		return models.Session{}, store.ErrNotFound
	}
	return models.Session{UserID: val.UserID, Method: val.Method, CreatedAt: val.Created, ExpiresAt: val.Expires}, nil
}

// DeleteSession ends one session. A session that does not exist is not an error.
func (v *Client) DeleteSession(ctx context.Context, tokenHash string) error {
	sess, err := v.GetSession(ctx, tokenHash)
	if errors.Is(err, store.ErrNotFound) {
		// Gone or expired: make sure no key is left behind all the same.
		ctx, cancel := context.WithTimeout(ctx, opTimeout)
		defer cancel()
		return firstError(v.c.DoMulti(ctx,
			v.c.B().Del().Key(sessPrefix+tokenHash).Build(),
			v.c.B().Zrem().Key(sessIndex).Member(tokenHash).Build(),
		))
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return firstError(v.c.DoMulti(ctx,
		v.c.B().Del().Key(sessPrefix+tokenHash).Build(),
		v.c.B().Zrem().Key(userKey(sess.UserID)).Member(tokenHash).Build(),
		v.c.B().Zrem().Key(sessIndex).Member(tokenHash).Build(),
	))
}

// DeleteUserSessions ends every session of one user. Only the sessions seen at the start are removed
// from the index, so one that is created at the same moment stays reachable by the next call.
func (v *Client) DeleteUserSessions(ctx context.Context, userID int64) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	uk := userKey(userID)
	hashes, err := v.c.Do(ctx, v.c.B().Zrange().Key(uk).Min("0").Max("-1").Build()).AsStrSlice()
	if err != nil {
		return err
	}
	if len(hashes) == 0 {
		return nil
	}
	// One DEL per key: the client refuses a command whose keys are in different cluster slots, and
	// these keys are (each session is a key of its own).
	cmds := make(valkey.Commands, 0, len(hashes)+2)
	for _, h := range hashes {
		cmds = append(cmds, v.c.B().Del().Key(sessPrefix+h).Build())
	}
	cmds = append(cmds,
		v.c.B().Zrem().Key(uk).Member(hashes...).Build(),
		v.c.B().Zrem().Key(sessIndex).Member(hashes...).Build(),
	)
	return firstError(v.c.DoMulti(ctx, cmds...))
}

// CountSessions returns how many sessions are alive.
func (v *Client) CountSessions(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	cutoff := strconv.FormatInt(v.now().UnixMilli(), 10)
	return v.c.Do(ctx, v.c.B().Zcount().Key(sessIndex).Min("("+cutoff).Max("+inf").Build()).AsInt64()
}
