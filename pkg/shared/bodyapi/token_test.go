package bodyapi

import (
	"crypto/ed25519"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newPair(t *testing.T) (*Signer, ed25519.PublicKey) {
	t.Helper()
	_, pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return NewSigner(priv), pub
}

func TestTokenRoundTrip(t *testing.T) {
	s, pub := newPair(t)
	now := time.Now()
	h := 181.5
	params := &EstimateParams{Timezone: "Europe/Amsterdam", Ranking: []string{"dexa", "scale"}, AthleteDefault: true, HeightCm: &h}
	c, err := NewVerifier(pub).Verify(s.Sign(42, ScopeWrite, params, now), now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != 42 || c.Scope != ScopeWrite || c.Audience != Audience || c.Params == nil ||
		c.Params.Timezone != "Europe/Amsterdam" || !c.Params.AthleteDefault || *c.Params.HeightCm != 181.5 || len(c.Params.Ranking) != 2 {
		t.Errorf("claims = %+v", c)
	}
}

func TestTokenRefusals(t *testing.T) {
	s, pub := newPair(t)
	other, otherPub := newPair(t)
	now := time.Now()
	v := NewVerifier(pub)
	good := s.Sign(1, ScopeRead, nil, now)

	for name, tc := range map[string]struct {
		token string
		at    time.Time
	}{
		"expired":                    {good, now.Add(MaxTokenAge + time.Minute)},
		"from the future":            {good, now.Add(-time.Minute)},
		"empty":                      {"", now},
		"no separator":               {strings.ReplaceAll(good, ".", ""), now},
		"signed by another key":      {other.Sign(1, ScopeRead, nil, now), now},
		"tampered subject":           {tamper(t, good, `"sub":1`, `"sub":2`), now},
		"tampered scope":             {tamper(t, good, `"scope":"read"`, `"scope":"admin"`), now},
		"garbage":                    {"abc.def", now},
		"signature of another token": {strings.SplitN(good, ".", 2)[0] + "." + strings.SplitN(s.Sign(2, ScopeRead, nil, now), ".", 2)[1], now},
	} {
		if _, err := v.Verify(tc.token, tc.at); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A key the verifier does not trust is refused even when it signs correctly; trusting it makes it work.
	if _, err := NewVerifier(otherPub).Verify(good, now); err == nil {
		t.Error("a token was accepted under a key that did not sign it")
	}
	if _, err := NewVerifier(pub, otherPub).Verify(other.Sign(1, ScopeRead, nil, now), now); err != nil {
		t.Errorf("two trusted keys: %v", err)
	}
}

// A token that is signed correctly but made to last long is refused: the signer is not trusted to
// choose the lifetime either, so a leaked token stops working soon.
func TestTokenWithLongLifetimeIsRefused(t *testing.T) {
	_, pub, priv, _ := GenerateKey()
	now := time.Now()
	long := NewSigner(priv)
	token := long.Sign(1, ScopeRead, nil, now.Add(time.Hour)) // exp is an hour and a minute away
	if _, err := NewVerifier(pub).Verify(token, now); err == nil {
		t.Error("a token that lives for over an hour was accepted")
	}
}

func TestWrongAudienceIsRefused(t *testing.T) {
	_, pub, priv, _ := GenerateKey()
	now := time.Now()
	s := NewSigner(priv)
	token := s.Sign(1, ScopeRead, nil, now)
	// Re-sign a payload that names another audience with the same trusted key.
	bad := signRaw(t, priv, `{"kid":"`+KeyID(pub)+`","sub":1,"aud":"somebody else","scope":"read","exp":`+itoa(now.Add(time.Minute).Unix())+`}`)
	if _, err := NewVerifier(pub).Verify(token, now); err != nil {
		t.Fatalf("control: %v", err)
	}
	if _, err := NewVerifier(pub).Verify(bad, now); err == nil {
		t.Error("a token for another audience was accepted")
	}
	noScope := signRaw(t, priv, `{"kid":"`+KeyID(pub)+`","sub":1,"aud":"`+Audience+`","scope":"root","exp":`+itoa(now.Add(time.Minute).Unix())+`}`)
	if _, err := NewVerifier(pub).Verify(noScope, now); err == nil {
		t.Error("an unknown scope was accepted")
	}
}

func TestScopeAllows(t *testing.T) {
	for _, tc := range []struct {
		have, need Scope
		ok         bool
	}{
		{ScopeRead, ScopeRead, true}, {ScopeRead, ScopeWrite, false}, {ScopeRead, ScopeAdmin, false},
		{ScopeWrite, ScopeRead, true}, {ScopeWrite, ScopeWrite, true}, {ScopeWrite, ScopeAdmin, false},
		{ScopeAdmin, ScopeRead, true}, {ScopeAdmin, ScopeWrite, true}, {ScopeAdmin, ScopeAdmin, true},
		{"", ScopeRead, false}, {"root", ScopeRead, false},
	} {
		if got := tc.have.Allows(tc.need); got != tc.ok {
			t.Errorf("%q allows %q = %v, want %v", tc.have, tc.need, got, tc.ok)
		}
	}
}

func TestErrorMatchesSentinels(t *testing.T) {
	e := error(&Error{Code: CodeNotFound, Message: "not found"})
	if !errors.Is(e, ErrNotFound) || errors.Is(e, ErrInvalid) || errors.Is(e, ErrUnavailable) {
		t.Error("errors.Is does not follow the code")
	}
	if StatusFor(CodeNotFound) != 404 || StatusFor(CodeUnavailable) != 503 || StatusFor("x") != 500 {
		t.Error("StatusFor")
	}
}

// tamper changes the payload of a token and keeps its signature.
func tamper(t *testing.T, token, from, to string) string {
	t.Helper()
	p, sig, _ := strings.Cut(token, ".")
	payload, err := b64.DecodeString(p)
	if err != nil || !strings.Contains(string(payload), from) {
		t.Fatalf("cannot tamper with %q: %v", payload, err)
	}
	return b64.EncodeToString([]byte(strings.Replace(string(payload), from, to, 1))) + "." + sig
}

// signRaw signs any payload with key, as a signer that is not this package's would.
func signRaw(t *testing.T, key ed25519.PrivateKey, payload string) string {
	t.Helper()
	return b64.EncodeToString([]byte(payload)) + "." + b64.EncodeToString(ed25519.Sign(key, []byte(payload)))
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestKeyFilesRoundTrip(t *testing.T) {
	_, pub, priv, _ := GenerateKey()
	got, err := ParsePrivateKey("# the signing key of the main backend\n" + EncodePrivateKey(priv) + "\n")
	if err != nil || !got.Equal(priv) {
		t.Fatalf("private key: %v", err)
	}
	_, pub2, _, _ := GenerateKey()
	pubs, err := ParsePublicKeys(EncodePublicKey(pub) + "\n\n# the next key\n" + EncodePublicKey(pub2) + "\n")
	if err != nil || len(pubs) != 2 || !pubs[0].Equal(pub) || !pubs[1].Equal(pub2) {
		t.Fatalf("public keys: %v", err)
	}
	// The public key made from the private one verifies its tokens.
	now := time.Now()
	if _, err := NewVerifier(pubs[0]).Verify(NewSigner(got).Sign(1, ScopeRead, nil, now), now); err != nil {
		t.Errorf("a token of the parsed key: %v", err)
	}
	for name, text := range map[string]string{"empty": "", "two keys": EncodePrivateKey(priv) + "\n" + EncodePrivateKey(priv), "no label": "AAAA", "damaged": privateLabel + "AAAA"} {
		if _, err := ParsePrivateKey(text); err == nil {
			t.Errorf("private key %s accepted", name)
		}
	}
	for name, text := range map[string]string{"empty": "# nothing\n", "no label": "AAAA", "not base64": publicLabel + "%%%", "short": publicLabel + "AAAA"} {
		if _, err := ParsePublicKeys(text); err == nil {
			t.Errorf("public key %s accepted", name)
		}
	}
	// A private key is refused where a public one belongs, even though both are 32 bytes, and the
	// other way round.
	if _, err := ParsePublicKeys(EncodePrivateKey(priv)); err == nil || !strings.Contains(err.Error(), "PRIVATE") {
		t.Errorf("a private key taken for a public one: %v", err)
	}
	if _, err := ParsePrivateKey(EncodePublicKey(pub)); err == nil {
		t.Error("a public key taken for a private one")
	}
}
