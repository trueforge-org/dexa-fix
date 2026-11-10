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
	"math/big"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/tlsutil"
)

// tlsCertFor makes a self-signed certificate with the given validity, for the cases the server
// would never generate itself: an expired certificate, or one close to expiry.
func tlsCertFor(t *testing.T, names []string, notBefore, notAfter time.Time) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0]},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
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
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func TestGenerateSelfSigned(t *testing.T) {
	names := []string{"dexa.example.com", "192.168.1.10", "::1", "localhost"}
	certPEM, keyPEM, err := generateSelfSigned(names)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
		t.Fatalf("certificate and key do not load as a pair: %v", err)
	}
	c, info, err := tlsutil.Parse(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseCertificate: %v", err)
	}
	leaf := c.Leaf

	for _, dns := range []string{"dexa.example.com", "localhost"} {
		if !slices.Contains(leaf.DNSNames, dns) {
			t.Errorf("DNS SANs %v do not contain %q", leaf.DNSNames, dns)
		}
	}
	for _, ip := range []string{"192.168.1.10", "::1"} {
		found := false
		for _, got := range leaf.IPAddresses {
			found = found || got.Equal(net.ParseIP(ip))
		}
		if !found {
			t.Errorf("IP SANs %v do not contain %s", leaf.IPAddresses, ip)
		}
	}
	if !sameNames(info.Names, names) {
		t.Errorf("reported names %v, want %v", info.Names, names)
	}

	priv, ok := c.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || !priv.PublicKey.Equal(leaf.PublicKey) {
		t.Error("the private key does not match the certificate's public key")
	}

	now := time.Now()
	if !leaf.NotBefore.Before(now) {
		t.Errorf("NotBefore %s is not in the past", leaf.NotBefore)
	}
	if d := leaf.NotAfter.Sub(now) - selfSignedDays*24*time.Hour; d < -time.Minute || d > time.Minute {
		t.Errorf("NotAfter %s is not about %d days from now", leaf.NotAfter, selfSignedDays)
	}
}

func TestParseCertificateErrors(t *testing.T) {
	names := []string{"dexa.example.com"}
	certPEM, _, err := generateSelfSigned(names)
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, err := generateSelfSigned(names)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	expiredCert, expiredKey := tlsCertFor(t, names, now.Add(-48*time.Hour), now.Add(-24*time.Hour))

	cases := []struct {
		name, cert, key, want string
	}{
		{"empty", "", "", "both a certificate and a private key"},
		{"certificate without key", certPEM, "", "both a certificate and a private key"},
		{"mismatching key", certPEM, otherKey, "do not belong together"},
		{"garbage", "garbage", "garbage", "do not belong together"},
		{"expired", expiredCert, expiredKey, "expired"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := tlsutil.Parse(c.cert, c.key)
			if err == nil {
				t.Fatal("accepted, want an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestSelfSignedNames(t *testing.T) {
	ts, _, _ := newAuthServer(t)
	srv := serverOf(ts)
	ctx := context.Background()
	base, err := url.Parse("https://dexa.example.com:8443")
	if err != nil {
		t.Fatal(err)
	}

	if got, want := srv.selfSignedNames(ctx, base), []string{"dexa.example.com", "localhost", "127.0.0.1", "::1"}; !slices.Equal(got, want) {
		t.Errorf("default names %v, want %v", got, want)
	}
	if got, want := srv.selfSignedNames(ctx, nil), []string{"localhost", "127.0.0.1", "::1"}; !slices.Equal(got, want) {
		t.Errorf("default names without a base URL %v, want %v", got, want)
	}

	if err := srv.store.PutSetting(ctx, settingTLSNames, " a.example.com ,10.0.0.5, ,  "); err != nil {
		t.Fatal(err)
	}
	if got, want := srv.selfSignedNames(ctx, base), []string{"a.example.com", "10.0.0.5"}; !slices.Equal(got, want) {
		t.Errorf("names from the setting %v, want %v (base URL host ignored)", got, want)
	}

	if !sameNames([]string{"Dexa.Example.com"}, []string{"dexa.example.com"}) {
		t.Error("sameNames is case-sensitive, want case-insensitive")
	}
	if sameNames([]string{"a.example.com"}, []string{"b.example.com"}) {
		t.Error("sameNames reports different names as the same")
	}
	if sameNames([]string{"a.example.com"}, []string{"a.example.com", "a.example.com"}) {
		t.Error("sameNames reports lists of different lengths as the same")
	}
}

func TestSelfSignedIsRegeneratedWhenNamesChangeOrNearExpiry(t *testing.T) {
	ts, _, _ := newAuthServer(t)
	srv := serverOf(ts)
	ctx := context.Background()

	_, first, err := srv.selfSigned(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, again, err := srv.selfSigned(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Fingerprint != first.Fingerprint {
		t.Error("the certificate was regenerated although nothing changed")
	}

	if err := srv.store.PutSetting(ctx, settingTLSNames, "changed.example.com"); err != nil {
		t.Fatal(err)
	}
	_, renamed, err := srv.selfSigned(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Fingerprint == first.Fingerprint {
		t.Error("the certificate was not regenerated after the names changed")
	}
	if !sameNames(renamed.Names, []string{"changed.example.com"}) {
		t.Errorf("names after the change %v, want changed.example.com", renamed.Names)
	}

	// Same names, but nearly expired: it must be replaced.
	now := time.Now()
	certPEM, keyPEM := tlsCertFor(t, srv.selfSignedNames(ctx, nil), now.Add(-time.Hour), now.Add(10*24*time.Hour))
	_, soon, err := tlsutil.Parse(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.PutSetting(ctx, settingTLSCert, certPEM); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.PutSetting(ctx, settingTLSKey, keyPEM); err != nil {
		t.Fatal(err)
	}
	_, fresh, err := srv.selfSigned(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Fingerprint == soon.Fingerprint {
		t.Error("a certificate that expires in 10 days was kept")
	}
	if time.Until(fresh.NotAfter) < 300*24*time.Hour {
		t.Errorf("regenerated certificate expires %s, want about a year from now", fresh.NotAfter)
	}
}
