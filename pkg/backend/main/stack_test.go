package mainbackend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/frontend"
	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// The tests of the backend drive it through the pages too, so they need a whole stack: a Backend
// and a Frontend over the internal API. The Frontend is built here exactly as pkg/app builds it
// (app imports this package, so the tests cannot use it).
//
// DEXAFIX_TEST_SPLIT=1 builds the stack as two processes instead: a frontend without a database
// talking over TCP to the backend. The whole browser-level suite then runs against the split:
//
//	DEXAFIX_TEST_SPLIT=1 go test ./pkg/backend ./pkg/frontend

// Server is the stack as the tests see it: both halves of the combined process. In a split run
// only the Backend is set (the frontend is a separate process, reached over HTTP).
type Server struct {
	*frontend.Frontend
	*Backend
	api *apiclient.Client // how the frontend reaches the backend
}

// New builds the combined stack on st, as pkg/app does. importers may be nil.
func New(st store.Store, importers *importer.Registry) (*Server, error) {
	b, err := NewBackend(st, importers)
	if err != nil {
		return nil, err
	}
	b.DisableInternalBudget() // no flood protection between the halves of one process
	secret := testRandomHex(32)
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
	b.OnSettingsChange(func() {
		if err := f.RefreshConfig(context.Background()); err != nil {
			log.Error().Err(err).Msg("could not pass the changed settings to the frontend")
		}
	})
	return &Server{Frontend: f, Backend: b, api: client}, nil
}

func testRandomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// SetVersion sets the version both halves report.
func (s *Server) SetVersion(v string) {
	s.Frontend.Version, s.Backend.Version = v, v
}

// Close stops both halves.
func (s *Server) Close() {
	s.Backend.Close()
	s.Frontend.Close()
}

// WrapListener wraps the listener of the pages.
func (s *Server) WrapListener(ln net.Listener) net.Listener { return s.Frontend.WrapListener(ln) }

// splitTests reports whether the test servers are built as two processes.
func splitTests() bool { return os.Getenv("DEXAFIX_TEST_SPLIT") != "" }

// newSplitServer starts a backend on st and returns the test server of a frontend that follows it.
// serverOf(ts) is a Server with the backend only: tests reach the store and the live settings through it.
func newSplitServer(t *testing.T, st store.Store, wi *withings.Importer) *httptest.Server {
	t.Helper()
	b, err := NewBackend(st, importer.NewRegistry(wi))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	const secret = "split-test-secret-0123456789abcdef0123"
	b.SetServiceSecrets(secret)
	b.DisableInternalBudget() // the tests are not a flood
	internal := httptest.NewServer(b.InternalHandler())
	t.Cleanup(internal.Close)
	pub := httptest.NewServer(b.PublicHandler())
	t.Cleanup(pub.Close)

	client, err := apiclient.New(internal.URL, nil, secret)
	if err != nil {
		t.Fatal(err)
	}
	f, err := frontend.NewFrontend(client)
	if err != nil {
		t.Fatal(err)
	}
	f.DisableLimits() // tests drive the frontend hard from one address
	t.Cleanup(f.Close)
	u, _ := url.Parse(pub.URL)
	f.SetAPIUpstream(u)
	if err := f.RefreshConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Settings the tests put in force on the backend directly reach the frontend at once, as the
	// Admin page's save does in production.
	b.OnSettingsChange(func() { f.RefreshConfig(context.Background()) })
	ts := httptest.NewServer(f.Handler())
	t.Cleanup(ts.Close)
	testServers.Store(ts, &Server{Backend: b}) // no Frontend: it is a separate process here
	return ts
}

// skipInSplit skips a test of the listeners or of the combined server: in a split they belong to
// the frontend process, which has its own tests.
func skipInSplit(t *testing.T) {
	t.Helper()
	if splitTests() {
		t.Skip("covers the combined server; the split has its own test")
	}
}
