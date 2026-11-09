package mtls

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type files struct{ dir, ca, srvCert, srvKey, cliCert, cliKey string }

func write(t *testing.T, dir string, b Bundle) files {
	t.Helper()
	f := files{dir, filepath.Join(dir, "ca.pem"), filepath.Join(dir, "api.pem"), filepath.Join(dir, "api.key"),
		filepath.Join(dir, "web.pem"), filepath.Join(dir, "web.key")}
	for p, v := range map[string]string{f.ca: b.CA, f.srvCert: b.ServerCert, f.srvKey: b.ServerKey, f.cliCert: b.ClientCert, f.cliKey: b.ClientKey} {
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func serve(t *testing.T, f files) *httptest.Server {
	t.Helper()
	cfg, err := ServerConfig(f.srvCert, f.srvKey, f.ca)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.TLS.PeerCertificates[0].Subject.CommonName))
	}))
	ts.TLS = cfg
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func client(t *testing.T, ca, cert, key, name string) *http.Client {
	t.Helper()
	cfg, err := ClientConfig(ca, cert, key, name)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
}

func get(c *http.Client, url string) (string, error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b := make([]byte, 64)
	n, _ := resp.Body.Read(b)
	return string(b[:n]), nil
}

func TestMutualTLSWorksBetweenTheTwoSides(t *testing.T) {
	b, err := Generate([]string{"api"})
	if err != nil {
		t.Fatal(err)
	}
	f := write(t, t.TempDir(), b)
	ts := serve(t, f)
	got, err := get(client(t, f.ca, f.cliCert, f.cliKey, ""), ts.URL) // 127.0.0.1 is among the names
	if err != nil || got != "dexa-fix web" {
		t.Fatalf("%q %v", got, err)
	}
	// A name that is not in the certificate is refused.
	if _, err := get(client(t, f.ca, f.cliCert, f.cliKey, "other.example.com"), ts.URL); err == nil {
		t.Error("a backend with another name was trusted")
	}
	if _, err := get(client(t, f.ca, f.cliCert, f.cliKey, "api"), ts.URL); err != nil {
		t.Errorf("the configured name was refused: %v", err)
	}
}

func TestServerRefusesClientsWithoutOrWithTheWrongCertificate(t *testing.T) {
	b, _ := Generate(nil)
	f := write(t, t.TempDir(), b)
	ts := serve(t, f)

	// No client certificate at all.
	plain := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 5 * time.Second}
	if _, err := get(plain, ts.URL); err == nil {
		t.Error("a client without a certificate was served")
	}
	// A certificate from another CA.
	other, _ := Generate(nil)
	g := write(t, t.TempDir(), other)
	cfg, err := ClientConfig(f.ca, g.cliCert, g.cliKey, "") // trusts the right backend, presents a foreign certificate
	if err != nil {
		t.Fatal(err)
	}
	if _, err := get(&http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}, ts.URL); err == nil {
		t.Error("a client certificate from another CA was accepted")
	}
	// A server certificate used as a client certificate has the wrong key usage.
	cfg, _ = ClientConfig(f.ca, f.srvCert, f.srvKey, "")
	if _, err := get(&http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}, ts.URL); err == nil {
		t.Error("the server's own certificate was accepted as a client certificate")
	}
}

func TestClientRefusesABackendFromAnotherCA(t *testing.T) {
	b, _ := Generate(nil)
	f := write(t, t.TempDir(), b)
	ts := serve(t, f)
	other, _ := Generate(nil)
	g := write(t, t.TempDir(), other)
	if _, err := get(client(t, g.ca, g.cliCert, g.cliKey, ""), ts.URL); err == nil {
		t.Error("a backend with a certificate from an unknown CA was trusted")
	}
}

func TestRotatedFilesAreUsedWithoutRestart(t *testing.T) {
	old := recheck
	recheck = 0
	t.Cleanup(func() { recheck = old })

	b1, _ := Generate(nil)
	f := write(t, t.TempDir(), b1)
	ts := serve(t, f)
	c := client(t, f.ca, f.cliCert, f.cliKey, "")
	if _, err := get(c, ts.URL); err != nil {
		t.Fatal(err)
	}
	// Rotate everything in place: new CA, new certificates.
	time.Sleep(20 * time.Millisecond) // a different modification time
	b2, _ := Generate(nil)
	write(t, f.dir, b2)
	c.CloseIdleConnections()
	if got, err := get(c, ts.URL); err != nil || got != "dexa-fix web" {
		t.Fatalf("after the rotation: %q %v", got, err)
	}
	// Half-written files do not break the link: the last good ones stay in use.
	os.WriteFile(f.srvCert, []byte("garbage"), 0o600)
	os.WriteFile(f.ca, []byte("garbage"), 0o600)
	c.CloseIdleConnections()
	if _, err := get(c, ts.URL); err != nil {
		t.Errorf("a broken file cut the link: %v", err)
	}
}

func TestConfigErrors(t *testing.T) {
	if _, err := ServerConfig("", "", ""); err == nil {
		t.Error("server config without files")
	}
	if _, err := ClientConfig("", "", "", ""); err == nil {
		t.Error("client config without files")
	}
	if _, err := ServerConfig("/nonexistent/a", "/nonexistent/b", "/nonexistent/c"); err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("missing files: %v", err)
	}
}
