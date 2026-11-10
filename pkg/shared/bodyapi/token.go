package bodyapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/must"
)

// Claims are what a token says. They are signed, so the body service can rely on all of them.
type Claims struct {
	KeyID    string          `json:"kid"`
	Subject  int64           `json:"sub"` // the user; 0 for a call that is about no user in particular
	Audience string          `json:"aud"`
	Scope    Scope           `json:"scope"`
	Expires  int64           `json:"exp"` // Unix seconds
	Params   *EstimateParams `json:"params,omitempty"`
}

// tokenSkew is how far a clock may be off between the two services.
const tokenSkew = 5 * time.Second

var errToken = errors.New("invalid token")

var b64 = base64.RawURLEncoding

// GenerateKey makes a signing key pair. The key id names the public key, so several can be trusted
// side by side while one is being replaced.
func GenerateKey() (keyID string, pub ed25519.PublicKey, priv ed25519.PrivateKey) {
	pub, priv = must.Value2(ed25519.GenerateKey(rand.Reader))
	return KeyID(pub), pub, priv
}

// KeyID is the first eight hexadecimal characters of the SHA-256 of a public key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:8]
}

// Signer makes tokens. Only the main backend holds one.
type Signer struct {
	kid  string
	priv ed25519.PrivateKey
}

// NewSigner returns a Signer for a private key.
func NewSigner(priv ed25519.PrivateKey) *Signer {
	return &Signer{kid: KeyID(priv.Public().(ed25519.PublicKey)), priv: priv}
}

// Sign makes a token for subject and scope that expires MaxTokenAge after now.
func (s *Signer) Sign(subject int64, scope Scope, params *EstimateParams, now time.Time) string {
	payload, _ := json.Marshal(Claims{
		KeyID: s.kid, Subject: subject, Audience: Audience, Scope: scope,
		Expires: now.Add(MaxTokenAge).Unix(), Params: params,
	})
	return b64.EncodeToString(payload) + "." + b64.EncodeToString(ed25519.Sign(s.priv, payload))
}

// Verifier checks tokens. The body service holds one, with public keys only.
type Verifier struct {
	keys map[string]ed25519.PublicKey
}

// NewVerifier trusts the given public keys.
func NewVerifier(keys ...ed25519.PublicKey) *Verifier {
	v := &Verifier{keys: map[string]ed25519.PublicKey{}}
	for _, k := range keys {
		v.keys[KeyID(k)] = k
	}
	return v
}

// Verify returns the claims of a token that is signed by a trusted key, meant for this service,
// not expired and not made to last longer than MaxTokenAge. Every failure looks the same.
func (v *Verifier) Verify(token string, now time.Time) (Claims, error) {
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return Claims{}, errToken
	}
	payload, err := b64.DecodeString(p)
	if err != nil {
		return Claims{}, errToken
	}
	signature, err := b64.DecodeString(sig)
	if err != nil {
		return Claims{}, errToken
	}
	// The key id is read before the signature is checked; it only picks which key to check with.
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, errToken
	}
	key, ok := v.keys[c.KeyID]
	if !ok || !ed25519.Verify(key, payload, signature) {
		return Claims{}, errToken
	}
	exp := time.Unix(c.Expires, 0)
	switch {
	case c.Audience != Audience,
		now.After(exp.Add(tokenSkew)),
		exp.After(now.Add(MaxTokenAge + tokenSkew)),
		c.Scope.rank() == 0:
		return Claims{}, errToken
	}
	return c, nil
}
