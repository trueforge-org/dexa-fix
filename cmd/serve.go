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

Flags can also be set through DEXAFIX_LISTEN, DEXAFIX_DB_DRIVER and DEXAFIX_DSN.`,
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

		srv, err := web.New(st)
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
	RootCmd.AddCommand(serveCmd)
}
