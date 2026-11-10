// Package tlsutil checks PEM certificates; the frontend serves them and the backend stores them.
package tlsutil

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Info describes the certificate in force, for the Admin page.
type Info struct {
	Subject     string
	Names       []string
	NotAfter    time.Time
	Fingerprint string // SHA-256 of the certificate
}

// Parse checks a PEM certificate chain and its private key.
func Parse(certPEM, keyPEM string) (*tls.Certificate, *Info, error) {
	if strings.TrimSpace(certPEM) == "" || strings.TrimSpace(keyPEM) == "" {
		return nil, nil, errors.New("both a certificate and a private key are needed")
	}
	c, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, nil, fmt.Errorf("the certificate and key do not belong together or are not PEM: %w", err)
	}
	leaf := c.Leaf // tls.X509KeyPair has parsed it since Go 1.23
	if time.Now().After(leaf.NotAfter) {
		return nil, nil, fmt.Errorf("the certificate expired on %s", leaf.NotAfter.UTC().Format("2006-01-02"))
	}
	sum := sha256.Sum256(leaf.Raw)
	names := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	return &c, &Info{Subject: leaf.Subject.String(), Names: names, NotAfter: leaf.NotAfter, Fingerprint: hex.EncodeToString(sum[:])}, nil
}
