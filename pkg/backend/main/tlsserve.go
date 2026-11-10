package mainbackend

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/must"
	"github.com/trueforge-org/dexafix/pkg/shared/tlsutil"
)

// loadCertificate reads the certificate for the stored mode. It returns nil, with a warning, when
// the stored material is unusable: the server then serves plain HTTP only, which keeps the Admin
// page reachable.
func (s *Backend) loadCertificate(ctx context.Context, mode string, base *url.URL, generate bool) (*tls.Certificate, *tlsutil.Info) {
	switch mode {
	case apiclient.TLSUpload:
		certPEM, err1 := s.settingValue(ctx, settingTLSCert)
		keyPEM, err2 := s.settingValue(ctx, settingTLSKey)
		if err1 != nil || err2 != nil {
			log.Warn().Msg("could not read the stored TLS certificate")
			return nil, nil
		}
		c, info, err := tlsutil.Parse(certPEM, keyPEM)
		if err != nil {
			log.Warn().Err(err).Msg("ignoring the stored TLS certificate")
			return nil, nil
		}
		return c, info
	case apiclient.TLSSelfSigned:
		if !generate { // another backend makes the certificate; use what it stored
			return s.storedCertificate(ctx)
		}
		c, info, err := s.selfSigned(ctx, base)
		if err != nil {
			log.Warn().Err(err).Msg("could not prepare the self-signed TLS certificate")
			return nil, nil
		}
		return c, info
	}
	return nil, nil
}

// storedCertificate loads the stored certificate without ever generating one. It returns nil when
// there is none yet, or it is unusable.
func (s *Backend) storedCertificate(ctx context.Context) (*tls.Certificate, *tlsutil.Info) {
	certPEM, err1 := s.settingValue(ctx, settingTLSCert)
	keyPEM, err2 := s.settingValue(ctx, settingTLSKey)
	if err1 != nil || err2 != nil || certPEM == "" || keyPEM == "" {
		return nil, nil
	}
	c, info, err := tlsutil.Parse(certPEM, keyPEM)
	if err != nil {
		log.Warn().Err(err).Msg("ignoring the stored TLS certificate")
		return nil, nil
	}
	return c, info
}

// selfSignedNames are the names the generated certificate covers: those the administrator gave,
// otherwise the base URL's host and the local names.
func (s *Backend) selfSignedNames(ctx context.Context, base *url.URL) []string {
	var names []string
	if raw, _ := s.settingValue(ctx, settingTLSNames); strings.TrimSpace(raw) != "" {
		for _, n := range strings.Split(raw, ",") {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
		return names
	}
	if base != nil && base.Hostname() != "" {
		names = append(names, base.Hostname())
	}
	return append(names, "localhost", "127.0.0.1", "::1")
}

// selfSigned returns the stored self-signed certificate, generating a new one when there is none,
// when it is nearly expired or when the names changed.
func (s *Backend) selfSigned(ctx context.Context, base *url.URL) (*tls.Certificate, *tlsutil.Info, error) {
	names := s.selfSignedNames(ctx, base)
	certPEM, _ := s.settingValue(ctx, settingTLSCert)
	keyPEM, _ := s.settingValue(ctx, settingTLSKey)
	if c, info, err := tlsutil.Parse(certPEM, keyPEM); err == nil && time.Until(info.NotAfter) > selfSignedRenew && sameNames(info.Names, names) {
		return c, info, nil
	}
	certPEM, keyPEM = must.Value2(generateSelfSigned(names)) // the names are never none: localhost is always among them
	if err := s.store.PutSetting(ctx, settingTLSCert, certPEM); err != nil {
		return nil, nil, err
	}
	if err := s.store.PutSetting(ctx, settingTLSKey, keyPEM); err != nil {
		return nil, nil, err
	}
	log.Info().Strs("names", names).Msg("generated a self-signed TLS certificate")
	return tlsutil.Parse(certPEM, keyPEM)
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, x := range a {
		seen[strings.ToLower(x)] = true
	}
	for _, x := range b {
		if !seen[strings.ToLower(x)] {
			return false
		}
	}
	return true
}

// generateSelfSigned makes an ECDSA P-256 certificate for the given DNS names and IP addresses.
func generateSelfSigned(names []string) (certPEM, keyPEM string, err error) {
	if len(names) == 0 {
		return "", "", errors.New("a self-signed certificate needs at least one name")
	}
	key := must.Value(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	serial := must.Value(rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127)))
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0], Organization: []string{"DexaFix (self-signed)"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(selfSignedDays * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, n)
		}
	}
	der := must.Value(x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key))
	keyDER := must.Value(x509.MarshalPKCS8PrivateKey(key))
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), nil
}

// renewCertificate makes sure a self-signed certificate exists and is not nearly expired: it
// generates one when there is none and renews one that is. It runs from the maintenance loop, and
// not at all on a backend that must not generate.
func (s *Backend) renewCertificate(ctx context.Context) {
	l := s.cfg()
	if s.noCertGen || l.tlsMode != apiclient.TLSSelfSigned {
		return
	}
	if l.tlsInfo != nil && time.Until(l.tlsInfo.NotAfter) > selfSignedRenew {
		return
	}
	if err := s.reload(ctx, true); err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("renew the self-signed certificate")
	}
}
