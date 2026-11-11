package mainbackend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/frontend"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/store"
	"github.com/trueforge-org/dexafix/pkg/workers/builtin"
	"github.com/trueforge-org/dexafix/pkg/workers/runner"
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
	api         *apiclient.Client // how the frontend reaches the backend
	stopWorkers func()            // stops the workers that run in this process
}

// New builds the combined stack on st, as pkg/app does, workers included.
func New(st store.Store, opts ...Option) (*Server, error) {
	return newStack(st, builtin.Options{}, opts...)
}

func init() {
	// The tests wait for jobs: ask for work often instead of once a second.
	leasePollEvery.Set(25 * time.Millisecond)
}

// startTestWorkers runs the built-in workers against the worker endpoints behind wc and returns
// once they have introduced themselves, so a test that queues an interactive job at once finds them.
func startTestWorkers(b *Backend, wc *workerapi.Client, wo builtin.Options) (stop func(), err error) {
	r := runner.New(wc, runner.Options{ID: "test-worker", Concurrency: 2, Poll: time.Second, ConfigEvery: 200 * time.Millisecond})
	if err := builtin.Register(r, nil, wo); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	stop = func() { cancel(); <-done }
	deadline := time.Now().Add(10 * time.Second)
	for {
		if ok, _ := b.workerOnline(ctx, kindWithingsImport); ok {
			return stop, nil
		}
		if time.Now().After(deadline) {
			stop()
			return nil, errors.New("the test workers did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newStack(st store.Store, wo builtin.Options, opts ...Option) (*Server, error) {
	b, err := NewBackend(st, append([]Option{testBody(st)}, opts...)...)
	if err != nil {
		return nil, err
	}
	b.DisableInternalBudget() // no flood protection between the halves of one process
	secret := testRandomHex(32)
	b.SetServiceSecrets(secret)
	workerSecret := testRandomHex(32)
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
	b.OnSettingsChange(func() {
		if err := f.RefreshConfig(context.Background()); err != nil {
			log.Error().Err(err).Msg("could not pass the changed settings to the frontend")
		}
	})
	wc, err := workerapi.New("http://backend.internal", &http.Client{Transport: apiclient.InProcess(b.InternalHandler())}, workerSecret)
	if err != nil {
		f.Close()
		b.Close()
		return nil, err
	}
	stop, err := startTestWorkers(b, wc, wo)
	if err != nil {
		f.Close()
		b.Close()
		return nil, err
	}
	return &Server{Frontend: f, Backend: b, api: client, stopWorkers: stop}, nil
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
	if s.stopWorkers != nil {
		s.stopWorkers()
	}
	s.Backend.Close()
	s.Frontend.Close()
}

// WrapListener wraps the listener of the pages.
func (s *Server) WrapListener(ln net.Listener) net.Listener { return s.Frontend.WrapListener(ln) }

// splitTests reports whether the test servers are built as two processes.
func splitTests() bool { return os.Getenv("DEXAFIX_TEST_SPLIT") != "" }

// newSplitServer starts a backend on st and returns the test server of a frontend that follows it.
// serverOf(ts) is a Server with the backend only: tests reach the store and the live settings through it.
func newSplitServer(t *testing.T, st store.Store, wo builtin.Options) *httptest.Server {
	t.Helper()
	b, err := NewBackend(st, testBody(st))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	const secret = "split-test-secret-0123456789abcdef0123"
	const workerSecret = "split-test-worker-secret-0123456789abcdef"
	b.SetServiceSecrets(secret)
	b.SetWorkerSecrets(workerSecret)
	b.DisableInternalBudget() // the tests are not a flood
	internal := httptest.NewServer(b.InternalHandler())
	t.Cleanup(internal.Close)
	// The workers talk to the backend over TCP, as their own processes would.
	wc, err := workerapi.New(internal.URL, nil, workerSecret)
	if err != nil {
		t.Fatal(err)
	}
	stopWorkers, err := startTestWorkers(b, wc, wo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopWorkers) // before the server closes: a worker waiting for a job holds a request open
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
	testServers.Store(ts, &Server{Backend: b, stopWorkers: stopWorkers}) // no Frontend: it is a separate process here
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
