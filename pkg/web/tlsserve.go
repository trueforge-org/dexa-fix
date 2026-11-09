package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/rs/zerolog/log"
)

// HTTP/2 and HTTP/3.
//
// The plain HTTP port (--listen) speaks HTTP/1.1 and, for proxies that use it, cleartext HTTP/2
// (h2c). TLS is optional and an Admin setting, served on its own port: with a certificate (uploaded,
// or generated and self-signed) the server also listens for HTTPS there, with HTTP/2 and HTTP/1.1
// negotiated through ALPN, and for HTTP/3 over QUIC on the UDP port with the same number. The two
// ports are separate, so a wrong certificate or port can never lock the administrator out of the
// plain one. HTTP/3 is advertised to HTTPS clients with an Alt-Svc header. Browsers only use it
// with a certificate they trust: a self-signed certificate suits tests and private networks where
// it is trusted explicitly.
const (
	tlsOff        = "off"
	tlsUpload     = "upload"
	tlsSelfSigned = "selfsigned"

	settingTLSMode  = "tls.mode"
	settingTLSCert  = "tls.cert"  // PEM chain, stored encrypted with the other settings
	settingTLSKey   = "tls.key"   // PEM private key, stored encrypted
	settingTLSNames = "tls.names" // names for a self-signed certificate
	settingHTTP3    = "http3.enabled"
	settingHTTPS    = "tls.port" // TCP port for HTTPS; HTTP/3 uses the same number on UDP

	defaultHTTPSPort = "8443"

	selfSignedDays  = 365
	selfSignedRenew = 30 * 24 * time.Hour // regenerate when less than this is left
	h3MaxAge        = 24 * time.Hour
)

// tlsInfo describes the certificate in force, for the Admin page.
type tlsInfo struct {
	Subject     string
	Names       []string
	NotAfter    time.Time
	Fingerprint string // SHA-256 of the certificate
}

// tlsConfig is the configuration for TCP connections; the certificate is looked up per handshake,
// so a new one applies at once.
func (s *Server) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c := s.cfg().tlsCert
			if c == nil {
				return nil, errors.New("no certificate is configured")
			}
			return c, nil
		},
	}
}

// loadCertificate reads the certificate for the stored mode. It returns nil, with a warning, when
// the stored material is unusable: the server then serves plain HTTP only, which keeps the Admin
// page reachable.
func (s *Server) loadCertificate(ctx context.Context, mode string, base *url.URL) (*tls.Certificate, *tlsInfo) {
	switch mode {
	case tlsUpload:
		certPEM, err1 := s.settingValue(ctx, settingTLSCert)
		keyPEM, err2 := s.settingValue(ctx, settingTLSKey)
		if err1 != nil || err2 != nil {
			log.Warn().Msg("could not read the stored TLS certificate")
			return nil, nil
		}
		c, info, err := parseCertificate(certPEM, keyPEM)
		if err != nil {
			log.Warn().Err(err).Msg("ignoring the stored TLS certificate")
			return nil, nil
		}
		return c, info
	case tlsSelfSigned:
		c, info, err := s.selfSigned(ctx, base)
		if err != nil {
			log.Warn().Err(err).Msg("could not prepare the self-signed TLS certificate")
			return nil, nil
		}
		return c, info
	}
	return nil, nil
}

// parseCertificate checks a PEM certificate chain and its private key.
func parseCertificate(certPEM, keyPEM string) (*tls.Certificate, *tlsInfo, error) {
	if strings.TrimSpace(certPEM) == "" || strings.TrimSpace(keyPEM) == "" {
		return nil, nil, errors.New("both a certificate and a private key are needed")
	}
	c, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, nil, fmt.Errorf("the certificate and key do not belong together or are not PEM: %w", err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("certificate: %w", err)
	}
	c.Leaf = leaf
	if time.Now().After(leaf.NotAfter) {
		return nil, nil, fmt.Errorf("the certificate expired on %s", leaf.NotAfter.UTC().Format("2006-01-02"))
	}
	sum := sha256.Sum256(leaf.Raw)
	names := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	return &c, &tlsInfo{Subject: leaf.Subject.String(), Names: names, NotAfter: leaf.NotAfter, Fingerprint: hex.EncodeToString(sum[:])}, nil
}

// selfSignedNames are the names the generated certificate covers: those the administrator gave,
// otherwise the base URL's host and the local names.
func (s *Server) selfSignedNames(ctx context.Context, base *url.URL) []string {
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
func (s *Server) selfSigned(ctx context.Context, base *url.URL) (*tls.Certificate, *tlsInfo, error) {
	names := s.selfSignedNames(ctx, base)
	certPEM, _ := s.settingValue(ctx, settingTLSCert)
	keyPEM, _ := s.settingValue(ctx, settingTLSKey)
	if c, info, err := parseCertificate(certPEM, keyPEM); err == nil && time.Until(info.NotAfter) > selfSignedRenew && sameNames(info.Names, names) {
		return c, info, nil
	}
	certPEM, keyPEM, err := generateSelfSigned(names)
	if err != nil {
		return nil, nil, err
	}
	if err := s.store.PutSetting(ctx, settingTLSCert, certPEM); err != nil {
		return nil, nil, err
	}
	if err := s.store.PutSetting(ctx, settingTLSKey, keyPEM); err != nil {
		return nil, nil, err
	}
	log.Info().Strs("names", names).Msg("generated a self-signed TLS certificate")
	return parseCertificate(certPEM, keyPEM)
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
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", "", err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0], Organization: []string{"dexa-fix (self-signed)"}},
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
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), nil
}

// renewCertificate regenerates a self-signed certificate that is nearly expired. It runs from the
// maintenance loop.
func (s *Server) renewCertificate(ctx context.Context) {
	l := s.cfg()
	if l.tlsMode != tlsSelfSigned || l.tlsInfo == nil || time.Until(l.tlsInfo.NotAfter) > selfSignedRenew {
		return
	}
	if err := s.reloadSettings(ctx); err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("renew the self-signed certificate")
	}
}

// ---- the HTTPS and HTTP/3 servers ----

// StartListeners starts the HTTPS and HTTP/3 servers when the settings ask for them, with handler
// as their handler (the one the plain server uses). It is called again by reloadSettings whenever a
// setting changes, which starts, moves or stops them without a restart.
func (s *Server) StartListeners(handler http.Handler) {
	s.lmu.Lock()
	s.handler = handler
	s.lmu.Unlock()
	s.applyListeners()
}

// bindAddr is the listen address with another port: same interface as the plain server.
func (s *Server) bindAddr(port string) string {
	host, _, err := net.SplitHostPort(s.listenAddr)
	if err != nil {
		host = ""
	}
	return net.JoinHostPort(host, port)
}

func (s *Server) applyListeners() {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	if s.handler == nil || s.listenAddr == "" {
		return
	}
	l := s.cfg()
	port := l.httpsPort
	wantTLS := l.tlsCert != nil
	want3 := wantTLS && l.http3

	if s.https != nil && (!wantTLS || s.httpsPort != port) {
		_ = s.https.Close()
		s.https = nil
		log.Info().Msg("HTTPS is off")
	}
	if s.h3 != nil && (!want3 || s.h3port != port) {
		_ = s.h3.Close()
		s.h3 = nil
		log.Info().Msg("HTTP/3 is off")
	}
	s.bindErr = ""
	if wantTLS && s.https == nil {
		s.startHTTPS(port)
	}
	if want3 && s.h3 == nil {
		s.startHTTP3(port)
	}
}

func (s *Server) startHTTPS(port string) {
	addr := s.bindAddr(port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.bindErr = "HTTPS could not listen on " + addr + ": " + err.Error()
		log.Error().Err(err).Str("addr", addr).Msg("HTTPS could not start: is the port free?")
		return
	}
	srv := &http.Server{
		Addr: addr, Handler: s.handler, TLSConfig: s.tlsConfig(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
	}
	s.https, s.httpsPort = srv, port
	go func() {
		// The certificate comes from TLSConfig.GetCertificate, so no files are named here.
		if err := srv.ServeTLS(s.proxyListener(ln), "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Str("addr", addr).Msg("HTTPS stopped")
		}
	}()
	log.Info().Str("addr", addr).Msg("HTTPS is on (HTTP/2 and HTTP/1.1)")
}

func (s *Server) startHTTP3(port string) {
	addr := s.bindAddr(port)
	srv := &http3.Server{
		Addr: addr, Handler: s.handler, TLSConfig: http3.ConfigureTLSConfig(s.tlsConfig()),
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
	}
	s.h3, s.h3port = srv, port
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Str("addr", addr).Msg("HTTP/3 stopped: is the UDP port free and reachable?")
		}
	}()
	log.Info().Str("udp", addr).Msg("HTTP/3 is on")
}

// closeListeners stops the HTTPS and HTTP/3 servers.
func (s *Server) closeListeners() {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	if s.https != nil {
		_ = s.https.Close()
		s.https = nil
	}
	if s.h3 != nil {
		_ = s.h3.Close()
		s.h3 = nil
	}
}

// listenError is the reason HTTPS could not start, for the Admin page; empty when all is well.
func (s *Server) listenError() string {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	return s.bindErr
}

// altSvc tells clients that arrive over TLS that HTTP/3 is available on the UDP port with the same
// number as the one they used.
func (s *Server) altSvc(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l := s.cfg(); r.TLS != nil && l.http3 {
			port := l.httpsPort
			if _, p, err := net.SplitHostPort(r.Host); err == nil {
				port = p // the port the client used, which may differ behind a port mapping
			}
			w.Header().Set("Alt-Svc", fmt.Sprintf(`h3=":%s"; ma=%d`, port, int(h3MaxAge.Seconds())))
		}
		next.ServeHTTP(w, r)
	})
}
