package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

// The health listener: a small extra listener that answers /healthz and /readyz and nothing else,
// for a container health check. The images are `scratch` (no shell, no curl), so the check runs
// `dexafix healthcheck <url>` in the same container. The listener is separate because the real
// listeners need a secret or a client certificate, and a health check should need neither.
//
//	/healthz  the process is up and answering
//	/readyz   it can do its work (the api and the body service: the database answers; a worker: it
//	          has heard from the api process lately)
//
// It tells nothing about the deployment beyond ok or not, so it is safe to leave open on a private
// network, and it listens on nothing unless --health-listen says so.

const healthReadyTimeout = 3 * time.Second

func addHealthFlag(c *cobra.Command) {
	c.Flags().String("health-listen", envOr("DEXAFIX_HEALTH_LISTEN", ""),
		"address of a listener that answers /healthz and /readyz only, for a container health check, such as 127.0.0.1:8083 (empty: off)")
}

// healthHandler answers /healthz and /readyz. ready may be nil: then readiness is liveness.
func healthHandler(ready func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), healthReadyTimeout)
			defer cancel()
			if err := ready(ctx); err != nil {
				// The reason is for the log, not for whoever asks: it may name a host.
				log.Debug().Err(err).Msg("not ready")
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		io.WriteString(w, "ok")
	})
	return mux
}

// startHealth serves the health listener in the background if --health-listen is set. It returns
// the server to shut down, or nil when there is none.
func startHealth(cmd *cobra.Command, ready func(context.Context) error, errCh chan<- error) (*http.Server, error) {
	addr, _ := cmd.Flags().GetString("health-listen")
	if addr == "" {
		return nil, nil
	}
	srv := newHTTPServer(addr, healthHandler(ready))
	if err := listenAndServe(srv, nil, errCh); err != nil {
		return nil, fmt.Errorf("health listener: %w", err)
	}
	log.Info().Str("listen", addr).Msg("serving the health check")
	return srv, nil
}

var healthcheckCmd = &cobra.Command{
	Use:   "healthcheck URL",
	Short: "Ask a health listener whether a process is healthy (exit code 0 or 1)",
	Long: `Makes one GET request to URL and exits 0 if the answer is 2xx, and 1 otherwise. It is for the
container health check, because the image has no shell and no curl:

  healthcheck: {test: ["CMD", "/dexafix", "healthcheck", "http://127.0.0.1:8083/readyz"]}

Start the process with --health-listen 127.0.0.1:8083. The web process needs no extra listener:
check http://127.0.0.1:8080/readyz.`,
	Example: `dexafix healthcheck http://127.0.0.1:8083/readyz`,
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		timeout, _ := cmd.Flags().GetDuration("timeout")
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, args[0], nil)
		if err != nil {
			return fmt.Errorf("healthcheck: %w", err)
		}
		hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("unhealthy: %w", err)
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return errors.New("unhealthy: " + resp.Status)
		}
		return nil
	},
}

func init() {
	healthcheckCmd.Flags().Duration("timeout", 3*time.Second, "give up after this long")
	RootCmd.AddCommand(healthcheckCmd)
}
