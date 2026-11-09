package backend

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// sealed values are short-lived, signed blobs that travel through the browser (a cookie or a form
// field) so that the server needs no storage for a flow that has not signed anyone in yet: the
// sign-in through a provider, the second step of a sign-in, a passkey ceremony.
//
// The signature covers the purpose as well as the payload, so a value made for one flow cannot be
// presented to another. Every value carries an expiry and a random nonce; a flow that must work
// only once records the nonce in usedNonces.

// sealedEnvelope is what is signed.
type sealedEnvelope struct {
	Expires int64           `json:"e"`
	Nonce   string          `json:"n"`
	Value   json.RawMessage `json:"v"`
}

// sealValue signs v for the given purpose; it is valid for ttl. The nonce is returned so that the
// caller can keep it for single use.
func (s *Backend) sealValue(purpose string, v any, ttl time.Duration) (sealed, nonce string, err error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", "", err
	}
	if nonce, err = randomHex(12); err != nil {
		return "", "", err
	}
	env, err := json.Marshal(sealedEnvelope{Expires: time.Now().Add(ttl).Unix(), Nonce: nonce, Value: raw})
	if err != nil {
		return "", "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(env)
	return payload + "." + s.sealMAC(purpose, payload), nonce, nil
}

// openValue verifies a value made by sealValue for the same purpose and decodes it into v. It
// returns the nonce and the expiry, and false for anything forged, made for another purpose,
// malformed or expired.
func (s *Backend) openValue(purpose, sealed string, v any) (nonce string, expires time.Time, ok bool) {
	payload, mac, found := strings.Cut(sealed, ".")
	if !found || !hmac.Equal([]byte(mac), []byte(s.sealMAC(purpose, payload))) {
		return "", time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", time.Time{}, false
	}
	var env sealedEnvelope
	if json.Unmarshal(raw, &env) != nil || time.Now().Unix() > env.Expires {
		return "", time.Time{}, false
	}
	if json.Unmarshal(env.Value, v) != nil {
		return "", time.Time{}, false
	}
	return env.Nonce, time.Unix(env.Expires, 0), true
}

func (s *Backend) sealMAC(purpose, payload string) string {
	m := hmac.New(sha256.New, s.cookieKey)
	m.Write([]byte(purpose))
	m.Write([]byte{0})
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
