package frontend

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
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
	defaultHTTPSPort = "8443"

	h3MaxAge = 24 * time.Hour
)

// tlsConfig is the configuration for TCP connections; the certificate is looked up per handshake,
// so a new one applies at once.
func (s *Frontend) tlsConfig() *tls.Config {
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

// ---- the HTTPS and HTTP/3 servers ----

// StartListeners starts the HTTPS and HTTP/3 servers when the settings ask for them, with handler
// as their handler (the one the plain server uses). It is called again by reloadSettings whenever a
// setting changes, which starts, moves or stops them without a restart.
func (s *Frontend) StartListeners(handler http.Handler) {
	s.lmu.Lock()
	s.handler = handler
	s.lmu.Unlock()
	s.applyListeners()
}

// bindAddr is the listen address with another port: same interface as the plain server.
func (s *Frontend) bindAddr(port string) string {
	host, _, err := net.SplitHostPort(s.listenAddr)
	if err != nil {
		host = ""
	}
	return net.JoinHostPort(host, port)
}

func (s *Frontend) applyListeners() {
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

func (s *Frontend) startHTTPS(port string) {
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

func (s *Frontend) startHTTP3(port string) {
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
func (s *Frontend) closeListeners() {
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
func (s *Frontend) listenError() string {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	return s.bindErr
}

// altSvc tells clients that arrive over TLS that HTTP/3 is available on the UDP port with the same
// number as the one they used.
func (s *Frontend) altSvc(next http.Handler) http.Handler {
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
