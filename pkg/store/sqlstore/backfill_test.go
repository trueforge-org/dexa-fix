package sqlstore

import (
	"context"
	"testing"
)

// Starting with a key encrypts the addresses written before it; an address that no key opens stops
// that, instead of being dropped.
func TestEncryptingExistingAddressesStopsAtOneNoKeyOpens(t *testing.T) {
	ctx := context.Background()
	s, _ := newFaultyStore(t, testCipher(t, 1))
	keyedSeed(t, s)
	if err := s.ExecForTests(ctx, `UPDATE users SET email = '`+unknownKeyValue+`' WHERE role = 'user'`); err != nil {
		t.Fatal(err)
	}
	if err := s.encryptExisting(ctx); err == nil {
		t.Fatal("encryptExisting went on")
	}
}
