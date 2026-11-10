package sqlstore

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// resetLinks counts the stored reset links of a user, live or not.
func resetLinks(t *testing.T, s *Store, userID int64) int64 {
	t.Helper()
	n, err := s.db.NewSelect().Table("password_resets").Where("user_id = ?", userID).Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPasswordResetLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	alice, err := s.CreateUser(ctx, "alice") // the first user is an administrator
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	carol, err := s.CreateUser(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	if _, err := s.PasswordResetUser(ctx, "hash-unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown link: %v", err)
	}

	if err := s.CreatePasswordReset(ctx, "hash-bob-1", bob.ID, nil, later); err != nil {
		t.Fatal(err)
	}
	if u, err := s.PasswordResetUser(ctx, "hash-bob-1"); err != nil || u.ID != bob.ID {
		t.Fatalf("live link: %+v %v", u, err)
	}

	// an expired link of another user is not usable, and the next create removes it
	if err := s.CreatePasswordReset(ctx, "hash-carol-old", carol.ID, nil, past); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PasswordResetUser(ctx, "hash-carol-old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired link: %v", err)
	}
	if n := resetLinks(t, s, carol.ID); n != 1 {
		t.Fatalf("expired row for carol: %d, want 1 before the next create", n)
	}

	// a second link for bob replaces the first, and purges carol's expired row
	if err := s.CreatePasswordReset(ctx, "hash-bob-2", bob.ID, nil, later); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PasswordResetUser(ctx, "hash-bob-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("first link still works after a second one was made: %v", err)
	}
	if u, err := s.PasswordResetUser(ctx, "hash-bob-2"); err != nil || u.ID != bob.ID {
		t.Fatalf("second link: %+v %v", u, err)
	}
	if n := resetLinks(t, s, bob.ID); n != 1 {
		t.Fatalf("links for bob: %d, want 1", n)
	}
	if n := resetLinks(t, s, carol.ID); n != 0 {
		t.Fatalf("expired row of carol not purged: %d", n)
	}

	// completing the link sets the password, signs the user out everywhere and removes the links
	if err := s.CreateSession(ctx, "sess-bob", bob.ID, models.ProviderLocal, later); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "sess-alice", alice.ID, models.ProviderLocal, later); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccessToken(ctx, &models.AccessToken{UserID: bob.ID, Name: "script", Prefix: "dxf_", Hash: "tok-bob"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.CompletePasswordReset(ctx, "hash-bob-2", "new-password-hash")
	if err != nil || id != bob.ID {
		t.Fatalf("complete: %d %v, want %d", id, err, bob.ID)
	}
	ident, err := s.GetIdentity(ctx, models.ProviderLocal, strconv.FormatInt(bob.ID, 10))
	if err != nil || ident.Secret != "new-password-hash" {
		t.Fatalf("password not set: %+v %v", ident, err)
	}
	if _, err := s.GetSession(ctx, "sess-bob"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session of bob survived the reset: %v", err)
	}
	if _, err := s.GetSession(ctx, "sess-alice"); err != nil {
		t.Errorf("session of another user was removed: %v", err)
	}
	if tokens, err := s.ListAccessTokens(ctx, bob.ID); err != nil || len(tokens) != 0 {
		t.Errorf("access tokens survived the reset: %d %v", len(tokens), err)
	}
	if _, err := s.CompletePasswordReset(ctx, "hash-bob-2", "again"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second complete: %v", err)
	}
	if n := resetLinks(t, s, bob.ID); n != 0 {
		t.Errorf("link left after completing: %d", n)
	}
	if ident, _ := s.GetIdentity(ctx, models.ProviderLocal, strconv.FormatInt(bob.ID, 10)); ident.Secret != "new-password-hash" {
		t.Errorf("a refused link changed the password to %q", ident.Secret)
	}

	// an expired link cannot be completed
	if err := s.CreatePasswordReset(ctx, "hash-bob-expired", bob.ID, nil, past); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompletePasswordReset(ctx, "hash-bob-expired", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired link completed: %v", err)
	}

	// deleting the links of a user
	if err := s.CreatePasswordReset(ctx, "hash-alice", alice.ID, nil, later); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePasswordResets(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PasswordResetUser(ctx, "hash-alice"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("link after DeletePasswordResets: %v", err)
	}
	if err := s.DeletePasswordResets(ctx, alice.ID); err != nil { // nothing left: not an error
		t.Errorf("second DeletePasswordResets: %v", err)
	}

	// deleting the user removes their links
	if err := s.CreatePasswordReset(ctx, "hash-bob-3", bob.ID, nil, later); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	if n := resetLinks(t, s, bob.ID); n != 0 {
		t.Errorf("links left after the user was deleted: %d", n)
	}
	if _, err := s.PasswordResetUser(ctx, "hash-bob-3"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("link of a deleted user: %v", err)
	}
}

func TestPasswordResetCompleteOnlyOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.CreateUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePasswordReset(ctx, "hash-race", bob.ID, nil, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Concurrent requests with the same link: exactly one wins.
	const n = 20
	var (
		wins  atomic.Int32
		wg    sync.WaitGroup
		errs  = make([]error, n)
		start = make(chan struct{})
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.CompletePasswordReset(ctx, "hash-race", "pw-"+strconv.Itoa(i))
			errs[i] = err
			if err == nil {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := wins.Load(); got != 1 {
		t.Fatalf("%d requests used the same link, want exactly 1", got)
	}
}

func TestPasswordResetSurvivesDeletedCreator(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.CreateUser(ctx, "alice"); err != nil { // administrator
		t.Fatal(err)
	}
	dana, err := s.CreateUser(ctx, "dana")
	if err != nil {
		t.Fatal(err)
	}
	eve, err := s.CreateUser(ctx, "eve")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRole(ctx, dana.ID, models.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePasswordReset(ctx, "hash-by-dana", eve.ID, &dana.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, dana.ID); err != nil {
		t.Fatal(err)
	}
	// created_by is cleared (ON DELETE SET NULL), the link itself stays usable
	if u, err := s.PasswordResetUser(ctx, "hash-by-dana"); err != nil || u.ID != eve.ID {
		t.Fatalf("link after its creator was deleted: %+v %v", u, err)
	}
}
