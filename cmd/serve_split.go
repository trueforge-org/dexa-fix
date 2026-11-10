package cmd

import (
	"context"
	"crypto/tls"
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

	bodybackend "github.com/trueforge-org/dexafix/pkg/backend/body"
	mainbackend "github.com/trueforge-org/dexafix/pkg/backend/main"
	"github.com/trueforge-org/dexafix/pkg/frontend"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
	"github.com/trueforge-org/dexafix/pkg/store/valkeystate"
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
(dexafix serve api) over its internal address, authenticated with the shared service secret.

The administrator settings it acts on (public address, trusted proxies, PROXY protocol, TLS) come
from the api process and apply without a restart.`,
	Example: `dexafix serve web --backend-url http://api:8081 --service-secret-file /run/secrets/dexafix_service`,
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
		for _, name := range []string{"DEXAFIX_DSN_FILE", "DEXAFIX_ENCRYPTION_KEY", "DEXAFIX_VALKEY_URL", "DEXAFIX_VALKEY_URL_FILE"} {
			if os.Getenv(name) != "" {
				log.Warn().Str("variable", name).Msg("ignored: the web frontend has no database; this belongs to the api process")
			}
		}
		hc, secured, err := backendHTTPClient(cmd, backend)
		if err != nil {
			return err
		}
		client, err := apiclient.New(backend, hc, secrets[0])
		if err != nil {
			return err
		}
		if u, perr := url.Parse(backend); perr == nil && !secured && !privateHost(u.Hostname()) {
			log.Warn().Str("backend", backend).Msg("the backend is reached over plain HTTP at an address that is not private: " +
				"the link carries sessions, passwords and the TLS private key; use a private network or mutual TLS (dexafix secrets mtls)")
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
  --internal-listen  the internal API, for the web frontend (dexafix serve web). Keep it on a
                     private network: it carries sessions and passwords, and is protected by the
                     shared service secret only.

Either address may be empty to turn that listener off, so that one api process can serve the public
API and another the internal one on the same database.`,
	Example: `dexafix serve api --internal-listen :8081 --service-secret-file /run/secrets/dexafix_service`,
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		listen, _ := cmd.Flags().GetString("listen")
		internal, _ := cmd.Flags().GetString("internal-listen")
		maintenance, _ := cmd.Flags().GetBool("maintenance")
		if listen == "" && internal == "" {
			return errors.New("nothing to serve: --listen and --internal-listen are both empty")
		}
		internalTLS, err := internalServerTLS(cmd)
		if err != nil {
			return err
		}
		if internalTLS != nil && internal == "" {
			return errors.New("the internal TLS files are set, but --internal-listen is empty")
		}
		var secrets []string
		if internal != "" {
			path, _ := cmd.Flags().GetString("service-secret-file")
			var err error
			if secrets, err = loadServiceSecrets(path); err != nil {
				return err
			}
		}
		var workerSecrets []string
		if path, _ := cmd.Flags().GetString("worker-secret-file"); path != "" {
			var err error
			if workerSecrets, err = loadWorkerSecrets(path); err != nil {
				return err
			}
		}
		// The body service is called over the network when --body-url is set; otherwise it runs
		// inside this process, on the same database.
		bodyClt, bodySecrets, remoteBody, err := bodyClient(cmd)
		if err != nil {
			return err
		}
		if without, _ := cmd.Flags().GetBool("without-body"); without && !remoteBody {
			return errors.New("--without-body needs --body-url: without a body service of its own this process runs one, on this database")
		}
		if err := distinctSecrets(map[string][]string{"service": secrets, "worker": workerSecrets, "body": bodySecrets}); err != nil {
			return err
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
		if remoteBody {
			opts = append(opts, sqlstore.WithoutBody()) // the scans and readings are in the body service's database
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
		backendOpts := []mainbackend.Option{mainbackend.WithBody(bodyClt)}
		if !remoteBody {
			// No --body-url: this process runs the body service too, on the same database.
			inProcess, bodySvc, err := bodybackend.NewInProcess(st)
			if err != nil {
				return err
			}
			bodySvc.SetVersion(Version)
			backendOpts = []mainbackend.Option{mainbackend.WithBody(inProcess)}
		}
		valkeyURL, err := secretFlag(cmd, "valkey-url", "valkey-url-file", dsnHasPassword)
		if err != nil {
			return err
		}
		if valkeyURL != "" {
			// The failed-attempt counters, the single-use nonces and the sessions live in Valkey, so that a flood of
			// wrong passwords costs the database nothing.
			vs, err := valkeystate.Open(ctx, valkeyURL)
			if err != nil {
				return err
			}
			defer vs.Close()
			backendOpts = append(backendOpts, mainbackend.WithState(vs))
			log.Info().Msg("the sign-in counters, single-use values and sessions are kept in Valkey")
		}
		if !maintenance {
			// Only the process that runs the maintenance loop creates and renews the self-signed
			// certificate; the others load what it stored.
			backendOpts = append(backendOpts, mainbackend.WithoutCertificateGeneration())
		}
		srv, err := mainbackend.NewBackend(st, backendOpts...)
		if err != nil {
			return err
		}
		srv.Version = Version
		srv.ForcePasswordLogin, _ = cmd.Flags().GetBool("force-password-login")
		srv.StartSettingsPoll()
		if maintenance {
			srv.StartMaintenance()
		}

		errCh := make(chan error, 3)
		var servers []*http.Server
		health, err := startHealth(cmd, srv.Ready, errCh)
		if err != nil {
			return err
		}
		servers = append(servers, health)
		if listen != "" {
			pub := newHTTPServer(listen, srv.PublicHandler())
			if err := listenAndServe(pub, srv.WrapListener, errCh); err != nil {
				return err
			}
			servers = append(servers, pub)
		}
		if len(workerSecrets) > 0 {
			srv.SetWorkerSecrets(workerSecrets...)
		}
		if internal != "" {
			srv.SetServiceSecrets(secrets...)
			in := newHTTPServer(internal, srv.InternalHandler())
			var wrap func(net.Listener) net.Listener
			if internalTLS != nil {
				wrap = func(ln net.Listener) net.Listener { return tls.NewListener(ln, internalTLS) }
			}
			if err := listenAndServe(in, wrap, errCh); err != nil {
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
	addBackendTLSFlags(serveWebCmd)

	serveAPICmd.Flags().String("listen", envOr("DEXAFIX_LISTEN", ":8080"), "address of the public API (empty: off)")
	addHealthFlag(serveAPICmd)
	serveAPICmd.Flags().String("valkey-url", envOr("DEXAFIX_VALKEY_URL", ""), "keep the failed sign-in counters, single-use values and sessions in Valkey (redis://host:6379/0, rediss:// for TLS) instead of the database; a password goes in --valkey-url-file, not here")
	serveAPICmd.Flags().String("valkey-url-file", envOr("DEXAFIX_VALKEY_URL_FILE", ""), "file with the Valkey URL, which may hold the password: redis://:password@host:6379/0")
	serveAPICmd.Flags().String("internal-listen", envOr("DEXAFIX_INTERNAL_LISTEN", ":8081"), "address of the internal API for the web frontend (empty: off); keep it on a private network")
	serveAPICmd.Flags().Bool("maintenance", envOr("DEXAFIX_MAINTENANCE", "true") != "false", "run the maintenance loop (audit log purge, self-signed certificate renewal); turn it off on all but one api process")
	addServiceSecretFlag(serveAPICmd)
	addWorkerSecretFlag(serveAPICmd, "file with the secret the api process and its workers share, one per line (the first is the one workers send); create one with `dexafix secrets worker-secret`; it must differ from the service secret")
	addInternalTLSFlags(serveAPICmd)
	addBodyClientFlags(serveAPICmd)
	addDBFlags(serveAPICmd)
	serveAPICmd.Flags().Bool("auto-migrate", envOr("DEXAFIX_AUTO_MIGRATE", "true") != "false", "apply pending database migrations on startup (otherwise fail if any are pending)")
	serveAPICmd.Flags().Bool("force-password-login", envOr("DEXAFIX_FORCE_PASSWORD_LOGIN", "false") == "true", "keep username and password sign-in on even if an administrator disabled it (to recover from a lockout)")
	addKeyFlags(serveAPICmd)

	serveCmd.Args = cobra.NoArgs
	serveCmd.AddCommand(serveWebCmd, serveAPICmd)
}
