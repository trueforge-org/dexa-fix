//go:build linux

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// becomeUser makes this process an ordinary user for the rest of the test, as the init container is.
// Folders that the test makes before it stay with root.
func becomeUser(t *testing.T, uid int) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to become somebody else for a moment")
	}
	if err := syscallSetEuid(uid); err != nil {
		t.Skipf("cannot become user %d: %v", uid, err)
	}
	t.Cleanup(func() {
		if err := syscallSetEuid(0); err != nil {
			t.Fatalf("could not become root again: %v", err)
		}
	})
}

// reachable lets every user walk down to dir.
func reachable(t *testing.T, dir string) {
	t.Helper()
	for p := filepath.Dir(dir); strings.HasPrefix(p, os.TempDir()) && p != os.TempDir(); p = filepath.Dir(p) {
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// Init as the ordinary user of the containers, in a folder that belongs to root: it may not change
// the mode of it, and says what to do instead of "operation not permitted".
func TestSecretsInitAsAnotherUserStopsWithAdviceInAFolderOfRoot(t *testing.T) {
	o, dirs := testInit(t)
	if err := os.MkdirAll(dirs["host"], 0o755); err != nil {
		t.Fatal(err)
	}
	reachable(t, dirs["host"])
	becomeUser(t, 65534)
	err := o.run()
	if err == nil || !strings.Contains(err.Error(), "--user 0:0") || !strings.Contains(err.Error(), "chmod") {
		t.Fatalf("want the advice to run init as root once, got %v", err)
	}
}

// Giving a file to somebody else is for root only: the user of the containers cannot, and is told.
func TestSecretsInitAsAnotherUserCannotGiveFilesAway(t *testing.T) {
	o, dirs := testInit(t)
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(d, 65534, 65534); err != nil {
			t.Fatal(err)
		}
	}
	reachable(t, dirs["host"])
	o.uid, o.gid = 12345, 12345
	becomeUser(t, 65534)
	err := o.run()
	if err == nil || !strings.Contains(err.Error(), "give db_admin_password to 12345:12345") || !strings.Contains(err.Error(), "--user 0:0") {
		t.Fatalf("want the failure to give a file away, with the advice: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dirs["host"])); len(entries) != 0 {
		t.Errorf("a file was left behind: %v", entries)
	}
}
