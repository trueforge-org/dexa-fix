// Package valkeystate keeps the state that must agree across backend processes and is touched on
// every sign-in in Valkey (or any Redis-compatible server) instead of the database: the failed
// attempt counters and the single-use nonces. It is optional. Without it the same state lives in
// the database tables (used_nonces, auth_failures), so a flood of wrong passwords turns into
// database writes; with it the flood costs the database nothing.
//
// It has the same methods and meaning as the store's, so the backend uses either one. Every
// operation has a time limit, and every error is returned: the backend fails closed on them (an
// address counts as locked out, a nonce as spent), exactly as it does when the database fails.
//
// Nothing here is evicted quietly. The server must run with maxmemory-policy noeviction: a counter
// or a nonce that was evicted would let an attacker try again, or replay a value, so a full server
// must refuse writes (which fails closed) instead.
package valkeystate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"
)

const (
	failPrefix  = "dexafix:fail:"
	noncePrefix = "dexafix:nonce:"

	// failTTL bounds how long a counter lives without a new failure. The window the backend counts
	// in is shorter (15 minutes), so this only limits memory.
	failTTL = time.Hour
	// nonceSlack is added to a nonce's life so that a small difference between clocks cannot let a
	// value that is still valid be used twice.
	nonceSlack = time.Minute
	// opTimeout is the longest any one operation may take.
	opTimeout = 2 * time.Second
)

// Client talks to the server. Its methods are safe for concurrent use.
type Client struct {
	c   valkey.Client
	now func() time.Time
}

// Open connects to the server at rawURL (redis://[:password@]host:port[/db], or rediss:// for TLS)
// and checks that it answers.
func Open(ctx context.Context, rawURL string) (*Client, error) {
	opt, err := valkey.ParseURL(rawURL)
	if err != nil {
		// The error of the parser can quote the URL, which holds the password.
		return nil, errors.New("the Valkey URL is not valid: use redis://[:password@]host:port[/db] or rediss://")
	}
	opt.DisableCache = true // client-side caching would hide a counter that another process changed
	opt.ClientName = "dexa-fix"
	c, err := valkey.NewClient(opt)
	if err != nil {
		return nil, fmt.Errorf("connect to Valkey: %w", redact(err))
	}
	v := &Client{c: c, now: time.Now}
	if err := v.Ping(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("connect to Valkey: %w", err)
	}
	return v, nil
}

// redact keeps a password out of an error that quotes the address.
func redact(err error) error { return errors.New(err.Error()) }

// Close closes the connections.
func (v *Client) Close() { v.c.Close() }

// Ping checks that the server answers.
func (v *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return v.c.Do(ctx, v.c.B().Ping().Build()).Error()
}

// firstError returns the first error among the results of a pipeline.
func firstError(res []valkey.ValkeyResult) error {
	for _, r := range res {
		if err := r.Error(); err != nil {
			return err
		}
	}
	return nil
}

// ---- failed attempts: a sorted set per key, scored by the time of each failure ----

// RecordFailure notes a failed attempt under key.
func (v *Client) RecordFailure(ctx context.Context, key string, at time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	var salt [6]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return err
	}
	// The member must be unique, or two failures in the same millisecond count once.
	member := strconv.FormatInt(at.UnixNano(), 10) + "-" + hex.EncodeToString(salt[:])
	k := failPrefix + key
	return firstError(v.c.DoMulti(ctx,
		v.c.B().Zadd().Key(k).ScoreMember().ScoreMember(float64(at.UnixMilli()), member).Build(),
		v.c.B().Pexpire().Key(k).Milliseconds(failTTL.Milliseconds()).Build(),
	))
}

// CountFailures counts the failures under key that are newer than since.
func (v *Client) CountFailures(ctx context.Context, key string, since time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	k := failPrefix + key
	cutoff := strconv.FormatInt(since.UnixMilli(), 10)
	res := v.c.DoMulti(ctx,
		// Older ones are no use to anybody: dropping them keeps the set small.
		v.c.B().Zremrangebyscore().Key(k).Min("-inf").Max(cutoff).Build(),
		v.c.B().Zcount().Key(k).Min("("+cutoff).Max("+inf").Build(),
	)
	if err := firstError(res); err != nil {
		return 0, err
	}
	n, err := res[1].AsInt64()
	return int(n), err
}

// ClearFailures forgets the failures under key, after a success.
func (v *Client) ClearFailures(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return v.c.Do(ctx, v.c.B().Del().Key(failPrefix+key).Build()).Error()
}

// ---- single-use nonces: a key with a life ----

// UseNonce claims nonce until expires and reports whether this is the first use. SET NX decides
// atomically, so two processes racing for the same value cannot both win.
func (v *Client) UseNonce(ctx context.Context, nonce string, expires time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	life := expires.Sub(v.now()) + nonceSlack
	if life < nonceSlack {
		life = nonceSlack // an expired value is refused elsewhere; this only keeps the claim
	}
	err := v.c.Do(ctx, v.c.B().Set().Key(noncePrefix+nonce).Value("1").Nx().PxMilliseconds(life.Milliseconds()).Build()).Error()
	if valkey.IsValkeyNil(err) {
		return false, nil // somebody holds it
	}
	return err == nil, err
}

// NonceSeen reports whether nonce was used, without claiming it.
func (v *Client) NonceSeen(ctx context.Context, nonce string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	n, err := v.c.Do(ctx, v.c.B().Exists().Key(noncePrefix+nonce).Build()).AsInt64()
	return n > 0, err
}
