package mainbackend

import (
	"context"
	"crypto/tls"
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/mailer"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/shared/tlsutil"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// Server settings are kept in the database and edited on the Admin page, not given as command line
// options: they take effect at once and need no restart. Only what the server must know before it
// can open the database (the listen address, the database, the encryption key) is a flag.
//
// The values are read into a liveSettings once and replaced as a whole when an administrator saves
// a change, so a request always sees one consistent set.
const (
	settingBaseURL        = "server.base_url"
	settingTrustedProxies = "server.trusted_proxies"
	settingProxyProtocol  = "server.proxy_protocol"
	settingAuditRetention = "audit.retention_days"
	settingKeyReminder    = "crypto.key_reminder_days"
	settingSMTPHost       = "smtp.host"
	settingSMTPPort       = "smtp.port"
	settingSMTPUsername   = "smtp.username"
	settingSMTPPassword   = "smtp.password" // stored encrypted like every setting
	settingSMTPFrom       = "smtp.from"
	settingSMTPTLS        = "smtp.tls"

	defaultAuditDays = 365
	defaultSMTPPort  = 587
	maxSettingDays   = 36500
)

// liveSettings is the set of administrator settings the server acts on.
type liveSettings struct {
	baseURL        *url.URL
	trusted        []netip.Prefix
	proxyProtocol  bool
	auditRetention time.Duration // zero keeps the audit log for good
	keyReminder    time.Duration // zero never reminds
	mailer         mailer.Sender // nil without an SMTP server
	tlsMode        string        // off, upload or selfsigned
	tlsCert        *tls.Certificate
	tlsInfo        *tlsutil.Info
	http3          bool
	httpsPort      string // TCP port for HTTPS, and UDP port for HTTP/3
}

// smtpSettings is what the Admin page shows and saves for e-mail.
type smtpSettings = apiclient.SMTPView

// cfg returns the settings in force. It is never nil.
func (s *Backend) cfg() *liveSettings { return s.live.Load() }

// swapSettings replaces the settings in force and rebuilds what depends on them.
func (s *Backend) swapSettings(l *liveSettings) {
	s.live.Store(l)
	if s.onSettings != nil {
		s.onSettings()
	}
}

// settingValue returns a stored setting, "" when it was never set.
func (s *Backend) settingValue(ctx context.Context, name string) (string, error) {
	v, err := s.store.GetSetting(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return v, err
}

// settingDays reads a number of days; an unset or invalid value gives def.
func (s *Backend) settingDays(ctx context.Context, name string, def int) (int, error) {
	v, err := s.settingValue(ctx, name)
	if err != nil || v == "" {
		return def, err
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > maxSettingDays {
		log.Warn().Str("setting", name).Str("value", v).Msg("ignoring an invalid setting")
		return def, nil
	}
	return n, nil
}

// loadSMTP reads the e-mail settings.
func (s *Backend) loadSMTP(ctx context.Context) (cfg mailer.Config, shown smtpSettings, err error) {
	get := func(name string) string {
		if err != nil {
			return ""
		}
		var v string
		v, err = s.settingValue(ctx, name)
		return v
	}
	cfg.Host, cfg.Username, cfg.From, cfg.TLS = get(settingSMTPHost), get(settingSMTPUsername), get(settingSMTPFrom), get(settingSMTPTLS)
	cfg.Password = get(settingSMTPPassword)
	port := get(settingSMTPPort)
	if err != nil {
		return cfg, shown, err
	}
	cfg.Port = defaultSMTPPort
	if n, perr := strconv.Atoi(port); perr == nil && n > 0 && n < 65536 {
		cfg.Port = n
	}
	if cfg.TLS == "" {
		cfg.TLS = mailer.StartTLS
	}
	shown = smtpSettings{Host: cfg.Host, Username: cfg.Username, From: cfg.From, TLS: cfg.TLS, Port: cfg.Port, PasswordSaved: cfg.Password != ""}
	return cfg, shown, nil
}

// reloadSettings reads the administrator settings from the database and puts them in force. A
// stored value that no longer parses is skipped with a warning, so a bad row cannot stop the server.
func (s *Backend) reloadSettings(ctx context.Context) error {
	l := &liveSettings{}

	if v, err := s.settingValue(ctx, settingBaseURL); err != nil {
		return err
	} else if u, perr := ParseBaseURL(v); perr != nil {
		log.Warn().Err(perr).Msg("ignoring the stored base URL")
	} else {
		l.baseURL = u
	}
	if v, err := s.settingValue(ctx, settingTrustedProxies); err != nil {
		return err
	} else if p, perr := ParseTrustedProxies([]string{v}); perr != nil {
		log.Warn().Err(perr).Msg("ignoring the stored trusted proxies")
	} else {
		l.trusted = p
	}
	if v, err := s.settingValue(ctx, settingProxyProtocol); err != nil {
		return err
	} else {
		l.proxyProtocol = v == valueEnabled
	}
	days, err := s.settingDays(ctx, settingAuditRetention, defaultAuditDays)
	if err != nil {
		return err
	}
	l.auditRetention = time.Duration(days) * 24 * time.Hour
	days, err = s.settingDays(ctx, settingKeyReminder, 0)
	if err != nil {
		return err
	}
	l.keyReminder = time.Duration(days) * 24 * time.Hour

	l.tlsMode, err = s.settingValue(ctx, settingTLSMode)
	if err != nil {
		return err
	}
	if l.tlsMode != apiclient.TLSUpload && l.tlsMode != apiclient.TLSSelfSigned {
		l.tlsMode = apiclient.TLSOff
	}
	l.tlsCert, l.tlsInfo = s.loadCertificate(ctx, l.tlsMode, l.baseURL)
	l.httpsPort = defaultHTTPSPort
	if v, err := s.settingValue(ctx, settingHTTPS); err != nil {
		return err
	} else if n, perr := strconv.Atoi(v); perr == nil && n > 0 && n < 65536 {
		l.httpsPort = v
	}
	h3, err := s.settingValue(ctx, settingHTTP3)
	if err != nil {
		return err
	}
	l.http3 = h3 == valueEnabled && l.tlsCert != nil

	cfg, _, err := s.loadSMTP(ctx)
	if err != nil {
		return err
	}
	if cfg.Host != "" {
		if m, merr := mailer.New(cfg); merr != nil {
			log.Warn().Err(merr).Msg("ignoring the stored e-mail settings")
		} else {
			l.mailer = m
		}
	}
	s.swapSettings(l)
	return nil
}

// SetBaseURL and SetTrustedProxies put a value in force without storing it. They exist for tests;
// the application changes these settings through the Admin page.
func (s *Backend) SetBaseURL(u *url.URL) {
	l := *s.cfg()
	l.baseURL = u
	s.swapSettings(&l)
}

func (s *Backend) SetTrustedProxies(p []netip.Prefix) {
	l := *s.cfg()
	l.trusted = p
	s.swapSettings(&l)
}

// selfServiceReset reports whether users can ask for a reset e-mail: password sign-in is on, an
// SMTP server is set up, and the public address is known (e-mailed links must not be built from a
// Host header the sender of the request controls).
func (s *Backend) selfServiceReset(ctx context.Context) bool {
	l := s.cfg()
	return l.mailer != nil && l.baseURL != nil && s.passwordEnabled(ctx)
}

// NetworkSettingNames are the settings that can make the Admin page unreachable when they are
// wrong (the base URL, the trusted proxies, the PROXY protocol). `dexa-fix settings reset-network`
// clears them.
var NetworkSettingNames = []string{settingBaseURL, settingTrustedProxies, settingProxyProtocol}

// ReloadSettings reads the administrator settings from the database again and puts them in force.
// A running backend does this by itself (StartSettingsPoll) when another process changes them; it
// is for a caller that changed the database behind the backend's back and cannot wait for the poll.
func (s *Backend) ReloadSettings(ctx context.Context) error { return s.reloadSettings(ctx) }
