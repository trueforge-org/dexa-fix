// Package app runs the application as one process: a frontend and a backend that talk to each other
// through the internal API over an in-process transport, exactly as two processes would over the
// network.
//
// The two halves are separate packages, and the frontend cannot import the backend (see
// pkg/frontend/boundary_test.go), so this package is the only place that holds both. The split
// deployment (`serve web` and `serve api`) builds each half on its own, see the cmd package.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	mainbackend "github.com/trueforge-org/dexa-fix/pkg/backend/main"
	"github.com/trueforge-org/dexa-fix/pkg/frontend"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexa-fix/pkg/store"
	"github.com/trueforge-org/dexa-fix/pkg/workers/builtin"
	"github.com/trueforge-org/dexa-fix/pkg/workers/runner"
)

// Server is the application as one process. Names that both halves have are defined here, so they
// act on the combined process.
type Server struct {
	*frontend.Frontend
	*mainbackend.Backend
	stopWorkers func() // stops the workers that run inside this process
}

// New builds the combined Server.
//
// The page handlers call the backend (the internal API) through an in-process transport, and the
// public API is served on the same handler, without the load budget of a separate backend. The
// workers run in this process too, as they would in their own containers: they lease jobs from the
// backend through the same internal API, with a secret of their own.
func New(st store.Store) (*Server, error) {
	return NewWithWorkers(st, builtin.Options{})
}

// NewWithWorkers is New with options for the workers; tests point them at fake services.
func NewWithWorkers(st store.Store, wo builtin.Options) (*Server, error) {
	b, err := mainbackend.NewBackend(st)
	if err != nil {
		return nil, err
	}
	b.DisableInternalBudget() // no flood protection between the halves of one process
	secret, err := randomHex(32)
	if err != nil {
		b.Close()
		return nil, err
	}
	b.SetServiceSecrets(secret)
	workerSecret, err := randomHex(32)
	if err != nil {
		b.Close()
		return nil, err
	}
	b.SetWorkerSecrets(workerSecret)
	client, err := apiclient.New("http://backend.internal", &http.Client{Transport: apiclient.InProcess(b.InternalHandler())}, secret)
	if err != nil {
		b.Close()
		return nil, err
	}
	f, err := frontend.NewFrontend(client)
	if err != nil {
		b.Close()
		return nil, err
	}
	f.DisableLimits()
	f.SetAPIHandler(b.APIHandler())
	if err := f.RefreshConfig(context.Background()); err != nil {
		f.Close()
		b.Close()
		return nil, err
	}
	// A setting changed in the backend (the Admin page, a test) reaches the frontend at once.
	b.OnSettingsChange(func() {
		if err := f.RefreshConfig(context.Background()); err != nil {
			log.Error().Err(err).Msg("could not pass the changed settings to the frontend")
		}
	})
	stop, err := startEmbeddedWorkers(b, workerSecret, wo)
	if err != nil {
		f.Close()
		b.Close()
		return nil, err
	}
	return &Server{Frontend: f, Backend: b, stopWorkers: stop}, nil
}

// startEmbeddedWorkers runs the built-in workers inside this process, talking to the backend over
// the in-process transport. It returns a function that stops them and waits.
func startEmbeddedWorkers(b *mainbackend.Backend, secret string, wo builtin.Options) (stop func(), err error) {
	wc, err := workerapi.New("http://backend.internal", &http.Client{Transport: apiclient.InProcess(b.InternalHandler())}, secret)
	if err != nil {
		return nil, err
	}
	r := runner.New(wc, runner.Options{ID: "embedded", Concurrency: 2, ConfigEvery: 5 * time.Second})
	if err := builtin.Register(r, nil, wo); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.Run(ctx); err != nil {
			log.Error().Err(err).Msg("the embedded workers stopped")
		}
	}()
	return func() { cancel(); <-done }, nil
}

// SetVersion sets the version both halves report.
func (s *Server) SetVersion(v string) {
	s.Frontend.Version, s.Backend.Version = v, v
}

// Close stops the background jobs of both halves and the HTTPS and HTTP/3 servers. Call it after
// the HTTP server has been shut down.
func (s *Server) Close() {
	if s.stopWorkers != nil {
		s.stopWorkers() // first: they call the backend
	}
	s.Backend.Close()
	s.Frontend.Close()
}

// WrapListener wraps the listener of the pages.
func (s *Server) WrapListener(ln net.Listener) net.Listener { return s.Frontend.WrapListener(ln) }

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
