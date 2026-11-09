package apiclient

import (
	"fmt"
	"io"
	"net/http"
)

// InProcess returns a transport that runs requests against h in the same process, on a goroutine of
// its own, so the combined `serve` mode goes through exactly the same middleware as two processes
// do. The body of the answer is streamed, not buffered.
func InProcess(h http.Handler) http.RoundTripper { return inProcess{h} }

type inProcess struct{ h http.Handler }

func (t inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	w := &pipeWriter{pw: pw, header: http.Header{}, ready: make(chan struct{})}

	// The handler sees its own request: a shallow clone with the same context, so cancelling the
	// caller cancels the handler.
	in := req.Clone(req.Context())
	in.RequestURI = req.URL.RequestURI()
	in.RemoteAddr = "127.0.0.1:0"
	if in.Body == nil {
		in.Body = http.NoBody
	}

	go func() {
		defer func() {
			if v := recover(); v != nil {
				if !w.wrote {
					w.header = http.Header{}
					w.WriteHeader(http.StatusInternalServerError)
				}
				pw.CloseWithError(fmt.Errorf("handler panic: %v", v))
				return
			}
			w.WriteHeader(http.StatusOK) // no-op once a header was written
			pw.Close()
		}()
		t.h.ServeHTTP(w, in)
	}()

	select {
	case <-w.ready:
	case <-req.Context().Done():
		pw.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
	resp := &http.Response{
		Status:        fmt.Sprintf("%d %s", w.status, http.StatusText(w.status)),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.snapshot,
		Body:          pr,
		ContentLength: -1,
		Request:       req,
	}
	return resp, nil
}

// pipeWriter is a ResponseWriter whose body is read through a pipe by the caller.
type pipeWriter struct {
	pw       *io.PipeWriter
	header   http.Header
	snapshot http.Header
	status   int
	wrote    bool
	ready    chan struct{}
}

func (w *pipeWriter) Header() http.Header { return w.header }

func (w *pipeWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	w.snapshot = w.header.Clone()
	close(w.ready)
}

func (w *pipeWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(b)
}

// Flush lets handlers that stream (exports) push data through.
func (w *pipeWriter) Flush() { w.WriteHeader(http.StatusOK) }
