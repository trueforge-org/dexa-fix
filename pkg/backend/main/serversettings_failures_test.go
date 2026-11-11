package mainbackend

import (
	"context"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

func TestSettingsThatCannotBeReadAreNotPutInForce(t *testing.T) {
	// Every setting the backend loads is read from the database; when one cannot be read, the
	// settings in force stay as they were and the caller hears of it.
	names := []string{
		settingBaseURL, settingTrustedProxies, settingProxyProtocol, settingAllowPrivate, settingAuditRetention, settingKeyReminder,
		settingTLSMode, settingHTTPS, settingHTTP3, settingSMTPHost, settingScarfPixel,
		settingWorkersDefaultPriority, settingWorkersKeepDays, kindSettingName(kindWithingsImport, fieldMaxAttempts),
	}
	for _, name := range names {
		t.Run("when "+name+" cannot be read", func(t *testing.T) {
			r := newJobRig(t)
			before := r.b.cfg()
			r.flaky.failOn("GetSetting:" + name)
			if err := r.b.reloadSettings(context.Background()); err == nil {
				t.Fatal("the settings were loaded although one could not be read")
			}
			if r.b.cfg() != before {
				t.Fatal("the settings in force changed")
			}
		})
	}
	t.Run("when the generation cannot be read", func(t *testing.T) {
		r := newJobRig(t)
		r.flaky.failOn("SettingsGeneration")
		if err := r.b.reloadSettings(context.Background()); err == nil {
			t.Fatal("the settings were loaded without their generation")
		}
	})
}

func TestStoredSettingsThatAreNonsenseAreIgnored(t *testing.T) {
	r := newJobRig(t)
	ctx := context.Background()
	for name, v := range map[string]string{settingBaseURL: "ftp://x", settingTrustedProxies: "banana", settingHTTPS: "99999", settingSMTPHost: "mail", settingSMTPFrom: "not an address"} {
		if err := r.st.PutSetting(ctx, name, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.b.reloadSettings(ctx); err != nil {
		t.Fatal(err)
	}
	l := r.b.cfg()
	if l.baseURL != nil || len(l.trusted) != 0 || l.httpsPort != defaultHTTPSPort || l.mailer != nil {
		t.Fatalf("nonsense was put in force: %+v", l)
	}
}

func TestTheStoredCertificateIsLoadedOrIgnored(t *testing.T) {
	cert, key := certAndKey(t)
	put := func(r *jobRig, mode, c, k string) {
		t.Helper()
		for name, v := range map[string]string{settingTLSMode: mode, settingTLSCert: c, settingTLSKey: k} {
			if err := r.st.PutSetting(context.Background(), name, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	reload := func(r *jobRig) error { return r.b.reloadSettings(context.Background()) }
	t.Run("an uploaded certificate is used", func(t *testing.T) {
		r := newJobRig(t)
		put(r, apiclient.TLSUpload, cert, key)
		if err := reload(r); err != nil || r.b.cfg().tlsCert == nil {
			t.Fatalf("certificate in force: %v, error %v", r.b.cfg().tlsCert != nil, err)
		}
	})
	t.Run("an uploaded certificate that cannot be read leaves HTTPS off", func(t *testing.T) {
		r := newJobRig(t)
		put(r, apiclient.TLSUpload, cert, key)
		r.flaky.failOn("GetSetting:" + settingTLSKey)
		if err := reload(r); err != nil || r.b.cfg().tlsCert != nil {
			t.Fatalf("certificate in force: %v, error %v", r.b.cfg().tlsCert != nil, err)
		}
	})
	t.Run("an uploaded certificate that is not one leaves HTTPS off", func(t *testing.T) {
		r := newJobRig(t)
		put(r, apiclient.TLSUpload, "nonsense", "nonsense")
		if err := reload(r); err != nil || r.b.cfg().tlsCert != nil {
			t.Fatalf("certificate in force: %v, error %v", r.b.cfg().tlsCert != nil, err)
		}
	})
	for _, name := range []string{settingTLSCert, settingTLSKey} {
		t.Run("a self-signed certificate whose "+name+" cannot be stored leaves HTTPS off", func(t *testing.T) {
			r := newJobRig(t)
			if err := r.st.PutSetting(context.Background(), settingTLSMode, apiclient.TLSSelfSigned); err != nil {
				t.Fatal(err)
			}
			r.flaky.failOn("PutSetting:" + name)
			if err := reload(r); err != nil || r.b.cfg().tlsCert != nil {
				t.Fatalf("certificate in force %v, error %v", r.b.cfg().tlsCert != nil, err)
			}
		})
	}
	t.Run("a backend that must not generate loads what another stored, if it is usable", func(t *testing.T) {
		r := newJobRig(t)
		r.b.noCertGen = true
		put(r, apiclient.TLSSelfSigned, "nonsense", "nonsense")
		if err := reload(r); err != nil || r.b.cfg().tlsCert != nil {
			t.Fatalf("an unusable stored certificate was used: %v", err)
		}
		put(r, apiclient.TLSSelfSigned, cert, key)
		if err := reload(r); err != nil || r.b.cfg().tlsCert == nil {
			t.Fatalf("a usable stored certificate was not used: %v", err)
		}
	})
	t.Run("a certificate for no name cannot be made", func(t *testing.T) {
		if _, _, err := generateSelfSigned(nil); err == nil {
			t.Fatal("a certificate was made for no name")
		}
	})
}

func TestTheSelfSignedCertificateIsRenewedBeforeItExpires(t *testing.T) {
	r := newJobRig(t)
	ctx := context.Background()
	r.renewCertificateNow(ctx) // the mode is off: nothing happens
	if r.b.cfg().tlsCert != nil {
		t.Fatal("a certificate was made while TLS is off")
	}
	if err := r.st.PutSetting(ctx, settingTLSMode, apiclient.TLSSelfSigned); err != nil {
		t.Fatal(err)
	}
	r.b.swapSettings(withMode(r.b.cfg(), apiclient.TLSSelfSigned)) // the mode in force follows the stored one at the next reload
	r.flaky.failOn("SettingsGeneration")                           // the renewal cannot reload: it is logged and nothing changes
	r.renewCertificateNow(ctx)
	if r.b.cfg().tlsCert != nil {
		t.Fatal("a certificate appeared although the settings could not be reloaded")
	}
	r.flaky.heal()
	r.renewCertificateNow(ctx) // none yet: one is made
	first := r.b.cfg().tlsCert
	if first == nil {
		t.Fatal("no certificate was made")
	}
	r.renewCertificateNow(ctx) // far from expiry: the same one stays
	if r.b.cfg().tlsCert != first {
		t.Fatal("a certificate that is far from expiring was replaced")
	}
	r.b.noCertGen = true
	r.b.swapSettings(withInfoExpiring(r.b.cfg()))
	r.renewCertificateNow(ctx) // this backend must not generate
	if r.b.cfg().tlsCert != first {
		t.Fatal("a backend that must not generate renewed the certificate")
	}
}

func (r *jobRig) renewCertificateNow(ctx context.Context) { r.b.renewCertificate(ctx) }

func withMode(l *liveSettings, mode string) *liveSettings {
	c := *l
	c.tlsMode = mode
	return &c
}

func withInfoExpiring(l *liveSettings) *liveSettings {
	c := *l
	info := *c.tlsInfo
	info.NotAfter = time.Now().Add(time.Hour)
	c.tlsInfo = &info
	return &c
}
