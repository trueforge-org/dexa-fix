package inproc

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAFlushedAnswerReachesTheCallerBeforeTheHandlerEnds(t *testing.T) {
	release := make(chan struct{})
	tr := Transport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Stream", "yes")
		w.(http.Flusher).Flush()
		<-release // the caller must see the headers while the handler is still running
		w.Write([]byte("done"))
	}))
	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "http://backend/x", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Stream") != "yes" {
		t.Fatalf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
	close(release)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "done" {
		t.Fatalf("body %q", b)
	}
}
