package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/trueforge-org/dexa-fix/pkg/app"
	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

// Version is the application version, set by main.
var Version = "dev"

// encryptionKeyEnv holds the encryption key directly. There is deliberately no flag for it, since
// command lines are visible to other processes.
const encryptionKeyEnv = "DEXAFIX_ENCRYPTION_KEY"

// encryptionKeyPreviousEnv holds earlier keys, comma separated, that may still open values written
// before a rotation. Like the current key it has no flag.
const encryptionKeyPreviousEnv = "DEXAFIX_ENCRYPTION_KEY_PREVIOUS"

// resolveEncryptionKey returns the key from the environment value, or else from the key file,
// creating the file with a new random key if it does not exist. hasData, when not nil, tells
// whether the database already holds encrypted values: then a missing key file is an error, since a
// new key could not read them and would only bury the real problem (a key file that did not mount).
func resolveEncryptionKey(envValue, path string, hasData func() (bool, error)) (key []byte, created bool, err error) {
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
	if hasData != nil {
		has, err := hasData()
		if err != nil {
			return nil, false, fmt.Errorf("check the database for encrypted values: %w", err)
		}
		if has {
			return nil, false, fmt.Errorf("encryption key file %s not found, but the database already holds encrypted values: "+
				"restore the key file (is the volume mounted? is the working directory right?) or set %s; "+
				"no new key is created, because it could not read them", path, encryptionKeyEnv)
		}
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

// resolvePreviousKeys returns the keys named in the environment value (comma separated) and in the
// file (one per line; blank lines and lines starting with # are ignored).
func resolvePreviousKeys(envValue, path string) ([][]byte, error) {
	var keys [][]byte
	for _, part := range strings.Split(envValue, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		key, err := sqlstore.ParseKey(part)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", encryptionKeyPreviousEnv, err)
		}
		keys = append(keys, key)
	}
	if path == "" {
		return keys, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("previous encryption keys: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, err := sqlstore.ParseKey(line)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// keyFileBesideDatabase reports whether the database is a SQLite file in the same directory as the
// key file. A leak of that directory (a backup, a volume snapshot, an image layer) then exposes
// both the ciphertext and the key.
func keyFileBesideDatabase(driver, dsn, keyFile string) bool {
	if driver != sqlstore.DriverSQLite || keyFile == "" {
		return false
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(dsn, "file://"), "file:"), "?")
	if name == "" || name == ":memory:" {
		return false
	}
	return sameDir(name, keyFile)
}

func sameDir(a, b string) bool {
	dir := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		d := filepath.Dir(abs)
		if real, err := filepath.EvalSymlinks(d); err == nil {
			d = real
		}
		return d
	}
	return dir(a) == dir(b)
}

// addKeyFlags registers the encryption key flags shared by the commands that open the database
// with a cipher. The keys themselves are never accepted as flag values.
func addKeyFlags(c *cobra.Command) {
	c.Flags().String("encryption-key-file", envOr("DEXAFIX_ENCRYPTION_KEY_FILE", "dexa-fix.key"), "file holding the key that encrypts secrets in the database; created if missing for a new database, unused when "+encryptionKeyEnv+" is set")
	c.Flags().String("encryption-key-previous-file", envOr("DEXAFIX_ENCRYPTION_KEY_PREVIOUS_FILE", ""), "file with earlier encryption keys, one per line, that may still open values written before a rotation (see also "+encryptionKeyPreviousEnv+")")
}

// loadCipher resolves the current and previous keys. It refuses to create a key file for a database
// that already holds encrypted values.
func loadCipher(ctx context.Context, cmd *cobra.Command, driver, dsn string) (*sqlstore.Cipher, error) {
	keyFile, _ := cmd.Flags().GetString("encryption-key-file")
	prevFile, _ := cmd.Flags().GetString("encryption-key-previous-file")
	hasData := func() (bool, error) { return sqlstore.HasEncryptedValues(ctx, driver, dsn) }
	key, created, err := resolveEncryptionKey(os.Getenv(encryptionKeyEnv), keyFile, hasData)
	if err != nil {
		return nil, err
	}
	if created {
		log.Warn().Str("file", keyFile).Msg("created a new encryption key: back it up, encrypted values cannot be read without it")
	}
	if os.Getenv(encryptionKeyEnv) == "" && keyFileBesideDatabase(driver, dsn, keyFile) {
		log.Warn().Str("file", keyFile).Msg("the encryption key file is in the same directory as the database file: " +
			"a leak of that directory (backup, snapshot, image) exposes both the encrypted values and the key; " +
			"prefer " + encryptionKeyEnv + " from a secret store, or a key file on a separate secret mount")
	}
	previous, err := resolvePreviousKeys(os.Getenv(encryptionKeyPreviousEnv), prevFile)
	if err != nil {
		return nil, err
	}
	return sqlstore.NewCipher(key, previous...)
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

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		c, err := loadCipher(ctx, cmd, driver, dsn)
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
		srv, err := app.New(st, importer.NewRegistry(wi))
		if err != nil {
			return err
		}
		srv.SetVersion(Version)
		// These used to be flags or variables. They are Admin settings now (stored in the database,
		// applied without a restart), so the old ones are ignored, with a warning.
		for _, name := range []string{"DEXAFIX_TRUSTED_PROXIES", "DEXAFIX_BASE_URL", "DEXAFIX_AUDIT_RETENTION_DAYS"} {
			if os.Getenv(name) != "" {
				log.Warn().Str("variable", name).Msg("ignored: this is a setting on the Admin page now")
			}
		}
		srv.StartMaintenance()
		srv.ForcePasswordLogin, _ = cmd.Flags().GetBool("force-password-login")
		handler := srv.Handler()
		httpSrv := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
		// HTTP/1.1 and HTTP/2, including cleartext HTTP/2 (h2c) for a proxy that speaks it to this
		// server. With a certificate (Admin page) the same port also takes TLS, with HTTP/2 through
		// ALPN, and HTTP/3 runs on the UDP port with the same number.
		var protocols http.Protocols
		protocols.SetHTTP1(true)
		protocols.SetHTTP2(true)
		protocols.SetUnencryptedHTTP2(true)
		httpSrv.Protocols = &protocols

		ln, err := net.Listen("tcp", listen)
		if err != nil {
			return err
		}
		errCh := make(chan error, 1)
		go func() { errCh <- httpSrv.Serve(srv.WrapListener(ln)) }()
		srv.StartListeners(handler)
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
	serveCmd.Flags().Bool("force-password-login", envOr("DEXAFIX_FORCE_PASSWORD_LOGIN", "false") == "true", "keep username and password sign-in on even if an administrator disabled it (to recover from a lockout)")
	addKeyFlags(serveCmd)
	RootCmd.AddCommand(serveCmd)
}
