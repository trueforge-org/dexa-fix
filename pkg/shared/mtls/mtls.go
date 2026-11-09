// Package mtls secures the link between the web (frontend) and api (backend) processes with mutual
// TLS: the backend only talks to a client that presents a certificate from the configured CA, and
// the frontend only talks to a backend that presents one. It sits next to the shared service secret,
// which stays required, so that either one failing does not open the link.
//
// Certificates are read from files and picked up again when the files change, so rotating one needs
// no restart. The package uses only the standard library.
package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

// Bundle is a CA with a certificate for the backend (server) and one for the frontend (client), in
// PEM. The CA's private key is not kept: nothing else is meant to be issued from it.
type Bundle struct {
	CA         string
	ServerCert string
	ServerKey  string
	ClientCert string
	ClientKey  string
}

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 2 * 365 * 24 * time.Hour
)

// Generate makes a bundle. serverNames are the names and addresses the frontend uses to reach the
// backend (host names and IP addresses, such as "api" or "10.0.0.5"); "localhost" is always added.
func Generate(serverNames []string) (Bundle, error) {
	return GenerateFor(serverNames, "dexa-fix api", "dexa-fix web")
}

// GenerateFor is Generate for any link between two processes: serverCN and clientCN only label the
// two certificates (nothing checks them; the CA and the server names are what is verified). Every
// link should have a CA of its own, so that a certificate of one link opens nothing of another.
func GenerateFor(serverNames []string, serverCN, clientCN string) (Bundle, error) {
	now := time.Now().Add(-5 * time.Minute) // tolerate small clock differences between hosts
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Bundle{}, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "dexa-fix internal CA"},
		NotBefore:             now,
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return Bundle{}, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return Bundle{}, err
	}

	var b Bundle
	b.CA = pemCert(caDER)

	srvTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: serverCN},
		NotBefore:    now,
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	seen := map[string]bool{}
	for _, n := range append([]string{"localhost"}, serverNames...) {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		if ip := net.ParseIP(n); ip != nil {
			srvTmpl.IPAddresses = append(srvTmpl.IPAddresses, ip)
		} else {
			srvTmpl.DNSNames = append(srvTmpl.DNSNames, n)
		}
	}
	srvTmpl.IPAddresses = append(srvTmpl.IPAddresses, net.IPv4(127, 0, 0, 1), net.IPv6loopback)
	if b.ServerCert, b.ServerKey, err = issue(srvTmpl, ca, caKey); err != nil {
		return Bundle{}, err
	}

	cliTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: clientCN},
		NotBefore:    now,
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if b.ClientCert, b.ClientKey, err = issue(cliTmpl, ca, caKey); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // the system's random source failed
	}
	return n
}

func pemCert(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func issue(tmpl *x509.Certificate, ca *x509.Certificate, caKey *ecdsa.PrivateKey) (certPEM, keyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return pemCert(der), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), nil
}

// ---- reading files, and again when they change ----

// recheck is how often the files are looked at for changes. Tests shorten it.
var recheck = 30 * time.Second

type fileState struct {
	mod  time.Time
	size int64
}

func stat(paths ...string) ([]fileState, error) {
	out := make([]fileState, len(paths))
	for i, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		out[i] = fileState{fi.ModTime(), fi.Size()}
	}
	return out, nil
}

func same(a, b []fileState) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// loader holds a value built from files and rebuilds it when the files change. A failed rebuild
// keeps the value in use: a half-written file during rotation must not break the link.
type loader[T any] struct {
	paths []string
	build func() (T, error)

	mu      sync.Mutex
	value   T
	state   []fileState
	checked time.Time
}

func newLoader[T any](build func() (T, error), paths ...string) (*loader[T], error) {
	l := &loader[T]{paths: paths, build: build}
	st, err := stat(paths...)
	if err != nil {
		return nil, err
	}
	if l.value, err = build(); err != nil {
		return nil, err
	}
	l.state, l.checked = st, time.Now()
	return l, nil
}

func (l *loader[T]) get() T {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.checked) < recheck {
		return l.value
	}
	l.checked = time.Now()
	st, err := stat(l.paths...)
	if err != nil || same(st, l.state) {
		return l.value
	}
	if v, err := l.build(); err == nil {
		l.value, l.state = v, st
	}
	return l.value
}

func loadPair(certFile, keyFile string) func() (*tls.Certificate, error) {
	return func() (*tls.Certificate, error) {
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("certificate %s with key %s: %w", certFile, keyFile, err)
		}
		return &c, nil
	}
}

func loadPool(caFile string) func() (*x509.CertPool, error) {
	return func() (*x509.CertPool, error) {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("%s holds no certificate", caFile)
		}
		return pool, nil
	}
}

// ServerConfig is the TLS configuration of the backend's internal listener: it presents certFile
// and only accepts clients whose certificate was issued by a CA in clientCAFile.
func ServerConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" || clientCAFile == "" {
		return nil, errors.New("mutual TLS needs a certificate, its key and the CA of the clients")
	}
	cert, err := newLoader(loadPair(certFile, keyFile), certFile, keyFile)
	if err != nil {
		return nil, err
	}
	pool, err := newLoader(loadPool(clientCAFile), clientCAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert.get(), nil },
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{
				MinVersion:     tls.VersionTLS13,
				ClientAuth:     tls.RequireAndVerifyClientCert,
				ClientCAs:      pool.get(),
				GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert.get(), nil },
			}, nil
		},
	}, nil
}

// ClientConfig is the TLS configuration of the frontend's calls: it only trusts a backend with a
// certificate from the CA in caFile, and presents certFile as its own. serverName, if not empty, is
// the name the backend's certificate must carry (otherwise the host of the URL).
func ClientConfig(caFile, certFile, keyFile, serverName string) (*tls.Config, error) {
	if caFile == "" || certFile == "" || keyFile == "" {
		return nil, errors.New("mutual TLS needs the CA of the backend, a client certificate and its key")
	}
	cert, err := newLoader(loadPair(certFile, keyFile), certFile, keyFile)
	if err != nil {
		return nil, err
	}
	pool, err := newLoader(loadPool(caFile), caFile)
	if err != nil {
		return nil, err
	}
	// RootCAs is a pool fixed at creation, so the verification is done by hand against the pool in
	// force, which lets a rotated CA take effect without a restart.
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         serverName,
		InsecureSkipVerify: true, // replaced by VerifyConnection below, which checks everything
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return cert.get(), nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the backend presented no certificate")
			}
			opts := x509.VerifyOptions{
				Roots:         pool.get(),
				DNSName:       cs.ServerName,
				Intermediates: x509.NewCertPool(),
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}
			for _, c := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(opts)
			return err
		},
	}, nil
}
