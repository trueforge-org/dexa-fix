package mainbackend

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// certAndKey makes a certificate for localhost and its private key, both as PEM.
func certAndKey(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func TestAdminTLSChangesTellTheAdministratorWhatHappenedOrFailCleanly(t *testing.T) {
	cert, key := certAndKey(t)
	self := url.Values{"mode": {"selfsigned"}, "port": {"9443"}, "names": {"localhost, 127.0.0.1"}}
	upload := url.Values{"mode": {"upload"}, "cert": {cert}, "key": {key}}
	runAdminChanges(t, []adminChange{
		{name: "a certificate mode that does not exist", path: "/admin/tls", form: url.Values{"mode": {"magic"}}, wantFlash: "Choose off"},
		{name: "an HTTPS port that is not a port", path: "/admin/tls", form: url.Values{"mode": {"off"}, "port": {"99999"}}, wantFlash: "HTTPS port is a number"},
		{name: "HTTP/3 without a certificate", path: "/admin/tls", form: url.Values{"mode": {"off"}, "http3": {"1"}}, wantFlash: "HTTP/3 needs TLS"},
		{name: "a change when the stored mode cannot be read", setup: failing("GetSetting:" + settingTLSMode), path: "/admin/tls", form: url.Values{"mode": {"off"}}},
		{name: "an uploaded certificate that is not one", path: "/admin/tls", form: url.Values{"mode": {"upload"}, "cert": {"x"}, "key": {"y"}}, wantFlash: "do not belong together"},
		{name: "an uploaded certificate whose storing fails", setup: failing("PutSetting:" + settingTLSCert), path: "/admin/tls", form: upload},
		{name: "an uploaded key whose storing fails", setup: failing("PutSetting:" + settingTLSKey), path: "/admin/tls", form: upload},
		{name: "an uploaded certificate", path: "/admin/tls", form: upload, wantFlash: "HTTPS is on"},
		{name: "keeping a stored certificate that is gone", setup: func(t *testing.T, e *adminEnv) {
			if err := e.st.PutSetting(context.Background(), settingTLSMode, apiclient.TLSUpload); err != nil {
				t.Fatal(err)
			}
		}, path: "/admin/tls", form: url.Values{"mode": {"upload"}}, wantFlash: "could not be used"},
		{name: "a self-signed certificate with a name that has a space", path: "/admin/tls", form: formWith(self, "names", "bad name"), wantFlash: "host name or an IP address"},
		{name: "a self-signed certificate when an old certificate cannot be removed", setup: failing("DeleteSetting:" + settingTLSCert), path: "/admin/tls", form: self},
		{name: "a self-signed certificate when the key cannot be removed", setup: failing("DeleteSetting:" + settingTLSKey), path: "/admin/tls", form: self},
		{name: "a self-signed certificate when the names cannot be stored", setup: failing("PutSetting:" + settingTLSNames), path: "/admin/tls", form: self},
		{name: "a self-signed certificate with HTTP/3", path: "/admin/tls", form: formWith(self, "http3", "1"), wantFlash: "HTTP/3 listens on UDP port 9443"},
		{name: "turning TLS off", path: "/admin/tls", form: url.Values{"mode": {"off"}}, wantFlash: "TLS is off"},
		{name: "a change when the mode cannot be stored", setup: failing("PutSetting:" + settingTLSMode), path: "/admin/tls", form: self},
		{name: "a change when the port cannot be stored", setup: failing("PutSetting:" + settingHTTPS), path: "/admin/tls", form: self},
		{name: "a change when the HTTP/3 choice cannot be stored", setup: failing("PutSetting:" + settingHTTP3), path: "/admin/tls", form: self},
		{name: "a change that cannot be applied", setup: failing("SettingsGeneration"), path: "/admin/tls", form: self},
	})
}

func TestAdminSMTPChangesTellTheAdministratorWhatHappenedOrFailCleanly(t *testing.T) {
	smtp := url.Values{"host": {"127.0.0.1"}, "port": {"2525"}, "tls": {"none"}, "from": {"noreply@example.com"}, "username": {"u"}, "password": {"p"}}
	runAdminChanges(t, []adminChange{
		{name: "e-mail settings when they cannot be read", setup: failing("GetSetting:" + settingSMTPHost), path: "/admin/smtp", form: smtp},
		{name: "turning e-mail off when the setting cannot be removed", setup: failing("DeleteSetting:" + settingSMTPHost), path: "/admin/smtp", form: url.Values{}},
		{name: "turning e-mail off when it cannot be applied", setup: failing("SettingsGeneration"), path: "/admin/smtp", form: url.Values{}},
		{name: "turning e-mail off", path: "/admin/smtp", form: url.Values{}, wantFlash: "E-mail is off"},
		{name: "a port that is not a port", path: "/admin/smtp", form: formWith(smtp, "port", "70000"), wantFlash: "number from 1 to 65535"},
		{name: "a sender that is not an address", path: "/admin/smtp", form: formWith(smtp, "from", "not an address"), wantFlash: "from address"},
		{name: "e-mail settings when one cannot be stored", setup: failing("PutSetting:" + settingSMTPHost), path: "/admin/smtp", form: smtp},
		{name: "e-mail settings that cannot be applied", setup: failing("SettingsGeneration"), path: "/admin/smtp", form: smtp},
		{name: "e-mail settings without a base URL", path: "/admin/smtp", form: smtp, wantFlash: "once the base URL is set"},
	})
}
