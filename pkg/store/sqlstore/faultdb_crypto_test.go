package sqlstore

import (
	"bytes"
	"context"
	"testing"
)

// keyedSeed is seedFaultData for a store that has an encryption key, with the key checks written
// the way Open writes them.
func keyedSeed(t *testing.T, s *Store) faultData {
	t.Helper()
	fx := seedFaultData(t, s)
	ctx := context.Background()
	for _, verify := range []func(context.Context) (func() error, error){s.verifyKey, s.BodyStore.verifyKey} {
		write, err := verify(ctx)
		if err != nil || write == nil {
			t.Fatalf("seed: key check: %v %v", write != nil, err)
		}
		if err := write(); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

// withKeys gives the store a new key that opens the old one's data too, as after `db rekey` is
// prepared with the old key kept as a previous one.
func withKeys(t *testing.T, s *Store, current byte, previous ...byte) {
	t.Helper()
	var prev [][]byte
	for _, p := range previous {
		prev = append(prev, bytes.Repeat([]byte{p}, KeySize))
	}
	c, err := NewCipher(bytes.Repeat([]byte{current}, KeySize), prev...)
	if err != nil {
		t.Fatal(err)
	}
	s.cipher = c
	s.BodyStore.cipher = c.bodyCipher()
}

// Every statement of the key handling is failed once, on a store that has a key.
func TestKeyHandlingReportsADatabaseFailure(t *testing.T) {
	keyed := []faultOp{
		{"Rekey with a new key", func(c context.Context, s *Store, f faultData) error {
			withKeys(t, s, 2, 1)
			return e1(s.Rekey(c, false))
		}},
		{"Rekey as a dry run", func(c context.Context, s *Store, f faultData) error {
			withKeys(t, s, 2, 1)
			return e1(s.Rekey(c, true))
		}},
		{"Rekey with the key the data already has", func(c context.Context, s *Store, f faultData) error {
			return e1(s.Rekey(c, false))
		}},
		{"Stats of a store with a key", func(c context.Context, s *Store, f faultData) error { return e1(s.Stats(c)) }},
		{"verifyKey before the check is written", func(c context.Context, s *Store, f faultData) error {
			if err := s.ExecForTests(c, `DELETE FROM app_settings WHERE name = ?`, checkSetting); err != nil {
				return err
			}
			write, err := s.verifyKey(c)
			if err != nil {
				return err
			}
			return write()
		}},
		{"verifyKey with the check written", func(c context.Context, s *Store, f faultData) error { return e1(s.verifyKey(c)) }},
		{"body verifyKey with the check written", func(c context.Context, s *Store, f faultData) error { return e1(s.BodyStore.verifyKey(c)) }},
		{"body verifyKey before the check is written", func(c context.Context, s *Store, f faultData) error {
			if err := s.ExecForTests(c, `DELETE FROM body_meta`); err != nil {
				return err
			}
			write, err := s.BodyStore.verifyKey(c)
			if err != nil {
				return err
			}
			return write()
		}},
		{"HasEncryptedValues of the body data", func(c context.Context, s *Store, f faultData) error { return e1(s.BodyStore.HasEncryptedValues(c)) }},
		{"the first encrypted value when only a provider secret is encrypted", func(c context.Context, s *Store, f faultData) error {
			for _, q := range []string{`DELETE FROM user_integrations`, `DELETE FROM user_totp`, `UPDATE users SET email = '', email_lookup = NULL`} {
				if err := s.ExecForTests(c, q); err != nil {
					return err
				}
			}
			return e1(firstEncrypted(c, s.db))
		}},
		{"the first encrypted body row when only a scan is encrypted", func(c context.Context, s *Store, f faultData) error {
			if err := s.ExecForTests(c, `DELETE FROM scale_readings`); err != nil {
				return err
			}
			return e1(firstEncryptedBody(c, s.db))
		}},
		{"body Rekey", func(c context.Context, s *Store, f faultData) error {
			withKeys(t, s, 2, 1)
			return e1(s.BodyStore.Rekey(c, false))
		}},
	}
	for _, op := range keyed {
		t.Run(op.name, func(t *testing.T) {
			failEveryStatement(t, testCipher(t, 1), keyedSeed, op.do, faultAbsorbed[op.name]...)
		})
	}
}

// Rows written before a key was configured are encrypted when the store opens; every statement of that
// is failed once.
func TestEncryptingExistingRowsReportsADatabaseFailure(t *testing.T) {
	both := func(c context.Context, s *Store, f faultData) error {
		withKeys(t, s, 1)
		if err := s.encryptExisting(c); err != nil {
			return err
		}
		return s.BodyStore.encryptExisting(c)
	}
	t.Run("the main database and the readings", func(t *testing.T) {
		failEveryStatement(t, nil, func(t *testing.T, s *Store) faultData {
			fx := seedFaultData(t, s)
			// A finished job with a result in plain text, and a user whose address is not indexed yet.
			if err := s.ExecForTests(context.Background(), `UPDATE worker_jobs SET result = '{"a":1}' WHERE state = 'succeeded'`); err != nil {
				t.Fatal(err)
			}
			return fx
		}, both)
	})
	t.Run("the body database on its own", func(t *testing.T) {
		failEveryStatement(t, nil, seedFaultData, func(c context.Context, s *Store, f faultData) error {
			withKeys(t, s, 1)
			return s.BodyStore.encryptExisting(c)
		})
	})
}

// The body store of a body-only database has its own copies of the schema calls.
func TestBodyStoreReportsADatabaseFailure(t *testing.T) {
	ops := []faultOp{
		{"Ping", func(c context.Context, s *Store, f faultData) error { return s.BodyStore.Ping(c) }},
		{"SchemaVersion", func(c context.Context, s *Store, f faultData) error { return e1(s.BodyStore.SchemaVersion(c)) }},
		{"HasPending", func(c context.Context, s *Store, f faultData) error { return e1(s.BodyStore.HasPending(c)) }},
		{"Migrate", func(c context.Context, s *Store, f faultData) error { return e1(s.BodyStore.Migrate(c)) }},
	}
	absorbed := map[string][]int{"SchemaVersion": {1}, "HasPending": {1}, "Migrate": {1}}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			failEveryStatement(t, nil, seedFaultData, op.do, absorbed[op.name]...)
		})
	}
}

func emptyStore(t *testing.T) *Store {
	s, _ := newFaultyStore(t, nil)
	return s
}

// Copy reports a failure of either database, whichever statement it is.
func TestCopyReportsADatabaseFailureOfEitherDatabase(t *testing.T) {
	t.Run("of the source", func(t *testing.T) {
		failEveryStatement(t, nil, func(t *testing.T, s *Store) *Store { seedFaultData(t, s); return emptyStore(t) },
			func(c context.Context, s *Store, dst *Store) error { return e1(Copy(c, s, dst)) }, 1)
	})
	t.Run("of the target", func(t *testing.T) {
		failEveryStatement(t, nil, func(t *testing.T, s *Store) *Store { src := emptyStore(t); seedFaultData(t, src); return src },
			func(c context.Context, dst *Store, src *Store) error { return e1(Copy(c, src, dst)) }, 1)
	})
}
