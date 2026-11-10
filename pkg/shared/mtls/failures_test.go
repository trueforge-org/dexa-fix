package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"strings"
	"testing"
)

func TestConfigsRefuseFilesThatAreMissingOrNotCertificates(t *testing.T) {
	b, _ := Generate(nil)
	good := write(t, t.TempDir(), b)
	bad := write(t, t.TempDir(), b)
	if err := os.WriteFile(bad.ca, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := good.dir + "/nothing.pem"

	t.Run("a server with a CA file that holds no certificate", func(t *testing.T) {
		_, err := ServerConfig(good.srvCert, good.srvKey, bad.ca)
		if err == nil || !strings.Contains(err.Error(), "holds no certificate") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a server whose CA path is a folder", func(t *testing.T) {
		if _, err := ServerConfig(good.srvCert, good.srvKey, good.dir); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("a server with a missing CA file", func(t *testing.T) {
		if _, err := ServerConfig(good.srvCert, good.srvKey, missing); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("a client with a missing certificate", func(t *testing.T) {
		if _, err := ClientConfig(good.ca, missing, good.cliKey, ""); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("a client with a CA file that holds no certificate", func(t *testing.T) {
		if _, err := ClientConfig(bad.ca, good.cliCert, good.cliKey, ""); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("a client with a missing CA file", func(t *testing.T) {
		if _, err := ClientConfig(missing, good.cliCert, good.cliKey, ""); err == nil {
			t.Fatal("accepted")
		}
	})
}

func TestServerConfigPresentsItsCertificateEvenOutsideAClientHello(t *testing.T) {
	b, _ := Generate(nil)
	f := write(t, t.TempDir(), b)
	cfg, err := ServerConfig(f.srvCert, f.srvKey, f.ca)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := cfg.GetCertificate(nil); err != nil || c == nil {
		t.Fatalf("got %v, %v", c, err)
	}
}

func TestClientVerifiesTheWholeChainTheServerSends(t *testing.T) {
	b, _ := Generate(nil)
	f := write(t, t.TempDir(), b)
	cfg, err := ClientConfig(f.ca, f.cliCert, f.cliKey, "")
	if err != nil {
		t.Fatal(err)
	}
	parse := func(p string) *x509.Certificate {
		block, _ := pem.Decode([]byte(p))
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	server, ca := parse(b.ServerCert), parse(b.CA)

	if err := cfg.VerifyConnection(tls.ConnectionState{}); err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Errorf("a server without a certificate: %v", err)
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{ServerName: "localhost", PeerCertificates: []*x509.Certificate{server, ca}}); err != nil {
		t.Errorf("a server with its chain: %v", err)
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{ServerName: "other.example", PeerCertificates: []*x509.Certificate{server, ca}}); err == nil {
		t.Error("a certificate for another name was accepted")
	}
}

func TestGenerateIgnoresEmptyAndRepeatedNames(t *testing.T) {
	b, err := GenerateFor([]string{"api", "", "api", "localhost", "10.0.0.5"}, "srv", "cli")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(b.ServerCert))
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.DNSNames, ","); got != "localhost,api" {
		t.Errorf("DNS names %q", got)
	}
	if len(c.IPAddresses) != 3 { // 10.0.0.5, 127.0.0.1, ::1
		t.Errorf("IP addresses %v", c.IPAddresses)
	}
}
