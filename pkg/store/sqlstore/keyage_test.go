package sqlstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// ageKeyCheck moves the crypto.check row's updated_at back by age, as if the key had been
// introduced that long ago. It returns the moved time.
func ageKeyCheck(t *testing.T, s *Store, age time.Duration) time.Time {
	t.Helper()
	when := now().Add(-age)
	if _, err := s.db.ExecContext(context.Background(), `UPDATE app_settings SET updated_at = ? WHERE name = ?`, when, checkSetting); err != nil {
		t.Fatal(err)
	}
	return when
}

// keyDateWithin fails unless got is within a minute of want.
func keyDateWithin(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if d := got.Sub(want); d < -time.Minute || d > time.Minute {
		t.Fatalf("%s: got %s, want about %s", what, got, want)
	}
}

func TestKeyAgeKeptWhenRekeyedWithSameKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a.db")
	s := openFile(t, path, WithCipher(cipherWith(t, 1)))
	fillSensitive(t, s)
	aged := ageKeyCheck(t, s, 90*24*time.Hour)

	if _, err := s.Rekey(ctx, false); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Same key again: the check value is unchanged, so the date stays where it was.
	keyDateWithin(t, "KeySince", st.KeySince, aged)
	if st.KeyID != cipherWith(t, 1).current.id {
		t.Fatalf("KeyID: %q", st.KeyID)
	}
	if st.KeyPrevious {
		t.Fatal("KeyPrevious set for the current key")
	}
}

func TestKeyAgeMovesOnNewKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a.db")
	s := openFile(t, path, WithCipher(cipherWith(t, 1)))
	fillSensitive(t, s)
	ageKeyCheck(t, s, 90*24*time.Hour)
	s.Close()

	// Key B becomes current, key A stays readable as previous.
	s = openFile(t, path, WithCipher(cipherWith(t, 2, 1)))
	if _, err := s.Rekey(ctx, false); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A new key is introduced now, so its date is recent.
	keyDateWithin(t, "KeySince", st.KeySince, now())
	if st.KeyID != cipherWith(t, 2).current.id {
		t.Fatalf("KeyID: %q", st.KeyID)
	}
	if st.KeyPrevious {
		t.Fatal("KeyPrevious set after rekey")
	}
}

func TestStatsKeyPrevious(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a.db")
	s := openFile(t, path, WithCipher(cipherWith(t, 1)))
	fillSensitive(t, s)
	s.Close()

	// Data is still under key A; B is current and A only reads.
	s = openFile(t, path, WithCipher(cipherWith(t, 2, 1)))
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.KeyPrevious {
		t.Fatal("KeyPrevious not set before rekey")
	}
	if !st.KeySince.IsZero() {
		t.Fatalf("KeySince before rekey: %s", st.KeySince)
	}
	if st.KeyID != cipherWith(t, 2).current.id {
		t.Fatalf("KeyID: %q", st.KeyID)
	}

	if _, err := s.Rekey(ctx, false); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	st, err = s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.KeyPrevious {
		t.Fatal("KeyPrevious still set after rekey")
	}
	if st.KeySince.IsZero() {
		t.Fatal("KeySince zero after rekey")
	}
}

func TestStatsKeyWithoutEncryption(t *testing.T) {
	ctx := context.Background()
	s := openFile(t, filepath.Join(t.TempDir(), "a.db"))
	fillSensitive(t, s)
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.KeyID != "" {
		t.Fatalf("KeyID: %q", st.KeyID)
	}
	if !st.KeySince.IsZero() {
		t.Fatalf("KeySince: %s", st.KeySince)
	}
	if st.KeyPrevious {
		t.Fatal("KeyPrevious set without encryption")
	}
}

func TestCopyKeepsKeyDate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := openFile(t, filepath.Join(dir, "src.db"), WithCipher(cipherWith(t, 1)))
	fillSensitive(t, src)
	ageKeyCheck(t, src, 90*24*time.Hour)
	srcStats, err := src.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	dst := openFile(t, filepath.Join(dir, "dst.db"), WithCipher(cipherWith(t, 1)))
	if _, err := Copy(ctx, src, dst); err != nil {
		t.Fatalf("copy: %v", err)
	}
	dstStats, err := dst.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dstStats.KeySince.Unix() != srcStats.KeySince.Unix() {
		t.Fatalf("KeySince: target %s, source %s", dstStats.KeySince, srcStats.KeySince)
	}
}
