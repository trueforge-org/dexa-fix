package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

// The web (frontend) and api (backend) processes prove to each other with a shared secret in a
// file. The first line is the secret the web process sends; the backend accepts every line, so a
// secret is rotated by adding a new first line, restarting both, and removing the old line later.

// minServiceSecretLen is the shortest accepted secret, in characters. `secrets service-secret`
// makes 64 hex characters (256 bits).
const minServiceSecretLen = 32

func addServiceSecretFlag(c *cobra.Command) {
	c.Flags().String("service-secret-file", envOr("DEXAFIX_SERVICE_SECRET_FILE", ""),
		"file with the secret the web and api processes share, one per line (the first is the one sent); create one with `dexa-fix secrets service-secret`")
}

// loadServiceSecrets reads the secrets in a file. Blank lines and lines starting with # are skipped.
func loadServiceSecrets(path string) ([]string, error) {
	if path == "" {
		return nil, errors.New("--service-secret-file (DEXAFIX_SERVICE_SECRET_FILE) is required: create one with `dexa-fix secrets service-secret`")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("service secret: %w", err)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Warn().Str("file", path).Str("mode", fmt.Sprintf("%04o", fi.Mode().Perm())).
			Msg("the service secret file can be read by other users; restrict it (chmod 600) unless a secret store mounts it for you")
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) < minServiceSecretLen {
			return nil, fmt.Errorf("%s: a service secret must be at least %d characters; create one with `dexa-fix secrets service-secret`", path, minServiceSecretLen)
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s holds no secret", path)
	}
	return out, nil
}

func newServiceSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Create secrets for a deployment",
}

var serviceSecretCmd = &cobra.Command{
	Use:   "service-secret",
	Short: "Print a new secret for the web and api processes to share",
	Long: "Prints a new random secret. Save it in a file readable only by the two processes and give the path " +
		"to both with --service-secret-file:\n\n  dexa-fix secrets service-secret > service.secret && chmod 600 service.secret",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := newServiceSecret()
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), s)
		return nil
	},
}

func init() {
	secretsCmd.AddCommand(serviceSecretCmd)
	RootCmd.AddCommand(secretsCmd)
}
