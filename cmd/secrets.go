package cmd

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// Secrets never travel on the command line: other users of the machine can read it with ps, and
// shells keep it in their history. They come from the environment or from a file instead.

// readSecretFile returns the trimmed contents of a file that holds a single secret.
func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return v, nil
}

var dsnKeyPassword = regexp.MustCompile(`(?i)(^|\s)password\s*=`)

// dsnHasPassword reports whether a connection string carries a password, in URL form
// (postgres://user:secret@host/db, ?password=...) or key/value form (host=... password=...).
func dsnHasPassword(dsn string) bool {
	if dsnKeyPassword.MatchString(dsn) {
		return true
	}
	if !strings.Contains(dsn, "://") {
		return false
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return strings.Contains(strings.ToLower(dsn), "password=") // unparsable: assume the worst
	}
	if _, has := u.User.Password(); has {
		return true
	}
	for k := range u.Query() {
		if strings.EqualFold(k, "password") {
			return true
		}
	}
	return false
}

// secretFlag resolves a value that may hold a secret. Order: the file named by fileFlag, then the
// flag itself. The flag's default already comes from the environment, so a value that was typed on
// the command line (Changed) is the only one that is refused when it contains a password.
func secretFlag(cmd *cobra.Command, flag, fileFlag string, hasSecret func(string) bool) (string, error) {
	if path, _ := cmd.Flags().GetString(fileFlag); path != "" {
		return readSecretFile(path)
	}
	v, _ := cmd.Flags().GetString(flag)
	if cmd.Flags().Changed(flag) && hasSecret(v) {
		return "", fmt.Errorf("--%s contains a password, which would be visible to other users of this machine: "+
			"put it in an environment variable or in a file and use --%s", flag, fileFlag)
	}
	return v, nil
}

// dsnFlag resolves a database connection string.
func dsnFlag(cmd *cobra.Command, flag string) (string, error) {
	return secretFlag(cmd, flag, flag+"-file", dsnHasPassword)
}
