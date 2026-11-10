//go:build linux

package cmd

import (
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// withFileSizeLimit runs f with files unable to grow past one byte, which is what a full disk looks like
// to a program: the write fails.
func withFileSizeLimit(t *testing.T, f func()) {
	t.Helper()
	signal.Ignore(syscall.SIGXFSZ) // the kernel signals the program that went over the limit
	t.Cleanup(func() { signal.Reset(syscall.SIGXFSZ) })
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1, Max: old.Max}); err != nil {
		t.Skipf("cannot limit the file size: %v", err)
	}
	defer func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
			t.Fatal(err)
		}
	}()
	f()
}

func TestANewEncryptionKeyFileThatCannotBeWrittenIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.key")
	var err error
	withFileSizeLimit(t, func() { _, _, err = resolveEncryptionKey("", path, nil) })
	if err == nil || !strings.Contains(err.Error(), "write encryption key file") {
		t.Fatalf("%v", err)
	}
}

// A file that cannot grow stops init without leaving a half-written secret behind. The limit is
// what a full disk looks like to a program: the write fails.
func TestSecretsInitLeavesNoHalfWrittenFileWhenTheDiskIsFull(t *testing.T) {
	o, _ := testInit(t)
	dir := t.TempDir()
	var err error
	withFileSizeLimit(t, func() { err = o.put(dir, "secret", "more than one byte") })
	if err == nil {
		t.Fatal("a write over the size limit was reported as done")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret")); !os.IsNotExist(err) {
		t.Errorf("the secret exists: %v", err)
	}
}
