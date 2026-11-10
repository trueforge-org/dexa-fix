package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The Valkey URL may hold a password, so it follows the rule of the DSN: a password on the command
// line is refused (other users of the machine can read it), a file or the environment is not.
func TestValkeyURLNeverTakesAPasswordOnTheCommandLine(t *testing.T) {
	newCmd := func() *cobra.Command {
		c := &cobra.Command{Use: "x"}
		c.Flags().String("valkey-url", "", "")
		c.Flags().String("valkey-url-file", "", "")
		return c
	}
	resolve := func(c *cobra.Command) (string, error) {
		return secretFlag(c, "valkey-url", "valkey-url-file", dsnHasPassword)
	}

	c := newCmd()
	c.Flags().Set("valkey-url", "redis://:hunter2hunter2@valkey:6379/0")
	if _, err := resolve(c); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("a password on the command line must be refused without quoting it: %v", err)
	}

	c = newCmd()
	c.Flags().Set("valkey-url", "redis://valkey:6379/0")
	if got, err := resolve(c); err != nil || got != "redis://valkey:6379/0" {
		t.Errorf("a URL without a password is fine: %q %v", got, err)
	}

	path := filepath.Join(t.TempDir(), "valkey_url")
	if err := os.WriteFile(path, []byte("redis://:from-a-file@valkey:6379/0\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	c = newCmd()
	c.Flags().Set("valkey-url-file", path)
	if got, err := resolve(c); err != nil || got != "redis://:from-a-file@valkey:6379/0" {
		t.Errorf("a file may hold the password: %q %v", got, err)
	}

	if got, err := resolve(newCmd()); err != nil || got != "" {
		t.Errorf("no Valkey configured must stay off: %q %v", got, err)
	}
}

func TestServeAPIHasTheValkeyFlags(t *testing.T) {
	for _, name := range []string{"valkey-url", "valkey-url-file", "health-listen"} {
		if serveAPICmd.Flags().Lookup(name) == nil {
			t.Errorf("serve api lacks --%s", name)
		}
	}
	for _, c := range []*cobra.Command{serveWebCmd, serveWorkerCmd} {
		if c.Flags().Lookup("valkey-url") != nil {
			t.Errorf("%s must not take --valkey-url: only the api process holds this state", c.Name())
		}
	}
}
