package cmd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
	"github.com/trueforge-org/dexa-fix/pkg/web"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the web application",
	Long: `Run the web application.

Flags can also be set through DEXAFIX_LISTEN, DEXAFIX_DB_DRIVER and DEXAFIX_DSN.

The Withings import is enabled by setting the client id, secret and redirect URL
(DEXAFIX_WITHINGS_CLIENT_ID, DEXAFIX_WITHINGS_CLIENT_SECRET, DEXAFIX_WITHINGS_REDIRECT_URL).`,
	Example: `dexa-fix serve --listen :8080
dexa-fix serve --db-driver postgres --dsn postgres://user:pass@localhost/dexafix`,
	RunE: func(cmd *cobra.Command, args []string) error {
		listen, _ := cmd.Flags().GetString("listen")
		driver, _ := cmd.Flags().GetString("db-driver")
		dsn, _ := cmd.Flags().GetString("dsn")

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		st, err := sqlstore.Open(ctx, driver, dsn)
		if err != nil {
			return err
		}
		defer st.Close()

		wcfg := withings.Config{}
		wcfg.ClientID, _ = cmd.Flags().GetString("withings-client-id")
		wcfg.ClientSecret, _ = cmd.Flags().GetString("withings-client-secret")
		wcfg.RedirectURL, _ = cmd.Flags().GetString("withings-redirect-url")
		wi := withings.New(wcfg, nil)
		if !wi.Configured() {
			log.Info().Msg("withings import disabled: client id, secret and redirect URL are not all set")
		}
		srv, err := web.New(st, importer.NewRegistry(wi))
		if err != nil {
			return err
		}
		httpSrv := &http.Server{Addr: listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

		errCh := make(chan error, 1)
		go func() { errCh <- httpSrv.ListenAndServe() }()
		log.Info().Str("listen", listen).Str("db", driver).Msg("serving")

		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
	},
}

func init() {
	serveCmd.Flags().String("listen", envOr("DEXAFIX_LISTEN", ":8080"), "address to listen on")
	serveCmd.Flags().String("db-driver", envOr("DEXAFIX_DB_DRIVER", sqlstore.DriverSQLite), "database driver: sqlite or postgres")
	serveCmd.Flags().String("dsn", envOr("DEXAFIX_DSN", "file:dexa-fix.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"), "database connection string")
	serveCmd.Flags().String("withings-client-id", envOr("DEXAFIX_WITHINGS_CLIENT_ID", ""), "Withings OAuth client id")
	serveCmd.Flags().String("withings-client-secret", envOr("DEXAFIX_WITHINGS_CLIENT_SECRET", ""), "Withings OAuth client secret")
	serveCmd.Flags().String("withings-redirect-url", envOr("DEXAFIX_WITHINGS_REDIRECT_URL", "http://localhost:8080/integrations/withings/callback"), "OAuth callback URL registered with the Withings application")
	RootCmd.AddCommand(serveCmd)
}
