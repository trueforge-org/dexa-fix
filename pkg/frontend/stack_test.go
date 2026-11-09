package frontend

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	bodybackend "github.com/trueforge-org/dexa-fix/pkg/backend/body"
	mainbackend "github.com/trueforge-org/dexa-fix/pkg/backend/main"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/shared/tlsutil"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

// The frontend has no database, so its tests that need a backend build one next to it: this is
// allowed in test files (the boundary test looks at the non-test dependencies only). The tests
// change the backend's settings through the store, as an administrator would through the Admin page.

const testSecret = "current-secret-0123456789abcdef"

// newBackendServer starts a backend on a new in-memory database and serves its internal API.
func newBackendServer(t *testing.T) (*httptest.Server, *mainbackend.Backend, *sqlstore.Store) {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bodyClient, _, err := bodybackend.NewInProcess(st)
	if err != nil {
		t.Fatal(err)
	}
	b, err := mainbackend.NewBackend(st, mainbackend.WithBody(bodyClient))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	b.SetServiceSecrets(testSecret, "previous-secret-0123456789abcdef")
	bs := httptest.NewServer(b.InternalHandler())
	t.Cleanup(bs.Close)
	return bs, b, st
}

func clientFor(t *testing.T, bs *httptest.Server, secret string) *apiclient.Client {
	t.Helper()
	c, err := apiclient.New(bs.URL, nil, secret)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// splitClient is a client of the backend with the right service secret.
func splitClient(t *testing.T, bs *httptest.Server) *apiclient.Client {
	return clientFor(t, bs, testSecret)
}

// putSettings stores settings and puts them in force in the backend.
func putSettings(t *testing.T, b *mainbackend.Backend, st *sqlstore.Store, settings map[string]string) {
	t.Helper()
	ctx := context.Background()
	for name, v := range settings {
		if err := st.PutSetting(ctx, name, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.ReloadSettings(ctx); err != nil {
		t.Fatal(err)
	}
}

// backendFingerprint is the fingerprint of the certificate the backend hands out.
func backendFingerprint(t *testing.T, bs *httptest.Server) string {
	t.Helper()
	cfg, _, err := splitClient(t, bs).FrontendConfig(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	_, info, err := tlsutil.Parse(cfg.CertPEM, cfg.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return info.Fingerprint
}

// selfSignedPEM makes a certificate and key for names, as an administrator would upload.
func selfSignedPEM(t *testing.T, names ...string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
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

// tlsFreePort returns a port that is free on TCP and, when udp is set, on UDP too (HTTP/3 uses the
// HTTPS port number on UDP). The test is skipped when no such port can be found.
func tlsFreePort(t *testing.T, udp bool) string {
	t.Helper()
	for range 5 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		ln.Close()
		if udp {
			pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", port))
			if err != nil {
				continue
			}
			pc.Close()
		}
		return port
	}
	t.Skip("no port is free for HTTP/3 (UDP) in this environment")
	return ""
}
