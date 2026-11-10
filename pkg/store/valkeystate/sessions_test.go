package valkeystate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/store"
)

func TestSessionLifecycle(t *testing.T) {
	v, mr := open(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)

	if err := v.CreateSession(ctx, "aaa", 7, "local", exp); err != nil {
		t.Fatal(err)
	}
	s, err := v.GetSession(ctx, "aaa")
	if err != nil || s.UserID != 7 || s.Method != "local" || s.ExpiresAt.Sub(exp).Abs() > time.Millisecond {
		t.Fatalf("get: %+v, %v", s, err)
	}
	if time.Since(s.CreatedAt) > time.Minute || s.CreatedAt.IsZero() {
		t.Errorf("created at %v", s.CreatedAt)
	}
	if _, err := v.GetSession(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unknown session: %v, want ErrNotFound", err)
	}
	if n, err := v.CountSessions(ctx); err != nil || n != 1 {
		t.Errorf("count = %d, %v; want 1", n, err)
	}

	// The key has the session's life: Valkey forgets it by itself.
	mr.FastForward(2 * time.Hour)
	if _, err := v.GetSession(ctx, "aaa"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an expired session: %v, want ErrNotFound", err)
	}
}

func TestSessionExpiryIsCheckedHereToo(t *testing.T) {
	v, _ := open(t)
	ctx := context.Background()
	// An expiry in the past: the key may exist for a moment, but the session is not valid.
	if err := v.CreateSession(ctx, "old", 1, "local", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := v.GetSession(ctx, "old"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a session that expired a minute ago: %v, want ErrNotFound", err)
	}
	if n, _ := v.CountSessions(ctx); n != 0 {
		t.Errorf("an expired session counted: %d", n)
	}
}

func TestDeleteSessionEndsOnlyThatOne(t *testing.T) {
	v, mr := open(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	for _, c := range []struct {
		hash string
		user int64
	}{{"a1", 1}, {"a2", 1}, {"b1", 2}} {
		if err := v.CreateSession(ctx, c.hash, c.user, "local", exp); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.DeleteSession(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.GetSession(ctx, "a1"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the deleted session still works")
	}
	for _, h := range []string{"a2", "b1"} {
		if _, err := v.GetSession(ctx, h); err != nil {
			t.Errorf("session %s: %v", h, err)
		}
	}
	if n, _ := v.CountSessions(ctx); n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	if members, _ := mr.ZMembers("dexafix:usess:1"); len(members) != 1 || members[0] != "a2" {
		t.Errorf("the user's index: %v", members)
	}
	// Deleting what is not there is fine.
	if err := v.DeleteSession(ctx, "a1"); err != nil {
		t.Errorf("deleting a session twice: %v", err)
	}
	if err := v.DeleteSession(ctx, "never-was"); err != nil {
		t.Errorf("deleting an unknown session: %v", err)
	}
}

func TestDeleteUserSessionsEndsThemAll(t *testing.T) {
	v, mr := open(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	for _, c := range []struct {
		hash string
		user int64
	}{{"a1", 1}, {"a2", 1}, {"a3", 1}, {"b1", 2}} {
		if err := v.CreateSession(ctx, c.hash, c.user, "local", exp); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.DeleteUserSessions(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"a1", "a2", "a3"} {
		if _, err := v.GetSession(ctx, h); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("session %s of the user survived: %v", h, err)
		}
	}
	if _, err := v.GetSession(ctx, "b1"); err != nil {
		t.Errorf("another user's session ended: %v", err)
	}
	if n, _ := v.CountSessions(ctx); n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
	for _, k := range mr.Keys() {
		if strings.HasPrefix(k, "dexafix:usess:1") || k == "dexafix:sess:a1" {
			t.Errorf("key %s left behind", k)
		}
	}
	// A user without sessions is not an error, and a new session after the call is a normal one.
	if err := v.DeleteUserSessions(ctx, 1); err != nil {
		t.Errorf("second call: %v", err)
	}
	if err := v.CreateSession(ctx, "a4", 1, "local", exp); err != nil {
		t.Fatal(err)
	}
	if _, err := v.GetSession(ctx, "a4"); err != nil {
		t.Errorf("a session made after the call: %v", err)
	}
}

func TestSessionsFailWhenTheServerIsDown(t *testing.T) {
	v, mr := open(t)
	ctx := context.Background()
	if err := v.CreateSession(ctx, "x", 1, "local", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	mr.Close()
	if _, err := v.GetSession(ctx, "x"); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Errorf("a down server must be an error, not 'no such session': %v", err)
	}
	if err := v.DeleteUserSessions(ctx, 1); err == nil {
		t.Error("ending sessions while the server is down must fail, so a password is not changed")
	}
	if err := v.CreateSession(ctx, "y", 1, "local", time.Now().Add(time.Hour)); err == nil {
		t.Error("creating a session while the server is down must fail")
	}
}

func TestACorruptSessionIsAnErrorThatNamesNothing(t *testing.T) {
	v, mr := open(t)
	mr.Set("dexafix:sess:bad", "not json {secret-looking}")
	_, err := v.GetSession(context.Background(), "bad")
	if err == nil || errors.Is(err, store.ErrNotFound) || strings.Contains(err.Error(), "secret-looking") {
		t.Errorf("a corrupt session: %v", err)
	}
}
