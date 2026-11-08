package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveEncryptionKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.key")
	first, created, err := resolveEncryptionKey("", path, nil)
	if err != nil || !created || len(first) != 32 {
		t.Fatalf("create: %d %v %v", len(first), created, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
	second, created, err := resolveEncryptionKey("", path, nil)
	if err != nil || created || !bytes.Equal(first, second) {
		t.Fatalf("reuse: %v %v", created, err)
	}
}

func TestResolveEncryptionKeyFromEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unused.key")
	key, created, err := resolveEncryptionKey(strings.Repeat("ab", 32), path, nil)
	if err != nil || created || len(key) != 32 {
		t.Fatalf("env: %d %v %v", len(key), created, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("key file must not be created: %v", err)
	}
	if _, _, err := resolveEncryptionKey("too short", path, nil); err == nil {
		t.Fatal("bad key accepted")
	}
}

func TestResolveEncryptionKeyRejectsDamagedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.key")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveEncryptionKey("", path, nil); err == nil {
		t.Fatal("damaged key file accepted")
	}
}
