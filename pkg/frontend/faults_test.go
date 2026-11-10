package frontend

import (
	"bytes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// A faultStack is a real backend with a frontend in front of it, and between the two a proxy that
// can make chosen calls fail: answer 500, answer garbage, cut the connection or answer a crafted
// status. It is how the tests see what a visitor sees when the backend misbehaves.
type faultStack struct {
	t   *testing.T
	f   *Frontend
	web *httptest.Server
	c   *http.Client // signed in as the first user, an administrator; does not follow redirects

	mu     sync.Mutex
	faults map[string]faultFn // "METHOD /path" of the internal API, without its prefix
	except map[string]bool    // while failAll is set, these calls still work
	all    faultFn
}

type faultFn func(w http.ResponseWriter, r *http.Request)

// Ways for a backend call to fail.
var (
	serverError faultFn = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }
	garbage     faultFn = func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{this is not json`)) }
	cutOff      faultFn = func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}
)

// answering is a backend that answers status with a JSON body.
func answering(status int, body string) faultFn {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}
}

const testPassword = "correct horse"

// newFaultStack builds the stack and signs the first user up (an administrator, id 1). Signing up
// is slow on purpose (password hashing), so tests that never need a session use newAnonymousStack.
func newFaultStack(t *testing.T) *faultStack {
	t.Helper()
	fs := newAnonymousStack(t)
	if res := fs.post("/signup", url.Values{"name": {"root"}, "email": {"root@example.com"}, "password": {testPassword}, "confirm": {testPassword}}); res.code != http.StatusSeeOther {
		t.Fatalf("signup: %d %s", res.code, res.body)
	}
	return fs
}

// newAnonymousStack is newFaultStack without a signed-in user.
func newAnonymousStack(t *testing.T) *faultStack {
	t.Helper()
	bs, _, _ := newBackendServer(t)
	target, _ := url.Parse(bs.URL)
	fs := &faultStack{t: t, faults: map[string]faultFn{}, except: map[string]bool{}}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, apiclient.Prefix)
		fs.mu.Lock()
		fn := fs.faults[key]
		if fn == nil && fs.all != nil && !fs.except[key] {
			fn = fs.all
		}
		fs.mu.Unlock()
		if fn != nil {
			fn(w, r)
			return
		}
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	client, err := apiclient.New(proxy.URL, nil, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFrontend(client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	f.DisableLimits()
	fs.f = f
	fs.web = httptest.NewServer(f.Handler())
	t.Cleanup(fs.web.Close)
	jar, _ := cookiejar.New(nil)
	fs.c = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return fs
}

// fail makes one call of the internal API fail, for example fail(serverError, "GET", "/session").
func (fs *faultStack) fail(fn faultFn, method, path string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.faults[method+" "+path] = fn
}

// failAll makes every call fail except the listed ones ("GET /session"), which is how a test of
// one handler lets the guards of its page pass and breaks the handler's own call.
func (fs *faultStack) failAll(fn faultFn, except ...string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.all = fn
	for _, e := range except {
		fs.except[e] = true
	}
}

// heal removes every fault.
func (fs *faultStack) heal() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.faults, fs.except, fs.all = map[string]faultFn{}, map[string]bool{}, nil
}

type result struct {
	code   int
	body   string
	header http.Header
}

func (fs *faultStack) do(req *http.Request) result {
	fs.t.Helper()
	resp, err := fs.c.Do(req)
	if err != nil {
		fs.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, string(b), resp.Header}
}

func (fs *faultStack) get(path string) result {
	fs.t.Helper()
	req, _ := http.NewRequest("GET", fs.web.URL+path, nil)
	return fs.do(req)
}

func (fs *faultStack) post(path string, form url.Values) result {
	fs.t.Helper()
	req, _ := http.NewRequest("POST", fs.web.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return fs.do(req)
}

// postRaw posts a body that is not necessarily a valid form.
func (fs *faultStack) postRaw(path, body string) result {
	fs.t.Helper()
	req, _ := http.NewRequest("POST", fs.web.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return fs.do(req)
}

// anonymous is a request without the session of the signed-in user.
func (fs *faultStack) anonymous(method, path string, form url.Values) result {
	fs.t.Helper()
	req, _ := http.NewRequest(method, fs.web.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		fs.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, string(b), resp.Header}
}

// logOutput captures what the process logs until the test ends.
type logOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logOutput) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logOutput) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureLog(t *testing.T) *logOutput {
	t.Helper()
	out := &logOutput{}
	old := log.Logger
	log.Logger = zerolog.New(out)
	t.Cleanup(func() { log.Logger = old })
	return out
}
