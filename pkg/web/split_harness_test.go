package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// splitTests reports whether the test servers are built as two processes: a frontend without a
// database talking over TCP to a backend. The whole browser-level suite then runs against the split:
//
//	DEXAFIX_TEST_SPLIT=1 go test ./pkg/web
func splitTests() bool { return os.Getenv("DEXAFIX_TEST_SPLIT") != "" }

// newSplitServer starts a backend on st and returns the test server of a frontend that follows it.
// serverOf(ts) is a Server with the backend only: tests reach the store and the live settings through it.
func newSplitServer(t *testing.T, st store.Store, wi *withings.Importer) *httptest.Server {
	t.Helper()
	old := limitFrontend
	limitFrontend = false // tests drive the frontend hard from one address
	t.Cleanup(func() { limitFrontend = old })

	b, err := NewBackend(st, importer.NewRegistry(wi))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	const secret = "split-test-secret-0123456789abcdef0123"
	b.SetServiceSecrets(secret)
	b.internalBudget = nil // the tests are not a flood
	internal := httptest.NewServer(b.InternalHandler())
	t.Cleanup(internal.Close)
	pub := httptest.NewServer(b.PublicHandler())
	t.Cleanup(pub.Close)

	client, err := apiclient.New(internal.URL, nil, secret)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFrontend(client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	u, _ := url.Parse(pub.URL)
	f.SetAPIUpstream(u)
	if err := f.RefreshConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Settings the tests put in force on the backend directly reach the frontend at once, as the
	// Admin page's save does in production.
	b.onSettings = func() { f.RefreshConfig(context.Background()) }
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
