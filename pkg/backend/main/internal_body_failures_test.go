package mainbackend

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/shared/bodyapi"
)

// A body service that answers every call with one failure, so that a test sees what the backend
// does when the readings and scans cannot be had. A 503 is a service that is down; a 403 is one
// that refuses the backend's credentials, which the backend reports as an internal error.
func bodyAnswering(t *testing.T, status int, code string) *bodyapi.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(`{"code":"` + code + `","message":"the body service says no"}`))
	}))
	t.Cleanup(ts.Close)
	_, _, priv := bodyapi.GenerateKey()
	c, err := bodyapi.New(ts.URL, bodyapi.HTTPClient(nil), "a-body-secret-of-this-test-0123456789", bodyapi.NewSigner(priv))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// bodyDown is a client of a body service that is not running.
func bodyDown(t *testing.T) *bodyapi.Client {
	t.Helper()
	ts := httptest.NewServer(http.NotFoundHandler())
	ts.Close()
	_, _, priv := bodyapi.GenerateKey()
	c, err := bodyapi.New(ts.URL, bodyapi.HTTPClient(nil), "a-body-secret-of-this-test-0123456789", bodyapi.NewSigner(priv))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// useBody swaps the body service a running stack calls.
func (e *adminEnv) useBody(c *bodyapi.Client) { e.backend.body = bodyService{c: c} }
