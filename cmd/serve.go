package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
	"github.com/trueforge-org/dexa-fix/pkg/web"
)

// Version is the application version, set by main.
var Version = "dev"

// encryptionKeyEnv holds the encryption key directly. There is deliberately no flag for it, since
// command lines are visible to other processes.
const encryptionKeyEnv = "DEXAFIX_ENCRYPTION_KEY"

// resolveEncryptionKey returns the key from the environment value, or else from the key file,
// creating the file with a new random key if it does not exist.
func resolveEncryptionKey(envValue, path string) (key []byte, created bool, err error) {
	if envValue != "" {
		key, err = sqlstore.ParseKey(envValue)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", encryptionKeyEnv, err)
		}
		return key, false, nil
	}
	data, err := os.ReadFile(path)
	if err == nil {
		key, err = sqlstore.ParseKey(string(data))
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		return key, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	key = make([]byte, sqlstore.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("create encryption key file: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		return nil, false, fmt.Errorf("write encryption key file: %w", err)
	}
	return key, true, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt reads an integer from the environment, falling back to def when it is unset or invalid.
func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n >= 0 {
		return n
	}
	return def
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the web application",
	Long: `Run the web application.

Each flag can also be set through an environment variable, such as DEXAFIX_LISTEN or DEXAFIX_DSN.
A flag on the command line overrides its variable.
Secrets are never accepted as flags: a connection string with a password goes in DEXAFIX_DSN or a
file (--dsn-file).

The Withings import is set up on the Admin page (client id, secret and redirect URL), not here.`,
	Example: `dexa-fix serve --listen :8080
DEXAFIX_DSN=postgres://user:pass@localhost/dexafix dexa-fix serve --db-driver postgres
dexa-fix serve --db-driver postgres --dsn-file /run/secrets/dexafix_dsn`,
	RunE: func(cmd *cobra.Command, args []string) error {
		listen, _ := cmd.Flags().GetString("listen")
		driver, dsn, err := dbFlags(cmd)
		if err != nil {
			return err
		}
		autoMigrate, _ := cmd.Flags().GetBool("auto-migrate")
		keyFile, _ := cmd.Flags().GetString("encryption-key-file")

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		key, created, err := resolveEncryptionKey(os.Getenv(encryptionKeyEnv), keyFile)
		if err != nil {
			return err
		}
		if created {
			log.Warn().Str("file", keyFile).Msg("created a new encryption key: back it up, encrypted values cannot be read without it")
		}
		c, err := sqlstore.NewCipher(key)
		if err != nil {
			return err
		}
		opts := []sqlstore.Option{sqlstore.WithCipher(c)}
		if !autoMigrate {
			opts = append(opts, sqlstore.RequireMigrated())
		}
		st, err := sqlstore.Open(ctx, driver, dsn, opts...)
		if err != nil {
			return err
		}
		defer st.Close()

		// The Withings application credentials are managed on the Admin page only; they are stored
		// encrypted in the database. Variables from older versions are ignored, with a warning.
		for _, name := range []string{"DEXAFIX_WITHINGS_CLIENT_ID", "DEXAFIX_WITHINGS_CLIENT_SECRET", "DEXAFIX_WITHINGS_CLIENT_SECRET_FILE", "DEXAFIX_WITHINGS_REDIRECT_URL"} {
			if os.Getenv(name) != "" {
				log.Warn().Str("variable", name).Msg("ignored: Withings credentials are set on the Admin page")
			}
		}
		wi := withings.New(withings.Config{}, nil)
		srv, err := web.New(st, importer.NewRegistry(wi))
		if err != nil {
			return err
		}
		srv.Version = Version
		proxies, _ := cmd.Flags().GetString("trusted-proxies")
		if srv.TrustedProxies, err = web.ParseTrustedProxies([]string{proxies}); err != nil {
			return err
		}
		baseURL, _ := cmd.Flags().GetString("base-url")
		if srv.BaseURL, err = web.ParseBaseURL(baseURL); err != nil {
			return err
		}
		if len(srv.TrustedProxies) > 0 || srv.BaseURL != nil {
			log.Info().Str("trusted_proxies", proxies).Str("base_url", baseURL).Msg("reverse proxy settings")
		}
		days, _ := cmd.Flags().GetInt("audit-retention-days")
		srv.AuditRetention = time.Duration(days) * 24 * time.Hour
		srv.StartMaintenance()
		srv.ForcePasswordLogin, _ = cmd.Flags().GetBool("force-password-login")
		httpSrv := &http.Server{Addr: listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}

		errCh := make(chan error, 1)
		go func() { errCh <- httpSrv.ListenAndServe() }()
		log.Info().Str("listen", listen).Str("db", driver).Msg("serving")

		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := httpSrv.Shutdown(shutdownCtx)
			srv.Close() // stop background syncs
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
	},
}

func init() {
	serveCmd.Flags().String("listen", envOr("DEXAFIX_LISTEN", ":8080"), "address to listen on")
	addDBFlags(serveCmd)
	serveCmd.Flags().Bool("auto-migrate", envOr("DEXAFIX_AUTO_MIGRATE", "true") != "false", "apply pending database migrations on startup (otherwise fail if any are pending)")
	serveCmd.Flags().Int("audit-retention-days", envInt("DEXAFIX_AUDIT_RETENTION_DAYS", 365), "delete audit log events (and the IP addresses in them) after this many days; 0 keeps them forever")
	serveCmd.Flags().String("trusted-proxies", envOr("DEXAFIX_TRUSTED_PROXIES", ""), "comma-separated addresses or CIDR ranges of reverse proxies whose X-Forwarded-* headers are believed; also \"loopback\" and \"private\"")
	serveCmd.Flags().String("base-url", envOr("DEXAFIX_BASE_URL", ""), "public address of the app, such as https://dexa.example.com; used for sign-in callback URLs and to decide that cookies are Secure")
	serveCmd.Flags().Bool("force-password-login", envOr("DEXAFIX_FORCE_PASSWORD_LOGIN", "false") == "true", "keep username and password sign-in on even if an administrator disabled it (to recover from a lockout)")
	serveCmd.Flags().String("encryption-key-file", envOr("DEXAFIX_ENCRYPTION_KEY_FILE", "dexa-fix.key"), "file holding the key that encrypts secrets in the database; created if missing, unused when "+encryptionKeyEnv+" is set")
	RootCmd.AddCommand(serveCmd)
}
