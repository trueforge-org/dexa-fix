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

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/backend"
	"github.com/trueforge-org/dexa-fix/pkg/frontend"
	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// Server is the application as one process. Names that both halves have are defined here, so they
// act on the combined process.
type Server struct {
	*frontend.Frontend
	*backend.Backend
}

// New builds the combined Server. importers may be nil.
//
// The page handlers call the backend (the internal API) through an in-process transport, and the
// public API is served on the same handler, without the load budget of a separate backend.
func New(st store.Store, importers *importer.Registry) (*Server, error) {
	b, err := backend.NewBackend(st, importers)
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
	return &Server{Frontend: f, Backend: b}, nil
}

// SetVersion sets the version both halves report.
func (s *Server) SetVersion(v string) {
	s.Frontend.Version, s.Backend.Version = v, v
}

// Close stops the background jobs of both halves and the HTTPS and HTTP/3 servers. Call it after
// the HTTP server has been shut down.
func (s *Server) Close() {
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
