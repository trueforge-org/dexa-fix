package mainbackend

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/store"
)

// flashOf returns the message of a redirect to the Admin page.
func flashOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Path != "/admin" {
		t.Fatalf("redirect to %q, want /admin", resp.Header.Get("Location"))
	}
	return loc.Query().Get("msg")
}

// storedSetting returns a stored setting, "" when it is absent.
func storedSetting(t *testing.T, srv *Server, name string) string {
	t.Helper()
	v, err := srv.settingValue(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// assertAbsent checks that a setting was removed from the store, not just stored empty.
func assertAbsent(t *testing.T, srv *Server, name string) {
	t.Helper()
	if _, err := srv.store.GetSetting(context.Background(), name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("setting %s still stored: %v", name, err)
	}
}

func TestReloadSettingsFromStore(t *testing.T) {
	ts, _, _ := newAuthServer(t)
	srv := serverOf(ts)
	ctx := context.Background()
	put := func(name, value string) {
		t.Helper()
		if err := srv.store.PutSetting(ctx, name, value); err != nil {
			t.Fatal(err)
		}
	}
	put(settingBaseURL, "https://dexa.example.com")
	put(settingTrustedProxies, "10.0.0.0/8, loopback")
	put(settingProxyProtocol, valueEnabled)
	put(settingAuditRetention, "30")
	put(settingKeyReminder, "90")
	put(settingSMTPHost, "mail.example.com")
	put(settingSMTPPort, "2587")
	put(settingSMTPUsername, "dexa")
	put(settingSMTPPassword, "smtp-pass")
	put(settingSMTPFrom, "dexa <noreply@example.com>")
	put(settingSMTPTLS, "starttls")
	if err := srv.reloadSettings(ctx); err != nil {
		t.Fatal(err)
	}

	l := srv.cfg()
	if l.baseURL == nil || l.baseURL.Scheme != "https" || l.baseURL.Host != "dexa.example.com" {
		t.Errorf("base URL = %v, want https://dexa.example.com", l.baseURL)
	}
	if len(l.trusted) != 3 {
		t.Errorf("trusted proxies = %v, want 10.0.0.0/8, 127.0.0.0/8 and ::1/128", l.trusted)
	}
	for addr, want := range map[string]bool{"10.9.8.7": true, "127.0.0.1": true, "203.0.113.7": false} {
		if got := srv.trusted(netip.MustParseAddr(addr)); got != want {
			t.Errorf("trusted(%s) = %v, want %v", addr, got, want)
		}
	}
	if !l.proxyProtocol {
		t.Error("PROXY protocol not in force")
	}
	if l.auditRetention != 30*24*time.Hour {
		t.Errorf("audit retention = %v, want 30 days", l.auditRetention)
	}
	if l.keyReminder != 90*24*time.Hour {
		t.Errorf("key reminder = %v, want 90 days", l.keyReminder)
	}
	if l.mailer == nil {
		t.Error("e-mail not set up from the stored SMTP settings")
	}
}

func TestReloadSettingsSkipsInvalidValues(t *testing.T) {
	ts, _, _ := newAuthServer(t)
	srv := serverOf(ts)
	ctx := context.Background()
	put := func(name, value string) {
		t.Helper()
		if err := srv.store.PutSetting(ctx, name, value); err != nil {
			t.Fatal(err)
		}
	}
	put(settingBaseURL, "dexa.example.com") // no scheme
	put(settingTrustedProxies, "nonsense")
	put(settingProxyProtocol, "maybe")
	put(settingAuditRetention, "abc")
	put(settingKeyReminder, "99999") // over the limit
	put(settingSMTPHost, "mail.example.com")
	put(settingSMTPFrom, "not an address")
	if err := srv.reloadSettings(ctx); err != nil {
		t.Fatalf("an invalid stored value stopped the reload: %v", err)
	}

	l := srv.cfg()
	if l.baseURL != nil {
		t.Errorf("invalid base URL kept: %v", l.baseURL)
	}
	if l.trusted != nil {
		t.Errorf("invalid trusted proxies kept: %v", l.trusted)
	}
	if l.proxyProtocol {
		t.Error("PROXY protocol on for a value other than enabled")
	}
	if l.auditRetention != 365*24*time.Hour {
		t.Errorf("audit retention = %v, want the 365 day default", l.auditRetention)
	}
	if l.keyReminder != 0 {
		t.Errorf("key reminder = %v, want off", l.keyReminder)
	}
	if l.mailer != nil {
		t.Error("e-mail set up from an invalid sender address")
	}

	// no host: e-mail is off, and that is not an error
	if err := srv.store.DeleteSetting(ctx, settingSMTPHost); err != nil {
		t.Fatal(err)
	}
	if err := srv.reloadSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if srv.cfg().mailer != nil {
		t.Error("e-mail on without a host")
	}
}

func TestAdminSavesServerSettings(t *testing.T) {
	ts, root, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, root, "root") // user 1, administrator
	own := ts.URL               // http://127.0.0.1:PORT, the address this test uses

	good := func(over url.Values) url.Values {
		v := url.Values{
			"base_url": {own}, "trusted_proxies": {"10.0.0.0/8"},
			"audit_retention_days": {"30"}, "key_reminder_days": {"90"},
		}
		for k, x := range over {
			v[k] = x
		}
		return v
	}
	checkUnchanged := func(step string) {
		t.Helper()
		if got := storedSetting(t, srv, settingBaseURL); got != own {
			t.Errorf("%s: base URL stored as %q", step, got)
		}
		if got := storedSetting(t, srv, settingTrustedProxies); got != "10.0.0.0/8" {
			t.Errorf("%s: trusted proxies stored as %q", step, got)
		}
		if got := storedSetting(t, srv, settingAuditRetention); got != "30" {
			t.Errorf("%s: audit retention stored as %q", step, got)
		}
		if got := storedSetting(t, srv, settingKeyReminder); got != "90" {
			t.Errorf("%s: key reminder stored as %q", step, got)
		}
		l := srv.cfg()
		if l.auditRetention != 30*24*time.Hour || l.keyReminder != 90*24*time.Hour {
			t.Errorf("%s: live retention %v, reminder %v", step, l.auditRetention, l.keyReminder)
		}
		if l.baseURL == nil || l.baseURL.Host != strings.TrimPrefix(own, "http://") {
			t.Errorf("%s: live base URL %v", step, l.baseURL)
		}
	}

	// the address in use: saved, and in force at once
	resp := post(t, root, ts.URL+"/admin/server", good(nil))
	if msg := flashOf(t, resp); !strings.Contains(msg, "saved") {
		t.Fatalf("save: flash %q", msg)
	}
	checkUnchanged("saved")
	if l := srv.cfg(); len(l.trusted) != 1 || !srv.trusted(netip.MustParseAddr("10.1.2.3")) {
		t.Errorf("live trusted proxies = %v", l.trusted)
	}

	refused := []struct {
		name string
		over url.Values
		want string
	}{
		{"another host", url.Values{"base_url": {"https://dexa.example.com"}}, "lock yourself out"},
		{"PROXY protocol without trusted proxies", url.Values{"proxy_protocol": {"1"}, "trusted_proxies": {""}}, "trusted proxies"},
		{"bad trusted proxy", url.Values{"trusted_proxies": {"abc"}}, "trusted proxy"},
		{"bad base URL", url.Values{"base_url": {"dexa.example.com"}}, "base URL"},
		{"negative audit days", url.Values{"audit_retention_days": {"-1"}}, "number of days"},
		{"audit days not a number", url.Values{"audit_retention_days": {"abc"}}, "number of days"},
		{"audit days over the limit", url.Values{"audit_retention_days": {"99999"}}, "number of days"},
		{"reminder days over the limit", url.Values{"key_reminder_days": {"99999"}}, "number of days"},
	}
	for _, c := range refused {
		resp := post(t, root, ts.URL+"/admin/server", good(c.over))
		if msg := flashOf(t, resp); !strings.Contains(msg, c.want) {
			t.Errorf("%s: flash %q, want it to mention %q", c.name, msg, c.want)
		}
		checkUnchanged(c.name)
	}

	// empty fields remove the settings, and the defaults apply
	resp = post(t, root, ts.URL+"/admin/server", url.Values{
		"base_url": {""}, "trusted_proxies": {""}, "audit_retention_days": {""}, "key_reminder_days": {""},
	})
	if msg := flashOf(t, resp); !strings.Contains(msg, "saved") {
		t.Fatalf("clear: flash %q", msg)
	}
	for _, name := range []string{settingBaseURL, settingTrustedProxies, settingAuditRetention, settingKeyReminder} {
		assertAbsent(t, srv, name)
	}
	if l := srv.cfg(); l.baseURL != nil || l.trusted != nil || l.auditRetention != 365*24*time.Hour || l.keyReminder != 0 {
		t.Errorf("after clearing: base %v, trusted %v, audit %v, reminder %v", l.baseURL, l.trusted, l.auditRetention, l.keyReminder)
	}

	// the PROXY protocol is saved with the trusted proxies and takes effect at once
	resp = post(t, root, ts.URL+"/admin/server", url.Values{"trusted_proxies": {"10.0.0.0/8"}, "proxy_protocol": {"1"}})
	if msg := flashOf(t, resp); !strings.Contains(msg, "saved") {
		t.Fatalf("PROXY protocol: flash %q", msg)
	}
	if on, trusted := srv.Backend.proxyProtocolPolicy(); !on || len(trusted) != 1 {
		t.Errorf("PROXY protocol policy = %v, %v", on, trusted)
	}
	if got := storedSetting(t, srv, settingProxyProtocol); got != valueEnabled {
		t.Errorf("PROXY protocol stored as %q", got)
	}
}

func TestAdminSavesSMTP(t *testing.T) {
	ts, root, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, root, "root") // user 1, administrator
	const secret = "smtp-secret-7731"
	const from = "dexa <noreply@example.com>"
	smtpForm := func(host, port, tlsMode, sender, password string) url.Values {
		return url.Values{"host": {host}, "port": {port}, "tls": {tlsMode}, "from": {sender}, "password": {password}}
	}

	resp := post(t, root, ts.URL+"/admin/smtp", smtpForm("localhost", "2525", "none", from, secret))
	if msg := flashOf(t, resp); !strings.Contains(msg, "saved") {
		t.Fatalf("save: flash %q", msg)
	}
	if srv.cfg().mailer == nil {
		t.Fatal("e-mail not in force after saving")
	}
	if got := storedSetting(t, srv, settingSMTPHost); got != "localhost" {
		t.Errorf("host stored as %q", got)
	}
	if got := storedSetting(t, srv, settingSMTPPort); got != "2525" {
		t.Errorf("port stored as %q", got)
	}
	if got := storedSetting(t, srv, settingSMTPPassword); got != secret {
		t.Errorf("password not stored")
	}

	// the password is never sent back to the browser
	code, page := body(t, root, ts.URL+"/admin")
	if code != 200 {
		t.Fatalf("admin page: %d", code)
	}
	if strings.Contains(page, secret) {
		t.Error("the Admin page shows the SMTP password")
	}
	if !strings.Contains(page, "saved: leave empty to keep it") {
		t.Error("the Admin page does not say a password is saved")
	}

	// an empty password keeps the saved one
	resp = post(t, root, ts.URL+"/admin/smtp", smtpForm("localhost", "2525", "none", from, ""))
	if msg := flashOf(t, resp); !strings.Contains(msg, "saved") {
		t.Fatalf("keep password: flash %q", msg)
	}
	if got := storedSetting(t, srv, settingSMTPPassword); got != secret {
		t.Errorf("an empty password field replaced the saved one")
	}

	// refused: unencrypted to another machine, a bad sender, a bad port; nothing changes
	refused := []struct {
		name string
		form url.Values
		want string
	}{
		{"tls none to another host", smtpForm("mail.example.com", "587", "none", from, ""), "only allowed for a server on this machine"},
		{"invalid sender", smtpForm("localhost", "2525", "none", "not an address", ""), "from address"},
		{"port out of range", smtpForm("localhost", "70000", "none", from, ""), "port"},
	}
	for _, c := range refused {
		resp := post(t, root, ts.URL+"/admin/smtp", c.form)
		if msg := flashOf(t, resp); !strings.Contains(msg, c.want) {
			t.Errorf("%s: flash %q, want it to mention %q", c.name, msg, c.want)
		}
		if got := storedSetting(t, srv, settingSMTPHost); got != "localhost" {
			t.Errorf("%s: host changed to %q", c.name, got)
		}
		if srv.cfg().mailer == nil {
			t.Errorf("%s: e-mail went off", c.name)
		}
	}

	// remove_password clears the saved password
	form := smtpForm("localhost", "2525", "none", from, "")
	form.Set("remove_password", "1")
	resp = post(t, root, ts.URL+"/admin/smtp", form)
	if msg := flashOf(t, resp); !strings.Contains(msg, "saved") {
		t.Fatalf("remove password: flash %q", msg)
	}
	assertAbsent(t, srv, settingSMTPPassword)

	// an empty host turns e-mail off
	resp = post(t, root, ts.URL+"/admin/smtp", smtpForm("", "", "", "", ""))
	if msg := flashOf(t, resp); !strings.Contains(msg, "E-mail is off") {
		t.Fatalf("off: flash %q", msg)
	}
	assertAbsent(t, srv, settingSMTPHost)
	if srv.cfg().mailer != nil {
		t.Error("e-mail still in force after turning it off")
	}
}

func TestAdminPageShowsSettingSections(t *testing.T) {
	ts, root, _ := newAuthServer(t)
	signup(t, ts, root, "root")
	code, page := body(t, root, ts.URL+"/admin")
	if code != 200 {
		t.Fatalf("admin page: %d", code)
	}
	for _, want := range []string{"Network and retention", "E-mail"} {
		if !strings.Contains(page, want) {
			t.Errorf("admin page has no %q", want)
		}
	}
}

func TestNonAdminCannotSeeSettings(t *testing.T) {
	ts, root, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, root, "root") // user 1, administrator
	bob := noRedirectClient()
	signup(t, ts, bob, "bob") // user 2

	if resp := post(t, root, ts.URL+"/admin/server", url.Values{"audit_retention_days": {"30"}}); flashOf(t, resp) == "" {
		t.Fatal("administrator could not save")
	}

	if code, page := body(t, bob, ts.URL+"/admin"); code != 404 || strings.Contains(page, "Network and retention") {
		t.Errorf("admin page for a user: %d", code)
	}
	forms := []struct {
		path string
		form url.Values
	}{
		{"/admin/server", url.Values{"audit_retention_days": {"5"}}},
		{"/admin/smtp", url.Values{"host": {"mail.example.com"}, "port": {"587"}, "from": {"dexa <noreply@example.com>"}}},
		{"/admin/smtp/test", url.Values{}},
	}
	for _, f := range forms {
		if resp := post(t, bob, ts.URL+f.path, f.form); resp.StatusCode != 404 {
			t.Errorf("POST %s by a user: %d, want 404", f.path, resp.StatusCode)
		}
	}

	if got := storedSetting(t, srv, settingAuditRetention); got != "30" {
		t.Errorf("a user changed the audit retention to %q", got)
	}
	assertAbsent(t, srv, settingSMTPHost)
	if srv.cfg().mailer != nil {
		t.Error("a user set up e-mail")
	}
}
