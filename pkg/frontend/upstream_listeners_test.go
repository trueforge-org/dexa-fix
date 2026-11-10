package frontend

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// bareFrontend is a frontend whose backend does not matter: for the parts that never ask it.
func bareFrontend(t *testing.T) *Frontend {
	t.Helper()
	c, err := apiclient.New("http://127.0.0.1:1", nil, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFrontend(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	f.DisableLimits()
	return f
}

func TestTheAPIUpstreamGetsOnlyWhatThisProcessSettled(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		switch r.URL.Path {
		case "/api/v1/public":
			w.Header().Set("Cache-Control", "public, max-age=60")
		case "/api/v1/private":
			w.Header().Set("Cache-Control", "private, no-cache")
		}
		w.Header().Set("Content-Security-Policy", "default-src *") // this process adds its own
		w.Write([]byte("{}"))
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	f := bareFrontend(t)
	f.SetAPIUpstream(target)
	web := httptest.NewServer(f.Handler())
	t.Cleanup(web.Close)

	get := func(path string, h map[string]string) *http.Response {
		req, _ := http.NewRequest("GET", web.URL+path, nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	t.Run("a visitor cannot choose the forwarded headers", func(t *testing.T) {
		get("/api/v1/x", map[string]string{"X-Forwarded-For": "203.0.113.9", "X-Forwarded-Proto": "https", "X-Real-Ip": "203.0.113.9"})
		if got := seen.Get("X-Forwarded-For"); got != "127.0.0.1" || seen.Get("X-Forwarded-Proto") != "http" || seen.Get("X-Real-Ip") != "" {
			t.Errorf("the upstream saw %v", seen)
		}
	})
	t.Run("a connection settled as secure is passed on as secure", func(t *testing.T) {
		f.swapSettings(&frontendSettings{baseURL: &url.URL{Scheme: "https", Host: "dexa.example.com"}, tlsMode: apiclient.TLSOff, httpsPort: defaultHTTPSPort})
		get("/api/v1/x", nil)
		if seen.Get("X-Forwarded-Proto") != "https" {
			t.Errorf("the upstream saw %v", seen)
		}
	})
	t.Run("caching that a shared cache could use is never let through", func(t *testing.T) {
		for path, want := range map[string]string{
			"/api/v1/public":  "no-store", // public: not allowed
			"/api/v1/none":    "no-store", // nothing said: not allowed
			"/api/v1/private": "private, no-cache",
		} {
			resp := get(path, nil)
			if got := resp.Header.Get("Cache-Control"); got != want {
				t.Errorf("%s: Cache-Control %q, want %q", path, got, want)
			}
			if resp.Header.Get("CDN-Cache-Control") != "no-store" || resp.Header.Get("Surrogate-Control") != "no-store" {
				t.Errorf("%s: a CDN may cache it: %v", path, resp.Header)
			}
			if strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src *") {
				t.Errorf("%s: the upstream's policy was passed on", path)
			}
		}
	})
	t.Run("an upstream that is down is a 502 in JSON", func(t *testing.T) {
		out := captureLog(t)
		upstream.Close()
		req, _ := http.NewRequest("GET", web.URL+"/api/v1/x", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Cache-Control") != "no-store" ||
			!strings.Contains(resp.Header.Get("Content-Type"), "json") {
			t.Errorf("%d %v", resp.StatusCode, resp.Header)
		}
		waitForLog(t, out, "the API upstream failed")
	})
}

func TestThePortOfHTTPSMustDifferFromThePortOfPlainHTTP(t *testing.T) {
	fs := newFaultStack(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	fs.f.WrapListener(ln)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	res := fs.post("/admin/tls", v("mode", "selfsigned", "port", port))
	if res.code != http.StatusSeeOther || !strings.Contains(res.header.Get("Location"), url.QueryEscape("must differ from the port of plain HTTP ("+port+")")) {
		t.Fatalf("%d %v", res.code, res.header)
	}
}

func TestAnHTTPSPortThatIsTakenIsReportedAndPlainHTTPKeepsWorking(t *testing.T) {
	out := captureLog(t)
	f := bareFrontend(t)
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: f.Handler()}
	go hs.Serve(f.WrapListener(plain))
	t.Cleanup(func() { hs.Close() })
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { taken.Close() })
	_, port, _ := net.SplitHostPort(taken.Addr().String())

	f.StartListeners(f.Handler())
	cert, key := selfSignedPEM(t, "localhost", "127.0.0.1")
	if err := f.applyFrontendConfig(apiclient.FrontendConfig{TLSMode: apiclient.TLSUpload, CertPEM: cert, KeyPEM: key, HTTPSPort: port}); err != nil {
		t.Fatal(err)
	}
	if msg := f.listenError(); !strings.Contains(msg, "HTTPS could not listen on") {
		t.Fatalf("listen error %q", msg)
	}
	waitForLog(t, out, "HTTPS could not start")
	resp, err := http.Get("http://" + plain.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plain HTTP: %d", resp.StatusCode)
	}
}

func TestHTTP3IsStartedAndStoppedWithTheFrontend(t *testing.T) {
	f := bareFrontend(t)
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plain.Close() })
	f.WrapListener(plain)
	f.StartListeners(f.Handler())
	port := tlsFreePort(t, true)
	cert, key := selfSignedPEM(t, "localhost", "127.0.0.1")
	if err := f.applyFrontendConfig(apiclient.FrontendConfig{TLSMode: apiclient.TLSUpload, CertPEM: cert, KeyPEM: key, HTTPSPort: port, HTTP3: true}); err != nil {
		t.Fatal(err)
	}
	f.lmu.Lock()
	on := f.https != nil && f.h3 != nil
	f.lmu.Unlock()
	if !on {
		t.Fatal("HTTPS and HTTP/3 were not started")
	}
	f.Close()
	f.lmu.Lock()
	off := f.https == nil && f.h3 == nil
	f.lmu.Unlock()
	if !off {
		t.Fatal("Close left a listener running")
	}
	// The serving goroutine closes its listener as it ends, a moment after Close returned.
	for deadline := time.Now().Add(5 * time.Second); ; {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), time.Second)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("HTTPS still answers after Close")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHTTP3ThatCannotBindIsLoggedAndTheRestKeepsRunning(t *testing.T) {
	out := captureLog(t)
	f := bareFrontend(t)
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plain.Close() })
	f.WrapListener(plain)
	f.StartListeners(f.Handler())
	port := tlsFreePort(t, true)
	busy, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Skip("the UDP port was taken in between")
	}
	t.Cleanup(func() { busy.Close() })
	cert, key := selfSignedPEM(t, "localhost", "127.0.0.1")
	if err := f.applyFrontendConfig(apiclient.FrontendConfig{TLSMode: apiclient.TLSUpload, CertPEM: cert, KeyPEM: key, HTTPSPort: port, HTTP3: true}); err != nil {
		t.Fatal(err)
	}
	waitForLog(t, out, "HTTP/3 stopped")
}

func TestWithoutACertificateAHandshakeIsRefused(t *testing.T) {
	f := bareFrontend(t)
	if _, err := f.tlsConfig().GetCertificate(nil); err == nil {
		t.Fatal("a certificate was handed out although none is configured")
	}
}

func TestTheHTTPSAddressUsesTheInterfaceOfPlainHTTP(t *testing.T) {
	f := bareFrontend(t)
	if got := f.bindAddr("8443"); got != ":8443" {
		t.Errorf("without a plain listener: %q", got)
	}
	f.listenAddr = "127.0.0.1:8080"
	if got := f.bindAddr("8443"); got != "127.0.0.1:8443" {
		t.Errorf("with one: %q", got)
	}
}

func TestAnAnswerCutOffInTheMiddleOfAnExportIsLogged(t *testing.T) {
	out := captureLog(t)
	fs := newFaultStack(t)
	fs.fail(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte(`{"scans":[`))
		w.(http.Flusher).Flush()
		cutOff(w, r)
	}, "GET", "/users/1/export/data")
	fs.get("/u/1/export/data")
	waitForLog(t, out, `"export":"data"`)
}

func TestAnExportTheBackendRefusesIsNotDownloaded(t *testing.T) {
	fs := newFaultStack(t)
	fs.fail(answering(http.StatusTeapot, `{}`), "GET", "/users/1/export/profile")
	if res := fs.get("/u/1/export/profile"); res.code != http.StatusTeapot || !strings.Contains(res.body, "export not available") {
		t.Fatalf("%d %q", res.code, res.body)
	}
	fs.heal()
	if res := fs.get("/u/1/export/profile"); res.code != http.StatusOK || !strings.Contains(res.header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("a healthy export: %d %v", res.code, res.header)
	}
}

func TestStepsOfAnAdminsSecondStepFailCleanly(t *testing.T) {
	fs := newFaultStack(t)
	fs.fail(serverError, "GET", "/admin/step-up")
	if res := fs.get("/admin/verify"); res.code != http.StatusServiceUnavailable {
		t.Errorf("the verify page: %d", res.code)
	}
	fs.heal()
	fs.fail(serverError, "POST", "/admin/step-up")
	if res := fs.post("/admin/verify", v("code", "123456")); res.code != http.StatusServiceUnavailable {
		t.Errorf("sending the code: %d", res.code)
	}
	fs.heal()
	if res := fs.get("/admin/verify"); res.code != http.StatusSeeOther || res.header.Get("Location") != "/admin" {
		t.Errorf("without the policy, the verify page sends the admin on: %d %v", res.code, res.header)
	}
}

func TestAnAdminPageIsNotShownWhenItsDataIsMissing(t *testing.T) {
	fs := newFaultStack(t)
	fs.fail(garbage, "GET", "/admin/page")
	if res := fs.get("/admin"); res.code == http.StatusOK {
		t.Fatalf("%d", res.code)
	}
	fs.heal()
	fs.fail(answering(http.StatusOK, `{"message":"Reset e-mail sent."}`), "POST", "/admin/users/2/reset-link")
	res := fs.post("/admin/users/2/reset-link", v("email", "1"))
	if res.code != http.StatusSeeOther || !strings.Contains(res.header.Get("Location"), "Reset+e-mail+sent.") {
		t.Fatalf("a reset link sent by e-mail: %d %v", res.code, res.header)
	}
}

func TestWorkerSettingsThatAreNotNumbersAreSentBack(t *testing.T) {
	fs := newFaultStack(t)
	fs.failAll(serverError, guards...) // the backend must not be asked
	bad := workerKindForm()
	bad.Set("max_running", "many")
	badOption := workerKindForm()
	badOption.Set("opt_pause_ms", "soon")
	for name, form := range map[string]url.Values{"a setting": bad, "an option": badOption} {
		res := fs.post("/admin/workers/kinds/withings", form)
		if res.code != http.StatusSeeOther || !strings.Contains(res.header.Get("Location"), url.QueryEscape("Every setting must be a whole number.")) {
			t.Errorf("%s: %d %v", name, res.code, res.header)
		}
	}
	res := fs.post("/admin/users/2/priority", v("priority", "high"))
	if res.code != http.StatusSeeOther || !strings.Contains(res.header.Get("Location"), url.QueryEscape("The priority must be a whole number from 1 to 100.")) {
		t.Errorf("priority: %d %v", res.code, res.header)
	}
}

func TestALongMessageOfTheWorkersPageIsCut(t *testing.T) {
	fs := newFaultStack(t)
	fs.fail(answering(http.StatusOK, `{"message":"`+strings.Repeat("y", 2*maxFlashLen)+`"}`), "POST", "/admin/workers/kinds/withings/resume")
	res := fs.post("/admin/workers/kinds/withings/resume", nil)
	loc, _ := url.Parse(res.header.Get("Location"))
	if res.code != http.StatusSeeOther || loc.Path != "/admin/workers" || len(loc.Query().Get("msg")) != maxFlashLen {
		t.Fatalf("%d %v", res.code, res.header)
	}
}

func TestTheFrontendStopsWaitingWhenItsContextEnds(t *testing.T) {
	f := bareFrontend(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.RefreshConfig(ctx); err == nil {
		t.Fatal("a cancelled refresh succeeded")
	}
}
