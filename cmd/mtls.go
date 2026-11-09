package cmd

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/shared/mtls"
)

// Mutual TLS between the web and api processes. It comes on top of the shared service secret, which
// stays required: with both, a stolen secret is useless without a client certificate and the other
// way round. The certificates are files; they are read again when they change, so rotating them
// needs no restart. `dexa-fix secrets mtls` makes a set.

func addInternalTLSFlags(c *cobra.Command) {
	c.Flags().String("internal-tls-cert-file", envOr("DEXAFIX_INTERNAL_TLS_CERT_FILE", ""), "serve the internal API over mutual TLS: certificate of this process (PEM)")
	c.Flags().String("internal-tls-key-file", envOr("DEXAFIX_INTERNAL_TLS_KEY_FILE", ""), "serve the internal API over mutual TLS: private key of the certificate (PEM)")
	c.Flags().String("internal-tls-client-ca-file", envOr("DEXAFIX_INTERNAL_TLS_CLIENT_CA_FILE", ""), "serve the internal API over mutual TLS: CA that issued the web process's client certificate (PEM)")
}

func addBackendTLSFlags(c *cobra.Command) {
	c.Flags().String("backend-ca-file", envOr("DEXAFIX_BACKEND_CA_FILE", ""), "mutual TLS to the api process: CA that issued its certificate (PEM)")
	c.Flags().String("backend-client-cert-file", envOr("DEXAFIX_BACKEND_CLIENT_CERT_FILE", ""), "mutual TLS to the api process: client certificate of this process (PEM)")
	c.Flags().String("backend-client-key-file", envOr("DEXAFIX_BACKEND_CLIENT_KEY_FILE", ""), "mutual TLS to the api process: private key of the client certificate (PEM)")
	c.Flags().String("backend-server-name", envOr("DEXAFIX_BACKEND_SERVER_NAME", ""), "mutual TLS to the api process: the name its certificate must carry, if not the host of --backend-url")
}

// internalServerTLS returns the TLS configuration of the internal listener, or nil for plain HTTP.
// Either all three files are given or none.
func internalServerTLS(cmd *cobra.Command) (*tls.Config, error) {
	cert, _ := cmd.Flags().GetString("internal-tls-cert-file")
	key, _ := cmd.Flags().GetString("internal-tls-key-file")
	ca, _ := cmd.Flags().GetString("internal-tls-client-ca-file")
	if cert == "" && key == "" && ca == "" {
		return nil, nil
	}
	if cert == "" || key == "" || ca == "" {
		return nil, errors.New("mutual TLS on the internal API needs --internal-tls-cert-file, --internal-tls-key-file and --internal-tls-client-ca-file together")
	}
	cfg, err := mtls.ServerConfig(cert, key, ca)
	if err != nil {
		return nil, fmt.Errorf("internal TLS: %w", err)
	}
	cfg.NextProtos = []string{"http/1.1"}
	return cfg, nil
}

// backendHTTPClient returns the client for the api process and whether the link is secured by TLS.
// An https:// address needs the three mutual TLS files, and the files need an https:// address.
func backendHTTPClient(cmd *cobra.Command, backendURL string) (*http.Client, bool, error) {
	ca, _ := cmd.Flags().GetString("backend-ca-file")
	cert, _ := cmd.Flags().GetString("backend-client-cert-file")
	key, _ := cmd.Flags().GetString("backend-client-key-file")
	name, _ := cmd.Flags().GetString("backend-server-name")
	u, err := url.Parse(backendURL)
	if err != nil || u.Host == "" {
		return nil, false, fmt.Errorf("--backend-url %q: use the form http://host:port or https://host:port", backendURL)
	}
	files := ca != "" || cert != "" || key != ""
	switch {
	case u.Scheme == "https" && !(ca != "" && cert != "" && key != ""):
		return nil, false, errors.New("an https:// --backend-url needs mutual TLS: --backend-ca-file, --backend-client-cert-file and --backend-client-key-file")
	case u.Scheme == "http" && files:
		return nil, false, errors.New("--backend-ca-file and the client certificate flags need an https:// --backend-url")
	case u.Scheme != "https":
		return apiclient.HTTPClient(nil), false, nil
	}
	cfg, err := mtls.ClientConfig(ca, cert, key, name)
	if err != nil {
		return nil, false, fmt.Errorf("mutual TLS to the backend: %w", err)
	}
	return apiclient.HTTPClient(cfg), true, nil
}

// ---- dexa-fix secrets mtls ----

var secretsMTLSCmd = &cobra.Command{
	Use:   "mtls",
	Short: "Create the certificates for mutual TLS between the web and api processes",
	Long: `Creates a private CA and two certificates in a directory: one for the api process (it
presents it to the web process) and one for the web process (it presents it to the api process).
The CA's private key is not kept, so nothing else can be issued from it; to rotate, create a new set.
Files that exist are never overwritten.

Files written:
  ca.pem                 the CA, for both processes
  api.pem, api.key       the api process's certificate and key
  web.pem, web.key       the web process's certificate and key

Use --name for each host name or address the web process reaches the api process by.`,
	Example: `dexa-fix secrets mtls --dir ./mtls --name api
# api:  --internal-tls-cert-file mtls/api.pem --internal-tls-key-file mtls/api.key --internal-tls-client-ca-file mtls/ca.pem
# web:  --backend-url https://api:8081 --backend-ca-file mtls/ca.pem --backend-client-cert-file mtls/web.pem --backend-client-key-file mtls/web.key`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, _ := cmd.Flags().GetString("dir")
		names, _ := cmd.Flags().GetStringSlice("name")
		if len(names) == 0 {
			return errors.New("give at least one --name: the host name or address the web process uses for the api process, such as api")
		}
		b, err := mtls.Generate(names)
		if err != nil {
			return err
		}
		files := []struct {
			name, content string
			mode          os.FileMode
		}{
			{"ca.pem", b.CA, 0o644},
			{"api.pem", b.ServerCert, 0o644}, {"api.key", b.ServerKey, 0o600},
			{"web.pem", b.ClientCert, 0o644}, {"web.key", b.ClientKey, 0o600},
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		for _, f := range files { // check first, so a set is never half written
			if _, err := os.Stat(filepath.Join(dir, f.name)); err == nil {
				return fmt.Errorf("%s already exists: nothing was written", filepath.Join(dir, f.name))
			}
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.content), f.mode); err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "created", filepath.Join(dir, f.name))
		}
		fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(`
api process:  --internal-tls-cert-file api.pem --internal-tls-key-file api.key --internal-tls-client-ca-file ca.pem
web process:  --backend-url https://`+names[0]+`:8081 --backend-ca-file ca.pem --backend-client-cert-file web.pem --backend-client-key-file web.key`))
		return nil
	},
}

func init() {
	secretsMTLSCmd.Flags().String("dir", "mtls", "directory to write the files to")
	secretsMTLSCmd.Flags().StringSlice("name", nil, "host name or address the web process reaches the api process by (repeatable)")
	secretsCmd.AddCommand(secretsMTLSCmd)
}
