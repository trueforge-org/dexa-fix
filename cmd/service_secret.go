package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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

// secretFile describes a file of shared secrets, for the messages that name it.
type secretFile struct {
	what   string // "service secret"
	flag   string // "--service-secret-file"
	env    string // "DEXAFIX_SERVICE_SECRET_FILE"
	create string // "dexa-fix secrets service-secret"
}

var (
	serviceSecretFile = secretFile{"service secret", "--service-secret-file", "DEXAFIX_SERVICE_SECRET_FILE", "dexa-fix secrets service-secret"}
	workerSecretFile  = secretFile{"worker secret", "--worker-secret-file", "DEXAFIX_WORKER_SECRET_FILE", "dexa-fix secrets worker-secret"}
)

// loadServiceSecrets reads the secrets in a file. Blank lines and lines starting with # are skipped.
func loadServiceSecrets(path string) ([]string, error) { return serviceSecretFile.load(path) }

// loadWorkerSecrets reads the secrets the workers and the api process share, in the same format.
func loadWorkerSecrets(path string) ([]string, error) { return workerSecretFile.load(path) }

func (k secretFile) load(path string) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("%s (%s) is required: create one with `%s`", k.flag, k.env, k.create)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k.what, err)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 && !mountedSecret(path) {
		log.Warn().Str("file", path).Str("mode", fmt.Sprintf("%04o", fi.Mode().Perm())).
			Msg("the " + k.what + " file can be read by other users; restrict it (chmod 600) unless a secret store mounts it for you")
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) < minServiceSecretLen {
			return nil, fmt.Errorf("%s: a %s must be at least %d characters; create one with `%s`", path, k.what, minServiceSecretLen, k.create)
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

var workerSecretCmd = &cobra.Command{
	Use:   "worker-secret",
	Short: "Print a new secret for the api process and its workers to share",
	Long: "Prints a new random secret. Save it in a file readable only by the api process and the workers and give the path " +
		"to both with --worker-secret-file. It must differ from the service secret: the two open different doors.\n\n" +
		"  dexa-fix secrets worker-secret > worker.secret && chmod 600 worker.secret",
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

func addWorkerSecretFlag(c *cobra.Command, help string) {
	c.Flags().String("worker-secret-file", envOr("DEXAFIX_WORKER_SECRET_FILE", ""), help)
}

func init() {
	secretsCmd.AddCommand(serviceSecretCmd, workerSecretCmd)
	RootCmd.AddCommand(secretsCmd)
}

// mountedSecret reports whether path is in one of the places where a container platform mounts
// secrets (Docker, Compose, Kubernetes). Those files are managed by the platform, which commonly makes
// them readable inside the container only, so the permission warning would just be noise.
func mountedSecret(path string) bool {
	p := filepath.Clean(path)
	return strings.HasPrefix(p, "/run/secrets/") || strings.HasPrefix(p, "/var/run/secrets/")
}
