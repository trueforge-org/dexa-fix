package mainbackend

import (
	"strings"
	"testing"
	"time"
)

// sealedServer returns a Server with its own cookie key, for the sealing functions.
func sealedServer(t *testing.T) *Server {
	t.Helper()
	ts, _, _ := newAuthServer(t)
	return serverOf(ts)
}

type sealedThing struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// flipFirst changes the first character of s to another base64 character.
func flipFirst(s string) string {
	if s[0] == 'A' {
		return "B" + s[1:]
	}
	return "A" + s[1:]
}

// flipLast changes the last character of s to another base64 character.
func flipLast(s string) string {
	last := len(s) - 1
	if s[last] == 'A' {
		return s[:last] + "B"
	}
	return s[:last] + "A"
}

func TestSealedValueRoundTrip(t *testing.T) {
	s := sealedServer(t)
	want := sealedThing{Name: "dexa", Count: 3}
	sealed, nonce, err := s.sealValue("a", want, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if nonce == "" {
		t.Fatal("empty nonce")
	}
	var got sealedThing
	gotNonce, expires, ok := s.openValue("a", sealed, &got)
	if !ok {
		t.Fatal("value sealed for \"a\" does not open with \"a\"")
	}
	if got != want {
		t.Fatalf("opened %+v, want %+v", got, want)
	}
	if nonce == "" || !expires.After(time.Now()) {
		t.Fatalf("nonce %q, expires %v: want a nonce to spend and an expiry ahead", nonce, expires)
	}
	if gotNonce != nonce {
		t.Fatalf("nonce %q, want %q", gotNonce, nonce)
	}
	if !expires.After(time.Now()) {
		t.Fatalf("expiry %s is not in the future", expires)
	}
}

func TestSealedValueRejectsOtherPurposeTamperingAndExpiry(t *testing.T) {
	s := sealedServer(t)
	sealed, _, err := s.sealValue("a", sealedThing{Name: "dexa", Count: 1}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	payload, mac, found := strings.Cut(sealed, ".")
	if !found {
		t.Fatalf("sealed value has no separator: %q", sealed)
	}
	var out sealedThing

	if _, _, ok := s.openValue("b", sealed, &out); ok {
		t.Error("value sealed for \"a\" opened as \"b\"")
	}
	if _, _, ok := s.openValue("a", flipFirst(payload)+"."+mac, &out); ok {
		t.Error("payload with a changed character opened")
	}
	if _, _, ok := s.openValue("a", payload+"."+flipLast(mac), &out); ok {
		t.Error("MAC with a changed character opened")
	}
	for _, garbage := range []string{"", "garbage", "no-separator", ".", "a.b", "!!!.???"} {
		if _, _, ok := s.openValue("a", garbage, &out); ok {
			t.Errorf("garbage %q opened", garbage)
		}
	}

	expired, _, err := s.sealValue("a", sealedThing{Name: "old"}, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.openValue("a", expired, &out); ok {
		t.Error("expired value opened")
	}
}

func TestProviderStateStillWorks(t *testing.T) {
	s := sealedServer(t)
	want := providerState{Slug: "idp", State: "state-1", Verifier: "verifier-1", Nonce: "nonce-1", Link: 3}

	sealed := s.sealState(want)
	got, nonce, expires, ok := s.openState(sealed)
	if !ok {
		t.Fatal("provider state does not open")
	}
	if nonce == "" || !expires.After(time.Now()) {
		t.Fatalf("nonce %q, expires %v: want a nonce to spend and an expiry ahead", nonce, expires)
	}
	if got != want {
		t.Fatalf("opened %+v, want %+v", got, want)
	}

	other, _, err := s.sealValue("other-purpose", want, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := s.openState(other); ok {
		t.Fatal("value sealed for another purpose opened as provider state")
	}
}
