package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/workers/builtin"
	"github.com/trueforge-org/dexafix/pkg/workers/runner"
)

// `serve worker` runs the workers: processes that talk to Withings, Strength Level and the like for
// the api process. A worker has no database and no keys. It leases jobs from the api process over
// its internal listener, with a secret of its own, does the outside call, and reports back; the api
// process decides what is stored. A worker can run in its own container, with network access to the
// outside but none to the database.

var serveWorkerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Run a worker: it talks to Withings, Strength Level and the like for the api process, without a database",
	Long: `Run a worker. It asks the api process (dexafix serve api) for jobs over the api's internal
address, authenticated with the worker secret, does the call to the outside service, and reports the
result. It has no database and no encryption key: the credentials of a user reach it only inside the
job it works on.

Which providers a worker runs is a boot-time choice (--providers); what an administrator can change
at any time (pausing a kind, time limits, retries, pauses between requests) is on the Admin page and
reaches every worker without a restart. Run as many workers as you like: the api process hands each
job to exactly one.`,
	Example: `dexafix serve worker --backend-url http://api:8081 --worker-secret-file /run/secrets/dexafix_worker --providers withings`,
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		backend, _ := cmd.Flags().GetString("backend-url")
		if backend == "" {
			return errors.New("--backend-url (DEXAFIX_BACKEND_URL) is required: the internal address of the api process, such as http://api:8081")
		}
		path, _ := cmd.Flags().GetString("worker-secret-file")
		secrets, err := loadWorkerSecrets(path)
		if err != nil {
			return err
		}
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		if concurrency < 1 || concurrency > 64 {
			return errors.New("--concurrency must be from 1 to 64")
		}
		var names []string
		if v, _ := cmd.Flags().GetString("providers"); v != "" {
			names = strings.Split(v, ",")
		}
		for _, name := range []string{"DEXAFIX_DSN_FILE", "DEXAFIX_ENCRYPTION_KEY", "DEXAFIX_ENCRYPTION_KEY_FILE", "DEXAFIX_SERVICE_SECRET_FILE", "DEXAFIX_VALKEY_URL", "DEXAFIX_VALKEY_URL_FILE"} {
			if os.Getenv(name) != "" {
				log.Warn().Str("variable", name).Msg("ignored: a worker has no database, no keys and no service secret")
			}
		}
		hc, secured, err := backendHTTPClient(cmd, backend)
		if err != nil {
			return err
		}
		client, err := workerapi.New(backend, hc, secrets[0])
		if err != nil {
			return err
		}
		if u, perr := url.Parse(backend); perr == nil && !secured && !privateHost(u.Hostname()) {
			log.Warn().Str("backend", backend).Msg("the backend is reached over plain HTTP at an address that is not private: " +
				"the link carries credentials of users' accounts at the providers; use a private network or mutual TLS (dexafix secrets mtls)")
		}
		id, _ := cmd.Flags().GetString("worker-id")
		r := runner.New(client, runner.Options{ID: id, Version: Version, Concurrency: concurrency})
		if err := builtin.Register(r, names, builtin.Options{}); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		healthErr := make(chan error, 1)
		health, err := startHealth(cmd, func(context.Context) error { return r.Ready(workerReadyAge) }, healthErr)
		if err != nil {
			return err
		}
		defer shutdown(health)
		go func() { // a health listener that dies is logged; the worker itself carries on
			if err := <-healthErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error().Err(err).Msg("the health listener stopped")
			}
		}()
		log.Info().Str("backend", backend).Str("id", r.ID()).Int("concurrency", concurrency).Msg("running the worker")
		if err := r.Run(ctx); err != nil {
			return fmt.Errorf("worker: %w", err)
		}
		return nil
	},
}

// workerReadyAge is how long a worker may go without an answer from the api process and still be
// ready. It is the time the api process counts a silent worker as running (90 s); a worker that waits
// for a job asks again every 25 s, so a longer silence means the link or the api process is down.
const workerReadyAge = 90 * time.Second

func init() {
	serveWorkerCmd.Flags().String("backend-url", envOr("DEXAFIX_BACKEND_URL", ""), "internal address of the api process, such as http://api:8081")
	addWorkerSecretFlag(serveWorkerCmd, "file with the secret the api process and its workers share, one per line (the first is the one sent); create one with `dexafix secrets worker-secret`")
	serveWorkerCmd.Flags().String("providers", envOr("DEXAFIX_WORKER_PROVIDERS", ""), "comma-separated providers this worker runs ("+strings.Join(builtin.Names(), ", ")+"); empty runs all")
	serveWorkerCmd.Flags().Int("concurrency", envInt("DEXAFIX_WORKER_CONCURRENCY", 2), "jobs this process runs at once (1-64)")
	serveWorkerCmd.Flags().String("worker-id", envOr("DEXAFIX_WORKER_ID", ""), "name of this worker in the Admin page (default: random)")
	addBackendTLSFlags(serveWorkerCmd)
	addHealthFlag(serveWorkerCmd)
	serveCmd.AddCommand(serveWorkerCmd)
}
