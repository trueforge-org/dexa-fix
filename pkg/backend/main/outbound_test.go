package mainbackend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/shared/netguard"
)

// A private address is refused before anything is sent. https://10.255.255.1 is never contacted: the
// guard answers on the address the dialer resolved to.
const privateHTTPS = "https://10.255.255.1/"

func TestTheProviderClientRefusesPrivateAddressesUnlessAllowed(t *testing.T) {
	g := newRig(t)
	_, err := g.b.httpClient.Get(privateHTTPS)
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("a private address by default: %v, want it refused", err)
	}
	_, err = g.b.httpClient.Get("https://169.254.169.254/latest/meta-data/")
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("the metadata address: %v, want it refused", err)
	}
}

func TestTheProviderClientHoldsRedirectsToTheRulesOfTheAddressOfAnAdministrator(t *testing.T) {
	g := newRig(t)
	redirect := func(to string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusFound) }))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	t.Run("a redirect to a private address is refused at the dial", func(t *testing.T) {
		if _, err := g.b.httpClient.Get(redirect(privateHTTPS)); !errors.Is(err, netguard.ErrBlocked) {
			t.Fatalf("got %v, want the guard to refuse it", err)
		}
	})
	t.Run("a redirect to the metadata address is refused", func(t *testing.T) {
		if _, err := g.b.httpClient.Get(redirect("https://169.254.169.254/")); !errors.Is(err, netguard.ErrBlocked) {
			t.Fatalf("got %v, want the guard to refuse it", err)
		}
	})
	t.Run("a redirect to plain http on another host is refused", func(t *testing.T) {
		_, err := g.b.httpClient.Get(redirect("http://example.com/"))
		if err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("got %v, want a refused redirect", err)
		}
	})
	t.Run("a loop ends", func(t *testing.T) {
		var self string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, self, http.StatusFound) }))
		t.Cleanup(srv.Close)
		self = srv.URL
		_, err := g.b.httpClient.Get(self)
		if err == nil || !strings.Contains(err.Error(), "too many redirects") {
			t.Fatalf("got %v, want the loop to end", err)
		}
	})
}

func TestTheAdministratorIsToldWhyAProviderOrMailServerCannotBeReachedWithoutTheirAnswer(t *testing.T) {
	provider := func(issuer string) url.Values {
		return url.Values{"kind": {"oidc"}, "slug": {"g"}, "label": {"G"}, "client_id": {"c"}, "client_secret": {"s"}, "issuer": {issuer}}
	}
	t.Run("a provider on a private network says how to allow it", func(t *testing.T) {
		e := newAdminEnv(t)
		code, out := e.do(t, "POST", "/admin/providers", provider("https://10.255.255.1"))
		got := adminFlash(t, out)
		if code != 303 || !strings.HasPrefix(got, "Could not read the OpenID configuration from that issuer: ") || !strings.Contains(got, "private or link-local") || !strings.Contains(got, settingLabelProviders) || strings.Contains(got, settingLabelMail) {
			t.Fatalf("%d %q", code, got)
		}
	})
	t.Run("a provider that answers badly does not show what it answered", func(t *testing.T) {
		secret := "secret-page-of-an-internal-service"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, secret, http.StatusTeapot)
		}))
		t.Cleanup(srv.Close)
		e := newAdminEnv(t)
		code, out := e.do(t, "POST", "/admin/providers", provider(srv.URL))
		got := adminFlash(t, out)
		if code != 303 || strings.Contains(got, secret) || strings.Contains(got, "418") || !strings.Contains(got, "could not be reached or did not answer as expected") {
			t.Fatalf("%d %q", code, got)
		}
	})
	t.Run("a mail server on a private network says how to allow it", func(t *testing.T) {
		e := newAdminEnv(t)
		code, out := e.do(t, "POST", "/admin/smtp", url.Values{"host": {"10.255.255.1"}, "port": {"587"}, "from": {"DexaFix <noreply@example.com>"}})
		if got := adminFlash(t, out); code != 303 || !strings.Contains(got, "saved") {
			t.Fatalf("saving: %d %q", code, got)
		}
		code, out = e.do(t, "POST", "/admin/smtp/test", url.Values{})
		got := adminFlash(t, out)
		if code != 303 || !strings.HasPrefix(got, "Could not send the test e-mail: ") || !strings.Contains(got, "private or link-local") || !strings.Contains(got, settingLabelMail) || strings.Contains(got, settingLabelProviders) {
			t.Fatalf("test e-mail: %d %q", code, got)
		}
	})
	t.Run("a mail server that is not there is not described in detail", func(t *testing.T) {
		e := newAdminEnv(t)
		code, out := e.do(t, "POST", "/admin/smtp", url.Values{"host": {"127.0.0.1"}, "port": {"1"}, "tls": {"none"}, "from": {"DexaFix <noreply@example.com>"}})
		if got := adminFlash(t, out); code != 303 || !strings.Contains(got, "saved") {
			t.Fatalf("saving: %d %q", code, got)
		}
		code, out = e.do(t, "POST", "/admin/smtp/test", url.Values{})
		got := adminFlash(t, out)
		if code != 303 || strings.Contains(got, "connect to") || strings.Contains(got, "127.0.0.1") || !strings.Contains(got, "could not be reached") {
			t.Fatalf("test e-mail: %d %q", code, got)
		}
	})
}

func TestThePrivateNetworkSettingsAreSeparateSavedViewedAndAppliedAtOnce(t *testing.T) {
	ts, root, _ := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, root, "root")
	checked := func(name string) bool {
		return strings.Contains(string(mustGet(t, root, ts.URL+"/admin")), `name="`+name+`" value="1" checked`)
	}
	save := func(form url.Values) {
		t.Helper()
		if msg := flashOf(t, post(t, root, ts.URL+"/admin/server", form)); !strings.Contains(msg, "saved") {
			t.Fatalf("saving %v: flash %q", form, msg)
		}
	}
	if srv.allowPrivateProviders() || srv.allowPrivateMail() {
		t.Fatal("private addresses are allowed before anybody asked for it")
	}

	save(url.Values{"allow_private_providers": {"1"}})
	if !srv.allowPrivateProviders() || srv.allowPrivateMail() {
		t.Fatalf("providers on: providers %v, mail %v", srv.allowPrivateProviders(), srv.allowPrivateMail())
	}
	if storedSetting(t, srv, settingAllowPrivateProviders) != valueEnabled || storedSetting(t, srv, settingAllowPrivateMail) != valueDisabled {
		t.Fatalf("stored %q and %q", storedSetting(t, srv, settingAllowPrivateProviders), storedSetting(t, srv, settingAllowPrivateMail))
	}
	if !checked("allow_private_providers") || checked("allow_private_mail") {
		t.Fatal("the Admin page does not show only the providers setting as on")
	}

	save(url.Values{"allow_private_mail": {"1"}})
	if srv.allowPrivateProviders() || !srv.allowPrivateMail() {
		t.Fatalf("mail on: providers %v, mail %v", srv.allowPrivateProviders(), srv.allowPrivateMail())
	}
	if checked("allow_private_providers") || !checked("allow_private_mail") {
		t.Fatal("the Admin page does not show only the mail setting as on")
	}

	save(url.Values{"allow_private_providers": {"1"}, "allow_private_mail": {"1"}})
	if !srv.allowPrivateProviders() || !srv.allowPrivateMail() || !checked("allow_private_providers") || !checked("allow_private_mail") {
		t.Fatal("both on: not both applied and shown")
	}
	// each guard asks its own setting, and metadata stays refused whatever is chosen
	if err := netguard.Check(mustAddr(t, "10.0.0.5"), srv.allowPrivateMail()); err != nil {
		t.Fatalf("a private address with mail allowed: %v", err)
	}
	if err := netguard.Check(mustAddr(t, "169.254.169.254"), srv.allowPrivateProviders()); !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("the metadata address with providers allowed: %v, want it refused still", err)
	}

	save(url.Values{})
	if srv.allowPrivateProviders() || srv.allowPrivateMail() || checked("allow_private_providers") || checked("allow_private_mail") {
		t.Fatal("unchecked boxes did not turn both off")
	}
}

// One setting must not open the other door: with the mail server allowed a provider on a private
// address is still refused, and the other way round. Both refuse before connecting, so nothing is
// contacted.
func TestAllowingPrivateAddressesForOneKindOfConnectionDoesNotAllowTheOther(t *testing.T) {
	t.Run("a private mail server does not open private providers", func(t *testing.T) {
		e := newAdminEnv(t)
		if code, out := e.do(t, "POST", "/admin/server", url.Values{"allow_private_mail": {"1"}}); code != 303 || !strings.Contains(adminFlash(t, out), "saved") {
			t.Fatalf("saving: %d %q", code, out)
		}
		code, out := e.do(t, "POST", "/admin/providers", url.Values{"kind": {"oidc"}, "slug": {"g"}, "label": {"G"}, "client_id": {"c"}, "client_secret": {"s"}, "issuer": {"https://10.255.255.1"}})
		if got := adminFlash(t, out); code != 303 || !strings.Contains(got, settingLabelProviders) {
			t.Fatalf("%d %q, want the provider refused and the providers setting named", code, got)
		}
	})
	t.Run("a private provider does not open a private mail server", func(t *testing.T) {
		e := newAdminEnv(t)
		if code, out := e.do(t, "POST", "/admin/server", url.Values{"allow_private_providers": {"1"}}); code != 303 || !strings.Contains(adminFlash(t, out), "saved") {
			t.Fatalf("saving: %d %q", code, out)
		}
		if code, out := e.do(t, "POST", "/admin/smtp", url.Values{"host": {"10.255.255.1"}, "port": {"587"}, "from": {"DexaFix <noreply@example.com>"}}); code != 303 || !strings.Contains(adminFlash(t, out), "saved") {
			t.Fatalf("saving the mail server: %d %q", code, out)
		}
		code, out := e.do(t, "POST", "/admin/smtp/test", url.Values{})
		if got := adminFlash(t, out); code != 303 || !strings.Contains(got, settingLabelMail) {
			t.Fatalf("%d %q, want the mail server refused and the mail setting named", code, got)
		}
	})
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
