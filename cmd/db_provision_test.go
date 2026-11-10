package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDBProvisionValidatesItsFlags(t *testing.T) {
	dir := t.TempDir()
	file := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o400); err != nil {
			t.Fatal(err)
		}
		return p
	}
	reset := func() {
		for _, f := range []string{"admin-dsn", "admin-dsn-file", "app-password-file", "body-password-file"} {
			dbProvisionCmd.Flags().Set(f, "")
			dbProvisionCmd.Flags().Lookup(f).Changed = false
		}
	}
	t.Cleanup(reset)

	cases := map[string]struct {
		flags map[string]string
		want  string
	}{
		"a password on the command line": {
			map[string]string{"admin-dsn": "postgres://postgres:hunter2hunter2@db/postgres"}, "contains a password"},
		"no app password file": {
			map[string]string{"admin-dsn-file": file("dsn", "postgres://postgres:x@db/postgres")}, "--app-password-file is required"},
		"no body password file": {
			map[string]string{"admin-dsn-file": file("dsn2", "postgres://postgres:x@db/postgres"), "app-password-file": file("app", "a")}, "--body-password-file is required"},
		"an empty password file": {
			map[string]string{"admin-dsn-file": file("dsn3", "postgres://postgres:x@db/postgres"), "app-password-file": file("empty", "\n"), "body-password-file": file("b", "b")}, "is empty"},
		"no admin connection string": {
			map[string]string{"app-password-file": file("a2", "a"), "body-password-file": file("b2", "b")}, "administrator connection string is required"},
	}
	for name, c := range cases {
		reset()
		for k, v := range c.flags {
			if err := dbProvisionCmd.Flags().Set(k, v); err != nil {
				t.Fatal(err)
			}
		}
		err := dbProvisionCmd.RunE(dbProvisionCmd, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the password was echoed: %v", name, err)
		}
	}
}
