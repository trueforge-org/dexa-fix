package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

func TestDSNHasPassword(t *testing.T) {
	for dsn, want := range map[string]bool{
		"file:dexa-fix.db":                               false,
		"file:/data/x.db?_pragma=busy_timeout(5000)":     false,
		"postgres://user@localhost/db":                   false,
		"postgres://user:secret@localhost/db":            true,
		"postgres://localhost/db?user=a&password=secret": true,
		"postgres://localhost/db?sslmode=require":        false,
		"host=localhost user=a password=secret dbname=x": true,
		"host=localhost user=a dbname=x":                 false,
		"host=localhost PASSWORD = secret":               true,
		"postgresql://user:@localhost/db":                true, // an empty password is still a password field
	} {
		if got := dsnHasPassword(dsn); got != want {
			t.Errorf("dsnHasPassword(%q) = %v, want %v", dsn, got, want)
		}
	}
}

func newDSNCommand() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("dsn", "file:default.db", "")
	c.Flags().String("dsn-file", "", "")
	return c
}

func TestPasswordInDSNFlagIsRefused(t *testing.T) {
	c := newDSNCommand()
	c.Flags().Set("dsn", "postgres://u:secret@h/db")
	if _, err := dsnFlag(c, "dsn"); err == nil || !strings.Contains(err.Error(), "--dsn-file") {
		t.Fatalf("password on the command line accepted: %v", err)
	}

	// The same value from the environment (the flag's default) is fine: it never shows in ps.
	c = newDSNCommand()
	c.Flags().Lookup("dsn").DefValue = "postgres://u:secret@h/db"
	c.Flags().Lookup("dsn").Value.Set("postgres://u:secret@h/db")
	c.Flags().Lookup("dsn").Changed = false
	if got, err := dsnFlag(c, "dsn"); err != nil || !strings.Contains(got, "secret") {
		t.Fatalf("environment DSN: %q %v", got, err)
	}

	// Without a password a flag is fine.
	c = newDSNCommand()
	c.Flags().Set("dsn", "file:other.db")
	if got, err := dsnFlag(c, "dsn"); err != nil || got != "file:other.db" {
		t.Fatalf("plain DSN: %q %v", got, err)
	}
}

func TestDSNFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dsn")
	os.WriteFile(path, []byte("postgres://u:secret@h/db\n"), 0o600)
	c := newDSNCommand()
	c.Flags().Set("dsn-file", path)
	if got, err := dsnFlag(c, "dsn"); err != nil || got != "postgres://u:secret@h/db" {
		t.Fatalf("file DSN: %q %v", got, err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(empty, []byte("\n"), 0o600)
	c.Flags().Set("dsn-file", empty)
	if _, err := dsnFlag(c, "dsn"); err == nil {
		t.Error("empty secret file accepted")
	}
	c.Flags().Set("dsn-file", filepath.Join(t.TempDir(), "missing"))
	if _, err := dsnFlag(c, "dsn"); err == nil {
		t.Error("missing secret file accepted")
	}
}

func TestWithingsSecretIsNeverAFlagValue(t *testing.T) {
	c := &cobra.Command{}
	c.Flags().String("api-secret", "", "")
	c.Flags().String("api-secret-file", "", "")
	c.Flags().Set("api-secret", "hunter2")
	always := func(string) bool { return true }
	if _, err := secretFlag(c, "api-secret", "api-secret-file", always); err == nil {
		t.Fatal("secret accepted on the command line")
	}
	path := filepath.Join(t.TempDir(), "s")
	os.WriteFile(path, []byte("from-file\n"), 0o600)
	c.Flags().Set("api-secret-file", path)
	if got, err := secretFlag(c, "api-secret", "api-secret-file", always); err != nil || got != "from-file" {
		t.Fatalf("file secret: %q %v", got, err)
	}
}

func TestConfirmRollback(t *testing.T) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, "file:"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	c := &cobra.Command{}
	c.Flags().Bool("yes", false, "")
	var stderr bytes.Buffer
	c.SetErr(&stderr)

	if err := confirmRollback(ctx, c, st, 0); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("rollback without --yes: %v", err)
	}
	if !strings.Contains(stderr.String(), "00001_init.sql") || !strings.Contains(stderr.String(), "drops") {
		t.Errorf("no explanation of what is dropped:\n%s", stderr.String())
	}
	if err := confirmRollback(ctx, c, st, 5); err != nil {
		t.Errorf("moving up needs no confirmation: %v", err)
	}
	c.Flags().Set("yes", "true")
	if err := confirmRollback(ctx, c, st, 0); err != nil {
		t.Errorf("rollback with --yes: %v", err)
	}
}
