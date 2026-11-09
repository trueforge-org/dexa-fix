package bodybackend

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/trueforge-org/dexa-fix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexa-fix/pkg/shared/inproc"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// NewInProcess runs a body service over st inside this process and returns it with a client for
// it. The combined `serve` mode uses it, and so does a main backend that has no body service of its
// own to call. The client talks to the service through the same handler, headers and tokens as a
// separate process would, with a secret and a signing key made for this run only.
func NewInProcess(st store.BodyStore, opts ...Option) (*bodyapi.Client, *Service, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, nil, err
	}
	secret := hex.EncodeToString(raw)
	_, pub, priv, err := bodyapi.GenerateKey()
	if err != nil {
		return nil, nil, err
	}
	svc, err := New(st, secret, bodyapi.NewVerifier(pub), opts...)
	if err != nil {
		return nil, nil, err
	}
	c, err := bodyapi.New("http://body.internal", &http.Client{Transport: inproc.Transport(svc.Handler())}, secret, bodyapi.NewSigner(priv))
	if err != nil {
		return nil, nil, err
	}
	return c, svc, nil
}

// SetVersion names the running program, for services built before it is known (see WithVersion).
// Call it before the service takes requests.
func (s *Service) SetVersion(v string) { s.version = v }
