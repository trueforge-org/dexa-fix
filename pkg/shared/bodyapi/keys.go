package bodyapi

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Keys are files of text so that they can be mounted as secrets. A private key is the base64 of its
// 32-byte seed and a public key the base64 of its 32 bytes, each with a label in front. The labels
// are there because the two have the same size: a private key must never be taken for a public one,
// since the body service is meant to hold the public key only. Blank lines and lines starting with #
// are skipped, so a file can carry a comment and, for public keys, several keys while one is replaced.
const (
	privateLabel = "ed25519-private:"
	publicLabel  = "ed25519-public:"
)

// EncodePrivateKey writes a private key as text.
func EncodePrivateKey(priv ed25519.PrivateKey) string {
	return privateLabel + base64.StdEncoding.EncodeToString(priv.Seed())
}

// EncodePublicKey writes a public key as text.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return publicLabel + base64.StdEncoding.EncodeToString(pub)
}

func keyLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// ParsePrivateKey reads the private key of a key file. A file holds exactly one.
func ParsePrivateKey(text string) (ed25519.PrivateKey, error) {
	lines := keyLines(text)
	if len(lines) != 1 {
		return nil, fmt.Errorf("a signing key file holds one key, found %d", len(lines))
	}
	text, ok := strings.CutPrefix(lines[0], privateLabel)
	if !ok {
		return nil, errors.New("this is not a signing key (it should start with " + privateLabel + "): create one with `dexafix secrets body-token-key`")
	}
	seed, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("the signing key is damaged: create a new one with `dexafix secrets body-token-key`")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// ParsePublicKeys reads the public keys of a key file, one per line. At least one is required.
func ParsePublicKeys(text string) ([]ed25519.PublicKey, error) {
	var out []ed25519.PublicKey
	for _, line := range keyLines(text) {
		if strings.HasPrefix(line, privateLabel) {
			return nil, errors.New("this is a PRIVATE key: the body service must hold the public key only, so it can check tokens but never make one (create it with `dexafix secrets body-token-public`)")
		}
		rest, ok := strings.CutPrefix(line, publicLabel)
		if !ok {
			return nil, errors.New("this is not a public key (it should start with " + publicLabel + "): create one with `dexafix secrets body-token-public`")
		}
		b, err := base64.StdEncoding.DecodeString(rest)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, errors.New("the public key is damaged: create a new one with `dexafix secrets body-token-public`")
		}
		out = append(out, ed25519.PublicKey(b))
	}
	if len(out) == 0 {
		return nil, errors.New("the file holds no public key")
	}
	return out, nil
}
