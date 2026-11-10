package apiclient

import (
	"net/http"

	"github.com/trueforge-org/dexafix/pkg/shared/inproc"
)

// InProcess returns a transport that runs requests against h in the same process, on a goroutine of
// its own, so the combined `serve` mode goes through exactly the same middleware as two processes
// do. The body of the answer is streamed, not buffered.
func InProcess(h http.Handler) http.RoundTripper { return inproc.Transport(h) }
