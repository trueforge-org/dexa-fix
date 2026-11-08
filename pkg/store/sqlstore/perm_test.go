package sqlstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteFilePath(t *testing.T) {
	for dsn, want := range map[string]string{
		"file:dexa-fix.db":                        "dexa-fix.db",
		"file:/data/x.db?_pragma=busy_timeout(5)": "/data/x.db",
		"file:///data/x.db":                       "/data/x.db",
		"plain.db":                                "plain.db",
		"file::memory:?cache=shared":              "",
		"file:t?mode=memory&cache=shared":         "",
	} {
		if got := sqliteFilePath(dsn); got != want {
			t.Errorf("sqliteFilePath(%q) = %q, want %q", dsn, got, want)
		}
	}
}

func TestNewDatabaseFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	s, err := Open(context.Background(), DriverSQLite, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if err != nil {
			continue // the WAL files only exist while the database is open and written to
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is accessible to others: %v", filepath.Base(p), info.Mode().Perm())
		}
	}
	s.Close()
}

func TestExistingDatabaseFileIsNotChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), DriverSQLite, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Errorf("mode changed to %v", info.Mode().Perm())
	}
}
