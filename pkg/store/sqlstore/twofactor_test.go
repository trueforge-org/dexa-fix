package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/store"
)

// confirmedTOTP starts and confirms a two-step verification setup for userID with the code step.
func confirmedTOTP(t *testing.T, s *Store, userID int64, secret string, step int64) {
	t.Helper()
	ctx := context.Background()
	if err := s.PutPendingTOTP(ctx, userID, secret); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmTOTP(ctx, userID, step); err != nil {
		t.Fatal(err)
	}
}

func TestTOTPLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, err := s.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTOTP(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no setup: %v", err)
	}
	if err := s.DeleteTOTP(ctx, u.ID); err != nil { // no setup: nothing to remove
		t.Fatalf("delete without setup: %v", err)
	}

	if err := s.PutPendingTOTP(ctx, u.ID, "SECRET1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTOTP(ctx, u.ID)
	if err != nil || got.Secret != "SECRET1" || got.Confirmed {
		t.Fatalf("pending: %+v %v", got, err)
	}

	// Starting again discards the unconfirmed secret.
	if err := s.PutPendingTOTP(ctx, u.ID, "SECRET2"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetTOTP(ctx, u.ID)
	if err != nil || got.Secret != "SECRET2" || got.Confirmed {
		t.Fatalf("restarted: %+v %v", got, err)
	}

	if err := s.ConfirmTOTP(ctx, u.ID, 100); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetTOTP(ctx, u.ID)
	if err != nil || !got.Confirmed || got.LastStep != 100 || got.Secret != "SECRET2" {
		t.Fatalf("confirmed: %+v %v", got, err)
	}
	if err := s.ConfirmTOTP(ctx, u.ID, 101); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("confirm twice: %v", err)
	}
	if err := s.PutPendingTOTP(ctx, u.ID, "SECRET3"); !errors.Is(err, store.ErrExists) {
		t.Fatalf("replace confirmed: %v", err)
	}

	if err := s.DeleteTOTP(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTOTP(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.DeleteTOTP(ctx, u.ID); err != nil { // already gone
		t.Fatalf("second delete: %v", err)
	}
}

func TestUseTOTPStepIsOneShot(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, err := s.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	confirmedTOTP(t, s, u.ID, "SECRET", 100)

	for _, c := range []struct {
		step int64
		want bool
	}{
		{101, true},  // newer step is fresh
		{101, false}, // the same step again is replay
		{100, false}, // older than the guard
		{102, true},  // newer again
	} {
		got, err := s.UseTOTPStep(ctx, u.ID, c.step)
		if err != nil || got != c.want {
			t.Fatalf("step %d: got %v %v, want %v", c.step, got, err, c.want)
		}
	}

	// A setup the user has not confirmed accepts no step.
	pending, err := s.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutPendingTOTP(ctx, pending.ID, "PENDING"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UseTOTPStep(ctx, pending.ID, 500); err != nil || got {
		t.Fatalf("unconfirmed setup: got %v %v, want false", got, err)
	}

	// Concurrent requests with the same code: exactly one wins.
	c, err := s.CreateUser(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	confirmedTOTP(t, s, c.ID, "RACE", 100)
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
			ok, err := s.UseTOTPStep(ctx, c.ID, 500)
			errs[i] = err
			if ok {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := wins.Load(); got != 1 {
		t.Fatalf("%d requests won the same step, want exactly 1", got)
	}
}

func TestRecoveryCodes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, err := s.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	wantCount := func(userID int64, want int) {
		t.Helper()
		got, err := s.CountRecoveryCodes(ctx, userID)
		if err != nil || got != want {
			t.Fatalf("user %d: count %d %v, want %d", userID, got, err, want)
		}
	}

	if err := s.ReplaceRecoveryCodes(ctx, u.ID, []string{"h1", "h2", "h3"}); err != nil {
		t.Fatal(err)
	}
	wantCount(u.ID, 3)
	if ok, err := s.UseRecoveryCode(ctx, u.ID, "h2"); err != nil || !ok {
		t.Fatalf("first use of h2: %v %v", ok, err)
	}
	if ok, err := s.UseRecoveryCode(ctx, u.ID, "h2"); err != nil || ok {
		t.Fatalf("second use of h2: %v %v", ok, err)
	}
	wantCount(u.ID, 2)

	// A new set replaces the old one.
	if err := s.ReplaceRecoveryCodes(ctx, u.ID, []string{"h4", "h5"}); err != nil {
		t.Fatal(err)
	}
	wantCount(u.ID, 2)
	if ok, err := s.UseRecoveryCode(ctx, u.ID, "h1"); err != nil || ok {
		t.Fatalf("old code after replace: %v %v", ok, err)
	}
	if ok, err := s.UseRecoveryCode(ctx, u.ID, "h4"); err != nil || !ok {
		t.Fatalf("new code after replace: %v %v", ok, err)
	}
	wantCount(u.ID, 1)

	// Another user's code is not accepted for this user, and stays theirs.
	if err := s.ReplaceRecoveryCodes(ctx, other.ID, []string{"x1"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.UseRecoveryCode(ctx, u.ID, "x1"); err != nil || ok {
		t.Fatalf("another user's code: %v %v", ok, err)
	}
	wantCount(other.ID, 1)

	// Deleting the setup removes the codes that are left.
	if err := s.DeleteTOTP(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	wantCount(u.ID, 0)
}

func TestTwoFactorUsersAndStats(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a, err := s.CreateUser(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateUser(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateUser(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	confirmedTOTP(t, s, a.ID, "SECRET_A", 1)
	confirmedTOTP(t, s, c.ID, "SECRET_C", 1)
	if err := s.PutPendingTOTP(ctx, b.ID, "SECRET_B"); err != nil { // not confirmed
		t.Fatal(err)
	}

	users, err := s.TwoFactorUsers(ctx)
	if err != nil || len(users) != 2 || !users[a.ID] || !users[c.ID] || users[b.ID] {
		t.Fatalf("two-factor users: %v %v", users, err)
	}
	st, err := s.Stats(ctx)
	if err != nil || st.TwoFactorUsers != 2 {
		t.Fatalf("stats: %+v %v", st, err)
	}
}

func TestTOTPSecretIsEncryptedAndRekeyed(t *testing.T) {
	ctx := context.Background()
	const secret = "JBSWY3DPEHPK3PXP"
	path := filepath.Join(t.TempDir(), "a.db")
	s := openFile(t, path, WithCipher(cipherWith(t, 1)))
	u, err := s.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	confirmedTOTP(t, s, u.ID, secret, 10)
	rawSecret := func(st *Store, userID int64) string {
		return rawColumn(t, st, fmt.Sprintf(`SELECT secret FROM user_totp WHERE user_id = %d`, userID))
	}
	if raw := rawSecret(s, u.ID); !strings.HasPrefix(raw, encPrefix) || strings.Contains(raw, secret) {
		t.Fatalf("secret stored as %q", raw)
	}
	s.Close()

	// New key current, old key previous: the secret still opens.
	s = openFile(t, path, WithCipher(cipherWith(t, 2, 1)))
	dry, err := s.Rekey(ctx, true)
	if err != nil || dry.TOTP != 1 || dry.Changed() != 1 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	done, err := s.Rekey(ctx, false)
	if err != nil || done.TOTP != 1 || done.Changed() != dry.Changed() {
		t.Fatalf("rekey: %+v %v", done, err)
	}
	got, err := s.GetTOTP(ctx, u.ID)
	if err != nil || got.Secret != secret || !got.Confirmed || got.LastStep != 10 {
		t.Fatalf("after rekey: %+v %v", got, err)
	}
	if raw := rawSecret(s, u.ID); !strings.HasPrefix(raw, encV2+s.cipher.current.id+":") {
		t.Fatalf("secret not under the new key: %q", raw)
	}
	again, err := s.Rekey(ctx, false)
	if err != nil || again.TOTP != 0 {
		t.Fatalf("second run: %+v %v", again, err)
	}

	// The ciphertext is bound to its row: another user's row does not open it.
	bob, err := s.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutPendingTOTP(ctx, bob.ID, "OTHER"); err != nil {
		t.Fatal(err)
	}
	stolen := rawSecret(s, u.ID)
	if _, err := s.db.NewUpdate().Model((*totpRow)(nil)).Set("secret = ?", stolen).Where("user_id = ?", bob.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTOTP(ctx, bob.ID); err == nil {
		t.Fatal("another user's secret was accepted")
	}
}

func TestTOTPEncryptedOnOpenWhenEnablingEncryption(t *testing.T) {
	ctx := context.Background()
	const secret = "JBSWY3DPEHPK3PXP"
	path := filepath.Join(t.TempDir(), "a.db")
	plain := openFile(t, path)
	u, err := plain.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	confirmedTOTP(t, plain, u.ID, secret, 10)
	if raw := rawColumn(t, plain, fmt.Sprintf(`SELECT secret FROM user_totp WHERE user_id = %d`, u.ID)); raw != secret {
		t.Fatalf("setup: %q", raw)
	}
	plain.Close()

	s := openFile(t, path, WithCipher(cipherWith(t, 1)))
	raw := rawColumn(t, s, fmt.Sprintf(`SELECT secret FROM user_totp WHERE user_id = %d`, u.ID))
	if !strings.HasPrefix(raw, encPrefix) || strings.Contains(raw, secret) {
		t.Fatalf("secret not encrypted on open: %q", raw)
	}
	got, err := s.GetTOTP(ctx, u.ID)
	if err != nil || got.Secret != secret || !got.Confirmed {
		t.Fatalf("after encryption: %+v %v", got, err)
	}
}

func TestCopyKeepsTwoFactor(t *testing.T) {
	ctx := context.Background()
	const secret = "JBSWY3DPEHPK3PXP"
	dir := t.TempDir()
	src := openFile(t, filepath.Join(dir, "src.db"), WithCipher(testCipher(t, 1)))
	u, err := src.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	confirmedTOTP(t, src, u.ID, secret, 77)
	if err := src.ReplaceRecoveryCodes(ctx, u.ID, []string{"r1", "r2", "r3"}); err != nil {
		t.Fatal(err)
	}

	dst := openFile(t, filepath.Join(dir, "dst.db"), WithCipher(testCipher(t, 1)))
	if _, err := Copy(ctx, src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := dst.GetTOTP(ctx, u.ID)
	if err != nil || got.Secret != secret || !got.Confirmed || got.LastStep != 77 {
		t.Fatalf("copied setup: %+v %v", got, err)
	}
	if n, err := dst.CountRecoveryCodes(ctx, u.ID); err != nil || n != 3 {
		t.Fatalf("copied recovery codes: %d %v", n, err)
	}
}

func TestTwoFactorDeletedWithUser(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.CreateUser(ctx, "admin"); err != nil { // the first user is the admin; keep one
		t.Fatal(err)
	}
	b, err := s.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	confirmedTOTP(t, s, b.ID, "SECRET_B", 5)
	if err := s.ReplaceRecoveryCodes(ctx, b.ID, []string{"r1", "r2"}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteUser(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTOTP(ctx, b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("setup after delete: %v", err)
	}
	if n, err := s.CountRecoveryCodes(ctx, b.ID); err != nil || n != 0 {
		t.Fatalf("recovery codes after delete: %d %v", n, err)
	}
}
