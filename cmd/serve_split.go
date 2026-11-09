package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	mainbackend "github.com/trueforge-org/dexa-fix/pkg/backend/main"
	"github.com/trueforge-org/dexa-fix/pkg/frontend"
	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

// `serve web` and `serve api` run the two halves of the application as separate processes, so that
// a flood on one cannot take down the other:
//
//   - web renders the pages, owns the public HTTP, HTTPS and HTTP/3 listeners and has no database;
//   - api owns the database and every security decision. It serves the public API (/api/v1) and,
//     on another port, the internal API that web calls.
//
// Plain `serve` runs both in one process.

func newHTTPServer(addr string, h http.Handler) *http.Server {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	// HTTP/1.1 and HTTP/2, including cleartext HTTP/2 (h2c) for a proxy that speaks it.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Protocols = &protocols
	return srv
}

// listenAndServe serves h on addr in the background; wrap may change the listener (PROXY protocol).
func listenAndServe(srv *http.Server, wrap func(net.Listener) net.Listener, errCh chan<- error) error {
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	if wrap != nil {
		ln = wrap(ln)
	}
	go func() { errCh <- srv.Serve(ln) }()
	return nil
}

func shutdown(servers ...*http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var first error
	for _, s := range servers {
		if s == nil {
			continue
		}
		if err := s.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) && first == nil {
			first = err
		}
	}
	return first
}

// ---- serve web ----

const backendWait = 60 * time.Second

var serveWebCmd = &cobra.Command{
	Use:   "web",
	Short: "Run the web frontend: the pages and the public listeners, without a database",
	Long: `Run the web frontend. It renders the pages, serves the static files and owns the public HTTP,
HTTPS and HTTP/3 listeners. It has no database: everything else is asked of the api process
(dexa-fix serve api) over its internal address, authenticated with the shared service secret.

The administrator settings it acts on (public address, trusted proxies, PROXY protocol, TLS) come
from the api process and apply without a restart.`,
	Example: `dexa-fix serve web --backend-url http://api:8081 --service-secret-file /run/secrets/dexafix_service`,
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		listen, _ := cmd.Flags().GetString("listen")
		backend, _ := cmd.Flags().GetString("backend-url")
		upstream, _ := cmd.Flags().GetString("api-upstream")
		if backend == "" {
			return errors.New("--backend-url (DEXAFIX_BACKEND_URL) is required: the internal address of the api process, such as http://api:8081")
		}
		path, _ := cmd.Flags().GetString("service-secret-file")
		secrets, err := loadServiceSecrets(path)
		if err != nil {
			return err
		}
		for _, name := range []string{"DEXAFIX_DSN_FILE", "DEXAFIX_ENCRYPTION_KEY"} {
			if os.Getenv(name) != "" {
				log.Warn().Str("variable", name).Msg("ignored: the web frontend has no database; this belongs to the api process")
			}
		}
		client, err := apiclient.New(backend, nil, secrets[0])
		if err != nil {
			return err
		}
		if u, perr := url.Parse(backend); perr == nil && !privateHost(u.Hostname()) && u.Scheme == "http" {
			log.Warn().Str("backend", backend).Msg("the backend is reached over plain HTTP at an address that is not private: " +
				"the link carries sessions, passwords and the TLS private key; use a private network or protect the link")
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		srv, err := frontend.NewFrontend(client)
		if err != nil {
			return err
		}
		srv.Version = Version
		defer srv.Close()
		if upstream != "" {
			u, err := url.Parse(upstream)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("--api-upstream %q: use the form http://host:port", upstream)
			}
			srv.SetAPIUpstream(u)
		}
		if err := srv.WaitForBackend(ctx, backendWait); err != nil {
			return err
		}
		srv.StartConfigPoll()

		handler := srv.Handler()
		httpSrv := newHTTPServer(listen, handler)
		errCh := make(chan error, 1)
		if err := listenAndServe(httpSrv, srv.WrapListener, errCh); err != nil {
			return err
		}
		srv.StartListeners(handler)
		log.Info().Str("listen", listen).Str("backend", backend).Msg("serving the web frontend")

		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			return shutdown(httpSrv)
		}
	},
}

// privateHost reports whether host is a name or address that stays on a private network.
func privateHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true // a service name such as "api" resolves inside the private network
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// ---- serve api ----

var serveAPICmd = &cobra.Command{
	Use:   "api",
	Short: "Run the backend: the database, the public API and the internal API for the frontend",
	Long: `Run the backend. It owns the database, the encryption keys and every security decision.

  --listen           the public API (/api/v1), for scripts and apps; put it behind the reverse proxy.
  --internal-listen  the internal API, for the web frontend (dexa-fix serve web). Keep it on a
                     private network: it carries sessions and passwords, and is protected by the
                     shared service secret only.

Either address may be empty to turn that listener off, so that one api process can serve the public
API and another the internal one on the same database.`,
	Example: `dexa-fix serve api --internal-listen :8081 --service-secret-file /run/secrets/dexafix_service`,
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		listen, _ := cmd.Flags().GetString("listen")
		internal, _ := cmd.Flags().GetString("internal-listen")
		maintenance, _ := cmd.Flags().GetBool("maintenance")
		if listen == "" && internal == "" {
			return errors.New("nothing to serve: --listen and --internal-listen are both empty")
		}
		var secrets []string
		if internal != "" {
			path, _ := cmd.Flags().GetString("service-secret-file")
			var err error
			if secrets, err = loadServiceSecrets(path); err != nil {
				return err
			}
		}
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

		for _, name := range []string{"DEXAFIX_WITHINGS_CLIENT_ID", "DEXAFIX_WITHINGS_CLIENT_SECRET", "DEXAFIX_WITHINGS_CLIENT_SECRET_FILE", "DEXAFIX_WITHINGS_REDIRECT_URL",
			"DEXAFIX_TRUSTED_PROXIES", "DEXAFIX_BASE_URL", "DEXAFIX_AUDIT_RETENTION_DAYS"} {
			if os.Getenv(name) != "" {
				log.Warn().Str("variable", name).Msg("ignored: this is a setting on the Admin page now")
			}
		}
		srv, err := mainbackend.NewBackend(st, importer.NewRegistry(withings.New(withings.Config{}, nil)))
		if err != nil {
			return err
		}
		srv.Version = Version
		srv.ForcePasswordLogin, _ = cmd.Flags().GetBool("force-password-login")
		srv.StartSettingsPoll()
		if maintenance {
			srv.StartMaintenance()
		}

		errCh := make(chan error, 2)
		var servers []*http.Server
		if listen != "" {
			pub := newHTTPServer(listen, srv.PublicHandler())
			if err := listenAndServe(pub, srv.WrapListener, errCh); err != nil {
				return err
			}
			servers = append(servers, pub)
		}
		if internal != "" {
			srv.SetServiceSecrets(secrets...)
			in := newHTTPServer(internal, srv.InternalHandler())
			if err := listenAndServe(in, nil, errCh); err != nil {
				return err
			}
			servers = append(servers, in)
		}
		log.Info().Str("public", listen).Str("internal", internal).Str("db", driver).Msg("serving the backend")

		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			err := shutdown(servers...)
			srv.Close()
			return err
		}
	},
}

func init() {
	serveWebCmd.Flags().String("listen", envOr("DEXAFIX_LISTEN", ":8080"), "address to listen on")
	serveWebCmd.Flags().String("backend-url", envOr("DEXAFIX_BACKEND_URL", ""), "internal address of the api process, such as http://api:8081")
	serveWebCmd.Flags().String("api-upstream", envOr("DEXAFIX_API_UPSTREAM", ""), "pass /api/ on to this address (the api process's public listener), for setups without a reverse proxy that routes by path")
	addServiceSecretFlag(serveWebCmd)

	serveAPICmd.Flags().String("listen", envOr("DEXAFIX_LISTEN", ":8080"), "address of the public API (empty: off)")
	serveAPICmd.Flags().String("internal-listen", envOr("DEXAFIX_INTERNAL_LISTEN", ":8081"), "address of the internal API for the web frontend (empty: off); keep it on a private network")
	serveAPICmd.Flags().Bool("maintenance", envOr("DEXAFIX_MAINTENANCE", "true") != "false", "run the maintenance loop (audit log purge, self-signed certificate renewal); turn it off on all but one api process")
	addServiceSecretFlag(serveAPICmd)
	addDBFlags(serveAPICmd)
	serveAPICmd.Flags().Bool("auto-migrate", envOr("DEXAFIX_AUTO_MIGRATE", "true") != "false", "apply pending database migrations on startup (otherwise fail if any are pending)")
	serveAPICmd.Flags().Bool("force-password-login", envOr("DEXAFIX_FORCE_PASSWORD_LOGIN", "false") == "true", "keep username and password sign-in on even if an administrator disabled it (to recover from a lockout)")
	addKeyFlags(serveAPICmd)

	serveCmd.Args = cobra.NoArgs
	serveCmd.AddCommand(serveWebCmd, serveAPICmd)
}
