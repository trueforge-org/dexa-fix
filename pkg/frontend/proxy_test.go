package frontend

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/shared/edge"
)

func TestBaseURL(t *testing.T) {
	srv := &Frontend{}
	base, _ := edge.ParseBaseURL("https://dexa.example.com/")
	srv.swapSettings(&frontendSettings{baseURL: base})
	var got string
	h := srv.proxyHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = edge.ExternalURL(r, "/auth/x/callback") }))
	req := httptest.NewRequest("GET", "http://10.0.0.5:8080/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "https://dexa.example.com/auth/x/callback" {
		t.Errorf("callback URL %q", got)
	}
}
