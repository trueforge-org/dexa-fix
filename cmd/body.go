package cmd

import (
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	bodybackend "github.com/trueforge-org/dexa-fix/pkg/backend/body"
	"github.com/trueforge-org/dexa-fix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexa-fix/pkg/shared/mtls"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

// The body service holds the scale readings, the DEXA scans and the estimates. It can run inside the
// api process (the default), or as a process of its own, with a database, a key and a secret of its
// own:
//
//	dexa-fix serve body     the service. Knows nothing about people; trusts only the tokens of the api.
//	dexa-fix serve api --body-url ...   calls it, with a short-lived signed token for every call.
//
// Three things tie the two together, and each is a file: the body secret (the same file in both, like
// the service and worker secrets), the signing key (private for the api, public for the body service)
// and, optionally, mutual TLS on top.

var bodySecretFile = secretFile{"body secret", "--body-secret-file", "DEXAFIX_BODY_SECRET_FILE", "dexa-fix secrets body-secret"}

func addBodySecretFlag(c *cobra.Command, help string) {
	c.Flags().String("body-secret-file", envOr("DEXAFIX_BODY_SECRET_FILE", ""), help)
}

// addBodyClientFlags registers the flags with which the api process reaches a body service of its own.
func addBodyClientFlags(c *cobra.Command) {
	c.Flags().String("body-url", envOr("DEXAFIX_BODY_URL", ""), "address of the body service, such as http://body:8082 (empty: run the body service inside this process)")
	addBodySecretFlag(c, "file with the secret the api process and the body service share, one per line (the first is the one sent); create one with `dexa-fix secrets body-secret`; it must differ from the service and worker secrets")
	c.Flags().String("body-token-key-file", envOr("DEXAFIX_BODY_TOKEN_KEY_FILE", ""), "file with the private key that signs the tokens for the body service; create one with `dexa-fix secrets body-token-key`")
	c.Flags().String("body-ca-file", envOr("DEXAFIX_BODY_CA_FILE", ""), "mutual TLS to the body service: CA that issued its certificate (PEM)")
	c.Flags().String("body-client-cert-file", envOr("DEXAFIX_BODY_CLIENT_CERT_FILE", ""), "mutual TLS to the body service: client certificate of this process (PEM)")
	c.Flags().String("body-client-key-file", envOr("DEXAFIX_BODY_CLIENT_KEY_FILE", ""), "mutual TLS to the body service: private key of the client certificate (PEM)")
	c.Flags().String("body-server-name", envOr("DEXAFIX_BODY_SERVER_NAME", ""), "mutual TLS to the body service: the name its certificate must carry, if not the host of --body-url")
}

// bodyClient returns the client for a body service in another process, or remote == false when
// --body-url is empty and the service runs inside this process. The other body flags need the URL,
// and a URL needs the secret and the signing key. An https:// address needs the three mutual TLS
// files, and the files need an https:// address. The secrets of the link are returned so the caller
// can check them against the others.
func bodyClient(cmd *cobra.Command) (c *bodyapi.Client, secrets []string, remote bool, err error) {
	f := cmd.Flags()
	rawURL, _ := f.GetString("body-url")
	secretPath, _ := f.GetString("body-secret-file")
	keyPath, _ := f.GetString("body-token-key-file")
	ca, _ := f.GetString("body-ca-file")
	cert, _ := f.GetString("body-client-cert-file")
	key, _ := f.GetString("body-client-key-file")
	name, _ := f.GetString("body-server-name")
	if rawURL == "" {
		if secretPath != "" || keyPath != "" || ca != "" || cert != "" || key != "" || name != "" {
			return nil, nil, false, errors.New("the --body-* flags need --body-url: without it the body service runs inside this process")
		}
		return nil, nil, false, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, nil, false, fmt.Errorf("--body-url %q: use the form http://host:port or https://host:port", rawURL)
	}
	files := ca != "" || cert != "" || key != ""
	switch {
	case u.Scheme == "https" && !(ca != "" && cert != "" && key != ""):
		return nil, nil, false, errors.New("an https:// --body-url needs mutual TLS: --body-ca-file, --body-client-cert-file and --body-client-key-file")
	case u.Scheme == "http" && files:
		return nil, nil, false, errors.New("--body-ca-file and the client certificate flags need an https:// --body-url")
	}
	if secrets, err = bodySecretFile.load(secretPath); err != nil {
		return nil, nil, false, err
	}
	if keyPath == "" {
		return nil, nil, false, errors.New("--body-token-key-file (DEXAFIX_BODY_TOKEN_KEY_FILE) is required with --body-url: create one with `dexa-fix secrets body-token-key`")
	}
	priv, err := readSigningKey(keyPath)
	if err != nil {
		return nil, nil, false, err
	}
	var tlsConfig *tls.Config
	if u.Scheme == "https" {
		if tlsConfig, err = mtls.ClientConfig(ca, cert, key, name); err != nil {
			return nil, nil, false, fmt.Errorf("mutual TLS to the body service: %w", err)
		}
	} else if !privateHost(u.Hostname()) {
		log.Warn().Str("body", rawURL).Msg("the body service is reached over plain HTTP at an address that is not private: " +
			"the link carries every reading and scan; use a private network or mutual TLS (dexa-fix secrets mtls --link body)")
	}
	c, err = bodyapi.New(rawURL, bodyapi.HTTPClient(tlsConfig), secrets[0], bodyapi.NewSigner(priv))
	if err != nil {
		return nil, nil, false, err
	}
	return c, secrets, true, nil
}

// readSigningKey reads the private key of the tokens. It is a secret: a file other users can read
// is reported, unless a secret store mounts it.
func readSigningKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 && !mountedSecret(path) {
		log.Warn().Str("file", path).Str("mode", fmt.Sprintf("%04o", fi.Mode().Perm())).
			Msg("the token signing key file can be read by other users; restrict it (chmod 600) unless a secret store mounts it for you")
	}
	priv, err := bodyapi.ParsePrivateKey(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return priv, nil
}

// distinctSecrets refuses a deployment in which two doors share a secret: a secret of one link must
// not open another. The map is name of the link to its secrets.
func distinctSecrets(links map[string][]string) error {
	owner := map[string]string{}
	for _, name := range []string{"service", "worker", "body"} { // fixed order for a stable message
		for _, s := range links[name] {
			if other, dup := owner[s]; dup && other != name {
				return fmt.Errorf("the %s secret and the %s secret must differ: each opens a different door", other, name)
			}
			owner[s] = name
		}
	}
	return nil
}

// ---- mutual TLS of the body service ----

func addBodyServerTLSFlags(c *cobra.Command) {
	c.Flags().String("body-tls-cert-file", envOr("DEXAFIX_BODY_TLS_CERT_FILE", ""), "serve over mutual TLS: certificate of this process (PEM)")
	c.Flags().String("body-tls-key-file", envOr("DEXAFIX_BODY_TLS_KEY_FILE", ""), "serve over mutual TLS: private key of the certificate (PEM)")
	c.Flags().String("body-tls-client-ca-file", envOr("DEXAFIX_BODY_TLS_CLIENT_CA_FILE", ""), "serve over mutual TLS: CA that issued the api process's client certificate (PEM)")
}

// bodyServerTLS returns the TLS configuration of the body service, or nil for plain HTTP. Either all
// three files are given or none.
func bodyServerTLS(cmd *cobra.Command) (*tls.Config, error) {
	cert, _ := cmd.Flags().GetString("body-tls-cert-file")
	key, _ := cmd.Flags().GetString("body-tls-key-file")
	ca, _ := cmd.Flags().GetString("body-tls-client-ca-file")
	if cert == "" && key == "" && ca == "" {
		return nil, nil
	}
	if cert == "" || key == "" || ca == "" {
		return nil, errors.New("mutual TLS on the body service needs --body-tls-cert-file, --body-tls-key-file and --body-tls-client-ca-file together")
	}
	cfg, err := mtls.ServerConfig(cert, key, ca)
	if err != nil {
		return nil, fmt.Errorf("body TLS: %w", err)
	}
	cfg.NextProtos = []string{"http/1.1"}
	return cfg, nil
}

// ---- serve body ----

var serveBodyCmd = &cobra.Command{
	Use:   "body",
	Short: "Run the body service: scale readings, DEXA scans and estimates, in a database of its own",
	Long: `Run the body service. It holds the scale readings and DEXA scans (encrypted with a key of its own)
and computes the estimates. It knows nothing about people: the api process (dexa-fix serve api
--body-url ...) authenticates them and calls this service with a short-lived token signed for one user
and one scope. The service holds only the public key, so it can check tokens but never make one.

Three files tie it to the api process:
  --body-secret-file             the shared secret (dexa-fix secrets body-secret); every call carries it
  --body-token-public-key-file   the public key of the api process's signing key
                                 (dexa-fix secrets body-token-key, then body-token-public)
  --encryption-key-file          this service's own encryption key. The same file as the api process's
                                 gives the same key; a different one keeps a leak of one from opening the other.

Keep the listener on a private network that only the api process can reach.`,
	Example: `dexa-fix secrets body-token-key > body-token.key && chmod 600 body-token.key
dexa-fix secrets body-token-public --key-file body-token.key > body-token.pub
dexa-fix serve body --listen :8082 --db-driver postgres --dsn-file /run/secrets/body_dsn \
  --body-secret-file /run/secrets/body_secret --body-token-public-key-file /run/secrets/body_token_pub`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		listen, _ := cmd.Flags().GetString("listen")
		if listen == "" {
			return errors.New("--listen is empty: nothing to serve")
		}
		secretPath, _ := cmd.Flags().GetString("body-secret-file")
		secrets, err := bodySecretFile.load(secretPath)
		if err != nil {
			return err
		}
		pubPath, _ := cmd.Flags().GetString("body-token-public-key-file")
		if pubPath == "" {
			return errors.New("--body-token-public-key-file (DEXAFIX_BODY_TOKEN_PUBLIC_KEY_FILE) is required: create the key pair with `dexa-fix secrets body-token-key` and `dexa-fix secrets body-token-public`")
		}
		raw, err := os.ReadFile(pubPath)
		if err != nil {
			return fmt.Errorf("public key: %w", err)
		}
		pubs, err := bodyapi.ParsePublicKeys(string(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", pubPath, err)
		}
		tlsConfig, err := bodyServerTLS(cmd)
		if err != nil {
			return err
		}
		driver, dsn, err := dbFlags(cmd)
		if err != nil {
			return err
		}
		autoMigrate, _ := cmd.Flags().GetBool("auto-migrate")

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		c, err := loadCipherFor(ctx, cmd, driver, dsn, func() (bool, error) {
			return sqlstore.HasEncryptedBodyValues(ctx, driver, dsn)
		})
		if err != nil {
			return err
		}
		opts := []sqlstore.Option{sqlstore.WithCipher(c)}
		if !autoMigrate {
			opts = append(opts, sqlstore.RequireMigrated())
		}
		st, err := sqlstore.OpenBody(ctx, driver, dsn, opts...)
		if err != nil {
			return err
		}
		defer st.Close()

		svc, err := bodybackend.New(st, secrets[0], bodyapi.NewVerifier(pubs...),
			bodybackend.WithVersion(Version), bodybackend.WithSpareSecrets(secrets[1:]...))
		if err != nil {
			return err
		}
		srv := newHTTPServer(listen, svc.Handler())
		var wrap func(l net.Listener) net.Listener
		if tlsConfig != nil {
			wrap = func(l net.Listener) net.Listener { return tls.NewListener(l, tlsConfig) }
		}
		errCh := make(chan error, 1)
		if err := listenAndServe(srv, wrap, errCh); err != nil {
			return err
		}
		log.Info().Str("listen", listen).Str("db", driver).Bool("mutual_tls", tlsConfig != nil).Int("trusted_keys", len(pubs)).Msg("serving the body service")
		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			return shutdown(srv)
		}
	},
}

// ---- dexa-fix secrets body-secret, body-token-key, body-token-public ----

var secretsBodySecretCmd = &cobra.Command{
	Use:   "body-secret",
	Short: "Print a new secret for the api process and the body service to share",
	Long: "Prints a new random secret. Save it in a file readable only by the api process and the body service and give the path " +
		"to both with --body-secret-file. It must differ from the service and worker secrets: each opens a different door.\n\n" +
		"  dexa-fix secrets body-secret > body.secret && chmod 600 body.secret",
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

var secretsBodyTokenKeyCmd = &cobra.Command{
	Use:   "body-token-key",
	Short: "Print a new private key for the api process to sign body service tokens with",
	Long: "Prints a new private key. Only the api process holds it (--body-token-key-file): with it a token can be made for any user, " +
		"so keep it to that one process. The body service gets the public key (body-token-public) and can check tokens but not make them.\n\n" +
		"  dexa-fix secrets body-token-key > body-token.key && chmod 600 body-token.key",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		_, _, priv, err := bodyapi.GenerateKey()
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), bodyapi.EncodePrivateKey(priv))
		return nil
	},
}

var secretsBodyTokenPublicCmd = &cobra.Command{
	Use:   "body-token-public",
	Short: "Print the public key that belongs to a body token signing key",
	Long: "Prints the public key of the private key in --key-file, for the body service (--body-token-public-key-file). " +
		"A public key is no secret. During a key change the body service's file may hold several keys, one per line.\n\n" +
		"  dexa-fix secrets body-token-public --key-file body-token.key > body-token.pub",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path, _ := cmd.Flags().GetString("key-file")
		if path == "" {
			return errors.New("--key-file is required: the file made by `dexa-fix secrets body-token-key`")
		}
		priv, err := readSigningKey(path)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), bodyapi.EncodePublicKey(priv.Public().(ed25519.PublicKey)))
		return nil
	},
}

func init() {
	serveBodyCmd.Flags().String("listen", envOr("DEXAFIX_BODY_LISTEN", ":8082"), "address of the body service; keep it on a private network")
	addBodySecretFlag(serveBodyCmd, "file with the secret the api process and this service share, one per line (every line is accepted); create one with `dexa-fix secrets body-secret`")
	serveBodyCmd.Flags().String("body-token-public-key-file", envOr("DEXAFIX_BODY_TOKEN_PUBLIC_KEY_FILE", ""), "file with the public key(s) of the api process's token signing key, one per line; create one with `dexa-fix secrets body-token-public`")
	addBodyServerTLSFlags(serveBodyCmd)
	addDBFlags(serveBodyCmd)
	serveBodyCmd.Flags().Bool("auto-migrate", envOr("DEXAFIX_AUTO_MIGRATE", "true") != "false", "apply pending database migrations on startup (otherwise fail if any are pending)")
	addKeyFlags(serveBodyCmd)

	secretsBodyTokenPublicCmd.Flags().String("key-file", "", "file with the private key made by `dexa-fix secrets body-token-key`")
	secretsCmd.AddCommand(secretsBodySecretCmd, secretsBodyTokenKeyCmd, secretsBodyTokenPublicCmd)
	serveCmd.AddCommand(serveBodyCmd)
}
