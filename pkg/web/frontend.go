package web

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
)

// The frontend half on its own: a process with no database that renders pages from what the
// backend's internal API answers, serves the static files, and owns the public listeners (plain,
// HTTPS and HTTP/3). The administrator settings it acts on come from the backend, see
// RefreshConfig.

// NewFrontend builds a frontend that talks to the backend through api. Call RefreshConfig before
// serving, and StartConfigPoll to follow changes.
func NewFrontend(api *apiclient.Client) (*Server, error) {
	s, err := newServer()
	if err != nil {
		return nil, err
	}
	s.api = api
	s.swapSettings(&liveSettings{tlsMode: tlsOff, httpsPort: defaultHTTPSPort})
	return s, nil
}

// isFrontendOnly reports whether this process has no database of its own.
func (s *Server) isFrontendOnly() bool { return s.store == nil }

// RefreshConfig fetches the settings from the backend and puts them in force if they changed.
func (s *Server) RefreshConfig(ctx context.Context) error {
	cfg, etag, err := s.api.FrontendConfig(ctx, s.configETag)
	if err != nil {
		return err
	}
	if cfg == nil { // not modified
		return nil
	}
	if err := s.applyFrontendConfig(*cfg); err != nil {
		return err
	}
	s.configETag = etag
	if cfg.Version != "" && s.Version != "" && cfg.Version != s.Version && !s.warnedVersion {
		s.warnedVersion = true
		log.Warn().Str("frontend", s.Version).Str("backend", cfg.Version).Msg("the frontend and the backend run different versions: deploy both from the same release")
	}
	return nil
}

// applyFrontendConfig puts the settings in force and rebuilds what depends on them: the
// same-origin check and the listeners.
func (s *Server) applyFrontendConfig(c apiclient.FrontendConfig) error {
	l := &liveSettings{tlsMode: c.TLSMode, httpsPort: c.HTTPSPort, proxyProtocol: c.ProxyProtocol}
	if l.tlsMode != tlsUpload && l.tlsMode != tlsSelfSigned {
		l.tlsMode = tlsOff
	}
	if l.httpsPort == "" {
		l.httpsPort = defaultHTTPSPort
	}
	var err error
	if l.baseURL, err = ParseBaseURL(c.BaseURL); err != nil {
		return fmt.Errorf("base URL from the backend: %w", err)
	}
	if l.trusted, err = ParseTrustedProxies(c.TrustedProxies); err != nil {
		return fmt.Errorf("trusted proxies from the backend: %w", err)
	}
	if c.CertPEM != "" && l.tlsMode != tlsOff {
		cert, info, err := parseCertificate(c.CertPEM, c.KeyPEM)
		if err != nil {
			log.Warn().Err(err).Msg("ignoring the TLS certificate from the backend")
		} else {
			l.tlsCert, l.tlsInfo = cert, info
		}
	}
	l.http3 = c.HTTP3 && l.tlsCert != nil
	s.swapSettings(l)
	s.applyListeners()
	return nil
}

// WaitForBackend fetches the first configuration, retrying until the backend answers or timeout
// passes, so a frontend started together with its backend does not give up too early.
func (s *Server) WaitForBackend(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	delay := 250 * time.Millisecond
	for {
		err := s.RefreshConfig(ctx)
		if err == nil {
			return nil
		}
		if apiErr := new(apiclient.Error); errors.As(err, &apiErr) && apiErr.Status == 401 {
			return fmt.Errorf("the backend refused the service secret: use the same --service-secret-file as the api process")
		}
		if time.Now().Add(delay).After(deadline) {
			return fmt.Errorf("the backend is not available: %w", err)
		}
		log.Info().Err(err).Msg("waiting for the backend")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// configPollInterval is how often the frontend asks whether the settings changed. The answer is a
// cheap "not modified" in the normal case.
var configPollInterval = 5 * time.Second

// StartConfigPoll follows changes to the settings until Close. A backend that is down is not an
// error here: the frontend keeps the last good settings.
func (s *Server) StartConfigPoll() {
	s.startJob(func() {
		for {
			wait := configPollInterval + time.Duration(rand.Int64N(int64(configPollInterval/5)+1))
			select {
			case <-s.jobCtx.Done():
				return
			case <-time.After(wait):
			}
			ctx, cancel := context.WithTimeout(s.jobCtx, 10*time.Second)
			if err := s.RefreshConfig(ctx); err != nil && s.jobCtx.Err() == nil {
				log.Warn().Err(err).Msg("could not refresh the settings from the backend; keeping the last ones")
			}
			cancel()
		}
	})
}
