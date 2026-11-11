package mainbackend

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestASealedValueIsRefusedWhenItsSignedContentIsNotWhatItClaims(t *testing.T) {
	b := newRig(t).b
	signed := func(payload string) string { return payload + "." + b.sealMAC("p", payload) }
	enc := base64.RawURLEncoding.EncodeToString

	t.Run("a value that cannot be written as JSON is not sealed", func(t *testing.T) {
		if sealed, _, err := b.sealValue("p", make(chan int), time.Minute); err == nil || sealed != "" {
			t.Fatalf("sealed %q, err %v", sealed, err)
		}
	})
	t.Run("a payload that is not base64, though signed", func(t *testing.T) {
		var v sealedThing
		if _, _, ok := b.openValue("p", signed("!!not base64!!"), &v); ok {
			t.Fatal("opened")
		}
	})
	t.Run("a payload that is not an envelope, though signed", func(t *testing.T) {
		var v sealedThing
		if _, _, ok := b.openValue("p", signed(enc([]byte("not json"))), &v); ok {
			t.Fatal("opened")
		}
	})
	t.Run("an envelope whose value does not fit the target, though signed", func(t *testing.T) {
		sealed, _, err := b.sealValue("p", "a plain string", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		var v sealedThing
		if _, _, ok := b.openValue("p", sealed, &v); ok {
			t.Fatal("a string opened as a struct")
		}
	})
}
