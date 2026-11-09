package web

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/mailer"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// serverSettingsView is the Admin page's form for the network and retention settings. It shows the
// text as it was typed.
type serverSettingsView struct {
	BaseURL         string
	TrustedProxies  string
	ProxyProtocol   bool
	AuditDays       string
	KeyReminderDays string
}

func (s *Server) adminSettingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/server", s.admin(s.adminSaveServerSettings))
	mux.HandleFunc("POST /admin/smtp", s.admin(s.adminSaveSMTP))
	mux.HandleFunc("POST /admin/smtp/test", s.admin(s.adminTestSMTP))
}

// serverSettingsFor reads the stored text of the settings for the form.
func (s *Server) serverSettingsFor(ctx context.Context) (serverSettingsView, error) {
	var v serverSettingsView
	var err error
	get := func(name string) string {
		if err != nil {
			return ""
		}
		var x string
		x, err = s.settingValue(ctx, name)
		return x
	}
	v.BaseURL, v.TrustedProxies = get(settingBaseURL), get(settingTrustedProxies)
	v.ProxyProtocol = get(settingProxyProtocol) == valueEnabled
	v.AuditDays, v.KeyReminderDays = get(settingAuditRetention), get(settingKeyReminder)
	return v, err
}

// putOrClear stores a value, or removes the setting when it is empty so the default applies.
func (s *Server) putOrClear(ctx context.Context, name, value string) error {
	if value == "" {
		return s.store.DeleteSetting(ctx, name)
	}
	return s.store.PutSetting(ctx, name, value)
}

// parseDays reads a number of days from the form: empty means the default, and 0 is allowed.
func parseDays(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > maxSettingDays {
		return "", false
	}
	return strconv.Itoa(n), true
}

func hostOf(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(strings.Trim(hostport, "[]"))
}

func (s *Server) adminSaveServerSettings(w http.ResponseWriter, r *http.Request, _ models.User) {
	ctx := r.Context()
	base, err := ParseBaseURL(r.PostFormValue("base_url"))
	if err != nil {
		adminRedirect(w, r, err.Error())
		return
	}
	// A wrong address would make the cookies and the same-origin check fail for the page the
	// administrator is on, with no way back in. So the address has to be the one in use right now.
	if base != nil && hostOf(base.Host) != hostOf(r.Host) {
		adminRedirect(w, r, "The base URL must be the address you are using now ("+r.Host+"). Open the Admin page at "+base.Host+" and save it there, so you cannot lock yourself out.")
		return
	}
	proxiesText := strings.TrimSpace(r.PostFormValue("trusted_proxies"))
	trusted, err := ParseTrustedProxies([]string{proxiesText})
	if err != nil {
		adminRedirect(w, r, err.Error())
		return
	}
	proxyProtocol := r.PostFormValue("proxy_protocol") != ""
	if proxyProtocol && len(trusted) == 0 {
		adminRedirect(w, r, "The PROXY protocol needs the trusted proxies: only connections from them may send the header.")
		return
	}
	audit, ok := parseDays(r.PostFormValue("audit_retention_days"))
	if !ok {
		adminRedirect(w, r, "Audit log retention is a number of days from 0 to 36500 (0 keeps the log for good).")
		return
	}
	reminder, ok := parseDays(r.PostFormValue("key_reminder_days"))
	if !ok {
		adminRedirect(w, r, "The key reminder is a number of days from 0 to 36500 (0 never reminds).")
		return
	}
	baseText := ""
	if base != nil {
		baseText = base.Scheme + "://" + base.Host
	}
	proxyValue := valueDisabled
	if proxyProtocol {
		proxyValue = valueEnabled
	}
	for _, p := range []struct{ name, value string }{
		{settingBaseURL, baseText}, {settingTrustedProxies, proxiesText}, {settingProxyProtocol, proxyValue},
		{settingAuditRetention, audit}, {settingKeyReminder, reminder},
	} {
		if err := s.putOrClear(ctx, p.name, p.value); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := s.reloadSettings(ctx); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "server_settings")
	adminRedirect(w, r, "Server settings saved. They apply now.")
}

func (s *Server) adminSaveSMTP(w http.ResponseWriter, r *http.Request, _ models.User) {
	ctx := r.Context()
	cur, _, err := s.loadSMTP(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	cfg := mailer.Config{
		Host: strings.TrimSpace(r.PostFormValue("host")), Username: strings.TrimSpace(r.PostFormValue("username")),
		From: strings.TrimSpace(r.PostFormValue("from")), TLS: r.PostFormValue("tls"), Password: cur.Password,
	}
	if pw := r.PostFormValue("password"); pw != "" {
		cfg.Password = pw
	}
	if r.PostFormValue("remove_password") != "" {
		cfg.Password = ""
	}
	if cfg.Host == "" { // an empty host turns e-mail off
		if err := s.store.DeleteSetting(ctx, settingSMTPHost); err != nil {
			s.fail(w, err)
			return
		}
		if err := s.reloadSettings(ctx); err != nil {
			s.fail(w, err)
			return
		}
		s.audit(r, 0, auditSettingChanged, "smtp=off")
		adminRedirect(w, r, "E-mail is off. Administrators can still make reset links.")
		return
	}
	cfg.Port = defaultSMTPPort
	if raw := strings.TrimSpace(r.PostFormValue("port")); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 1 || n > 65535 {
			adminRedirect(w, r, "The port is a number from 1 to 65535.")
			return
		}
		cfg.Port = n
	}
	if _, err := mailer.New(cfg); err != nil {
		adminRedirect(w, r, err.Error())
		return
	}
	for _, p := range []struct{ name, value string }{
		{settingSMTPHost, cfg.Host}, {settingSMTPPort, strconv.Itoa(cfg.Port)}, {settingSMTPUsername, cfg.Username},
		{settingSMTPPassword, cfg.Password}, {settingSMTPFrom, cfg.From}, {settingSMTPTLS, cfg.TLS},
	} {
		if err := s.putOrClear(ctx, p.name, p.value); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := s.reloadSettings(ctx); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "smtp=on")
	msg := "E-mail settings saved. Send a test e-mail to check them."
	if s.cfg().baseURL == nil {
		msg += " Users can only ask for a reset e-mail once the base URL is set."
	}
	adminRedirect(w, r, msg)
}

func (s *Server) adminTestSMTP(w http.ResponseWriter, r *http.Request, u models.User) {
	m := s.cfg().mailer
	if m == nil {
		adminRedirect(w, r, "Set up and save the e-mail settings first.")
		return
	}
	if u.Email == "" {
		adminRedirect(w, r, "Your account has no e-mail address to send the test to.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	if err := m.Send(ctx, u.Email, "dexa-fix test e-mail", "This is a test e-mail from dexa-fix. E-mail is working.\n"); err != nil {
		adminRedirect(w, r, "Could not send the test e-mail: "+err.Error())
		return
	}
	adminRedirect(w, r, "A test e-mail was sent to "+u.Email+".")
}
