package web

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

// tlsRig is a server as cmd/serve starts it: a plain HTTP listener on 127.0.0.1, with the HTTPS and
// HTTP/3 listeners that the Admin page starts, stops and moves.
type tlsRig struct {
	srv       *Server
	base      string       // plain HTTP URL
	plainPort string       // port of the plain listener
	admin     *http.Client // signed in as the first account, the administrator
}

func newTLSRig(t *testing.T) *tlsRig {
	t.Helper()
	ts, _, _ := newAuthServer(t)
	return startTLSRig(t, serverOf(ts))
}

// newEncryptedTLSRig is newTLSRig with encryption of the stored settings turned on.
func newEncryptedTLSRig(t *testing.T) *tlsRig {
	t.Helper()
	ctx := context.Background()
	key, err := sqlstore.ParseKey(strings.Repeat("cd", sqlstore.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := sqlstore.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)", sqlstore.WithCipher(cipher))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutSetting(ctx, settingSignup, valueEnabled); err != nil {
		t.Fatal(err)
	}
	srv, err := New(st, importer.NewRegistry(withings.New(withings.Config{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return startTLSRig(t, srv)
}

// startTLSRig serves srv as cmd/serve does, with the plain listener on a free port of 127.0.0.1,
// and signs up the administrator over plain HTTP.
func startTLSRig(t *testing.T, srv *Server) *tlsRig {
	t.Helper()
	skipInSplit(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := srv.WrapListener(ln)
	handler := srv.Handler()
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	plain := &http.Server{Handler: handler, Protocols: &protocols}
	go func() { _ = plain.Serve(wrapped) }()
	srv.StartListeners(handler)
	t.Cleanup(func() {
		_ = plain.Close()
		srv.Close()
	})
	rig := &tlsRig{
		srv:       srv,
		base:      "http://" + ln.Addr().String(),
		plainPort: strconv.Itoa(ln.Addr().(*net.TCPAddr).Port),
	}
	rig.admin = rig.signUp(t, "admin")
	return rig
}

// tlsCookieTransport sends a session cookie with every request.
type tlsCookieTransport struct {
	cookie *http.Cookie
	next   http.RoundTripper
}

func (c tlsCookieTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.AddCookie(c.cookie)
	return c.next.RoundTrip(r)
}

// signUp creates an account over plain HTTP and returns a client that is signed in as it.
func (r *tlsRig) signUp(t *testing.T, name string) *http.Client {
	t.Helper()
	resp := post(t, noRedirectClient(), r.base+"/signup", url.Values{"name": {name}, "email": {name + "@example.com"}, "password": {testPassword}, "confirm": {testPassword}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("signup %s: %d", name, resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie && ck.MaxAge > 0 {
			c := noRedirectClient()
			c.Transport = tlsCookieTransport{cookie: ck, next: http.DefaultTransport}
			return c
		}
	}
	t.Fatalf("no session cookie for %s", name)
	return nil
}

// saveTLS posts the Admin page's TLS form as the administrator, against the plain port.
func (r *tlsRig) saveTLS(t *testing.T, form url.Values) *http.Response {
	t.Helper()
	resp, err := r.admin.PostForm(r.base+"/admin/tls", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// tlsFlash checks that a save went back to the Admin page and returns the message it carries.
func tlsFlash(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Path != "/admin" {
		t.Fatalf("redirect to %q, want /admin", resp.Header.Get("Location"))
	}
	return loc.Query().Get("msg")
}

// tlsFreePort returns a port that is free on TCP and, when udp is set, on UDP too (HTTP/3 uses the
// HTTPS port number on UDP). The test is skipped when no such port can be found.
func tlsFreePort(t *testing.T, udp bool) string {
	t.Helper()
	for range 5 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		ln.Close()
		if udp {
			pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", port))
			if err != nil {
				continue
			}
			pc.Close()
		}
		return port
	}
	t.Skip("no port is free for HTTP/3 (UDP) in this environment")
	return ""
}

// tlsClient is a client for the HTTPS server, whose certificate is self-signed. Every client has
// its own connections, so a changed certificate is seen by the next request.
func tlsClient(t *testing.T, h2 bool) *http.Client {
	t.Helper()
	cfg := &tls.Config{InsecureSkipVerify: true} // the test trusts the server's own certificate
	if !h2 {
		cfg.NextProtos = []string{"http/1.1"}
	}
	tr := &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: h2}
	t.Cleanup(tr.CloseIdleConnections)
	c := noRedirectClient()
	c.Transport = tr
	c.Timeout = 10 * time.Second
	return c
}

// tlsH3Client is a client for HTTP/3 with a per-request limit of d.
func tlsH3Client(t *testing.T, d time.Duration) *http.Client {
	t.Helper()
	tr := &http3.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // self-signed, as above
	t.Cleanup(func() { _ = tr.Close() })
	c := noRedirectClient()
	c.Transport = tr
	c.Timeout = d
	return c
}

// tlsFetch GETs u and returns the response, with its body read as a string.
func tlsFetch(c *http.Client, u string) (*http.Response, string, error) {
	resp, err := c.Get(u)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, string(b), err
}

// tlsWaitOK polls u until it answers 200 "ok", for up to d. The listeners start in the background,
// so the first request can come a little early.
func tlsWaitOK(t *testing.T, c *http.Client, u string, d time.Duration) *http.Response {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		resp, text, err := tlsFetch(c, u)
		if err == nil && resp.StatusCode == http.StatusOK && text == "ok" {
			return resp
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("status %d, body %q", resp.StatusCode, text)
			}
			t.Fatalf("GET %s: %v", u, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// tlsLeafFingerprint is the SHA-256 of the certificate the server presented.
func tlsLeafFingerprint(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("the response has no TLS certificate")
	}
	sum := sha256.Sum256(resp.TLS.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}

// tlsPEMFingerprint is the SHA-256 of the first certificate in a PEM chain.
func tlsPEMFingerprint(t *testing.T, certPEM string) string {
	t.Helper()
	blk, _ := pem.Decode([]byte(certPEM))
	if blk == nil {
		t.Fatal("certificate is not PEM")
	}
	sum := sha256.Sum256(blk.Bytes)
	return hex.EncodeToString(sum[:])
}

// tlsServedFingerprint returns the certificate a new HTTPS connection presents.
func tlsServedFingerprint(t *testing.T, port string) string {
	t.Helper()
	resp := tlsWaitOK(t, tlsClient(t, true), "https://127.0.0.1:"+port+"/healthz", 3*time.Second)
	return tlsLeafFingerprint(t, resp)
}

func TestHTTPSSelfSignedHTTP2(t *testing.T) {
	rig := newTLSRig(t)
	p := tlsFreePort(t, false)

	msg := tlsFlash(t, rig.saveTLS(t, url.Values{"mode": {"selfsigned"}, "port": {p}}))
	if !strings.Contains(msg, "HTTPS is on") {
		t.Fatalf("flash %q, want HTTPS to be on", msg)
	}
	u := "https://127.0.0.1:" + p + "/healthz"

	resp := tlsWaitOK(t, tlsClient(t, true), u, 3*time.Second)
	if resp.ProtoMajor != 2 {
		t.Errorf("HTTPS protocol %s, want HTTP/2", resp.Proto)
	}
	if resp.TLS == nil {
		t.Error("HTTPS response has no TLS state")
	}
	if alt := resp.Header.Get("Alt-Svc"); alt != "" {
		t.Errorf("Alt-Svc %q although HTTP/3 is off", alt)
	}

	resp1, text, err := tlsFetch(tlsClient(t, false), u)
	if err != nil {
		t.Fatal(err)
	}
	if resp1.ProtoMajor != 1 || text != "ok" {
		t.Errorf("HTTP/1.1 client got %s %q, want HTTP/1.1 \"ok\"", resp1.Proto, text)
	}

	if code, text := body(t, rig.admin, rig.base+"/healthz"); code != http.StatusOK || text != "ok" {
		t.Errorf("plain port: %d %q", code, text)
	}

	// Signing in over HTTPS sets Secure session cookies.
	lresp, err := tlsClient(t, true).PostForm("https://127.0.0.1:"+p+"/login", url.Values{"email": {"admin@example.com"}, "password": {testPassword}})
	if err != nil {
		t.Fatal(err)
	}
	lresp.Body.Close()
	if lresp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in over HTTPS: %d", lresp.StatusCode)
	}
	found := false
	for _, ck := range lresp.Cookies() {
		if ck.Name == sessionCookie && ck.MaxAge > 0 {
			found = true
			if !ck.Secure {
				t.Error("session cookie set over HTTPS is not Secure")
			}
		}
	}
	if !found {
		t.Error("no session cookie after sign-in over HTTPS")
	}
}

func TestHTTP3(t *testing.T) {
	rig := newTLSRig(t)
	p := tlsFreePort(t, true)
	u := "https://127.0.0.1:" + p + "/healthz"

	msg := tlsFlash(t, rig.saveTLS(t, url.Values{"mode": {"selfsigned"}, "port": {p}, "http3": {"1"}}))
	if !strings.Contains(msg, "HTTP/3 listens on UDP port") {
		t.Fatalf("flash %q, want HTTP/3 to be on", msg)
	}

	resp := tlsWaitOK(t, tlsClient(t, true), u, 3*time.Second)
	if alt := resp.Header.Get("Alt-Svc"); !strings.Contains(alt, `h3=":`+p+`"`) {
		t.Errorf("Alt-Svc %q does not advertise HTTP/3 on port %s", alt, p)
	}

	h3resp := tlsWaitOK(t, tlsH3Client(t, 3*time.Second), u, 5*time.Second)
	if h3resp.Proto != "HTTP/3.0" {
		t.Errorf("HTTP/3 client got protocol %s, want HTTP/3.0", h3resp.Proto)
	}

	// Turning HTTP/3 off stops it; HTTPS stays on.
	if msg := tlsFlash(t, rig.saveTLS(t, url.Values{"mode": {"selfsigned"}, "port": {p}})); !strings.Contains(msg, "HTTPS is on") {
		t.Fatalf("flash %q after HTTP/3 was turned off", msg)
	}
	if _, _, err := tlsFetch(tlsH3Client(t, 3*time.Second), u); err == nil {
		t.Error("HTTP/3 still answers after it was turned off")
	}
	resp = tlsWaitOK(t, tlsClient(t, true), u, 3*time.Second)
	if alt := resp.Header.Get("Alt-Svc"); alt != "" {
		t.Errorf("Alt-Svc %q after HTTP/3 was turned off", alt)
	}

	// Mode off stops HTTPS: the port refuses connections.
	if msg := tlsFlash(t, rig.saveTLS(t, url.Values{"mode": {"off"}, "port": {p}})); !strings.Contains(msg, "TLS is off") {
		t.Fatalf("flash %q after TLS was turned off", msg)
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+p, 2*time.Second)
	if err == nil {
		conn.Close()
		t.Error("HTTPS port still accepts connections with TLS off")
	}
}

func TestH2CPlain(t *testing.T) {
	rig := newTLSRig(t)
	var p http.Protocols
	p.SetHTTP1(false)
	p.SetUnencryptedHTTP2(true)
	c := noRedirectClient()
	c.Transport = &http.Transport{Protocols: &p}

	resp, text, err := tlsFetch(c, rig.base+"/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.ProtoMajor != 2 || text != "ok" {
		t.Errorf("cleartext HTTP/2 client got %s %q, want HTTP/2 \"ok\"", resp.Proto, text)
	}
}

func TestUploadedCertificate(t *testing.T) {
	rig := newTLSRig(t)
	p := tlsFreePort(t, false)
	certPEM, keyPEM, err := generateSelfSigned([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	want := tlsPEMFingerprint(t, certPEM)

	tlsFlash(t, rig.saveTLS(t, url.Values{"mode": {"upload"}, "port": {p}, "cert": {certPEM}, "key": {keyPEM}}))
	if got := tlsServedFingerprint(t, p); got != want {
		t.Fatalf("presented certificate %s, want the uploaded one %s", got, want)
	}

	// Saving again with no PEM keeps the stored certificate.
	tlsFlash(t, rig.saveTLS(t, url.Values{"mode": {"upload"}, "port": {p}, "cert": {""}, "key": {""}}))
	if got := tlsServedFingerprint(t, p); got != want {
		t.Fatalf("after saving without PEM the presented certificate is %s, want %s", got, want)
	}

	// A key that does not belong to the certificate is refused and the old one keeps serving.
	_, otherKey, err := generateSelfSigned([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	msg := tlsFlash(t, rig.saveTLS(t, url.Values{"mode": {"upload"}, "port": {p}, "cert": {certPEM}, "key": {otherKey}}))
	if !strings.Contains(msg, "do not belong together") {
		t.Errorf("mismatched key: flash %q, want a refusal", msg)
	}
	if got := tlsServedFingerprint(t, p); got != want {
		t.Errorf("after a refused key the presented certificate is %s, want the old %s", got, want)
	}

	// A key that is not PEM at all is refused the same way.
	msg = tlsFlash(t, rig.saveTLS(t, url.Values{"mode": {"upload"}, "port": {p}, "cert": {certPEM}, "key": {"garbage"}}))
	if !strings.Contains(msg, "do not belong together") {
		t.Errorf("garbage key: flash %q, want a refusal", msg)
	}
	if got := tlsServedFingerprint(t, p); got != want {
		t.Errorf("after a garbage key the presented certificate is %s, want the old %s", got, want)
	}
}

func TestUploadedKeyIsEncryptedAtRest(t *testing.T) {
	rig := newEncryptedTLSRig(t)
	p := tlsFreePort(t, false)
	certPEM, keyPEM, err := generateSelfSigned([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	want := tlsPEMFingerprint(t, certPEM)
	tlsFlash(t, rig.saveTLS(t, url.Values{"mode": {"upload"}, "port": {p}, "cert": {certPEM}, "key": {keyPEM}}))
	if got := tlsServedFingerprint(t, p); got != want {
		t.Fatalf("presented certificate %s, want %s", got, want)
	}

	// The settings table is read directly, under the same name as the store's in-memory database.
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var raw string
	if err := db.QueryRow("SELECT value FROM app_settings WHERE name = ?", settingTLSKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "enc:") || strings.Contains(raw, "PRIVATE KEY") {
		t.Error("the stored TLS private key is not encrypted")
	}
}

func TestTLSSettingsValidation(t *testing.T) {
	rig := newTLSRig(t)
	p := tlsFreePort(t, false)

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"port is the plain port", url.Values{"mode": {"selfsigned"}, "port": {rig.plainPort}}, "must differ"},
		{"port 0", url.Values{"mode": {"selfsigned"}, "port": {"0"}}, "from 1 to 65535"},
		{"port 70000", url.Values{"mode": {"selfsigned"}, "port": {"70000"}}, "from 1 to 65535"},
		{"port not a number", url.Values{"mode": {"selfsigned"}, "port": {"abc"}}, "from 1 to 65535"},
		{"HTTP/3 without TLS", url.Values{"mode": {"off"}, "port": {p}, "http3": {"1"}}, "HTTP/3 needs TLS"},
		{"name with a space", url.Values{"mode": {"selfsigned"}, "port": {p}, "names": {"a b"}}, "without spaces"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := tlsFlash(t, rig.saveTLS(t, c.form))
			if !strings.Contains(msg, c.want) {
				t.Errorf("flash %q, want it to mention %q", msg, c.want)
			}
		})
	}
	if rig.srv.cfg().tlsCert != nil {
		t.Fatal("a refused change turned TLS on")
	}

	// A non-administrator gets 404 and changes nothing.
	bob := rig.signUp(t, "bob")
	resp, err := bob.PostForm(rig.base+"/admin/tls", url.Values{"mode": {"selfsigned"}, "port": {p}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("non-admin POST /admin/tls: %d, want 404", resp.StatusCode)
	}
	if rig.srv.cfg().tlsCert != nil {
		t.Error("a non-admin turned TLS on")
	}

	// The Admin page has the section.
	if code, text := body(t, rig.admin, rig.base+"/admin"); code != http.StatusOK || !strings.Contains(text, "HTTPS, HTTP/2 and HTTP/3") {
		t.Errorf("Admin page: %d, section missing", code)
	}

	// A port taken by another listener: the Admin page says why HTTPS did not start.
	busyPort := tlsFreePort(t, false)
	busy, err := net.Listen("tcp", "127.0.0.1:"+busyPort)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { busy.Close() })
	msg := tlsFlash(t, rig.saveTLS(t, url.Values{"mode": {"selfsigned"}, "port": {busyPort}}))
	if !strings.Contains(msg, "could not listen") {
		t.Errorf("busy port: flash %q, want the bind error", msg)
	}
	if code, text := body(t, rig.admin, rig.base+"/admin"); code != http.StatusOK || !strings.Contains(text, "could not listen") {
		t.Errorf("Admin page after a bind error: %d, bind error not shown", code)
	}
}

func TestTLSBadStoredCertificateFallsBack(t *testing.T) {
	rig := newTLSRig(t)
	ctx := context.Background()
	for name, value := range map[string]string{settingTLSMode: apiclient.TLSUpload, settingTLSCert: "garbage", settingTLSKey: "garbage"} {
		if err := rig.srv.store.PutSetting(ctx, name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := rig.srv.reloadSettings(ctx); err != nil {
		t.Fatalf("reloadSettings: %v", err)
	}
	if rig.srv.cfg().tlsCert != nil {
		t.Error("an unusable stored certificate was put in force")
	}
	if code, text := body(t, rig.admin, rig.base+"/healthz"); code != http.StatusOK || text != "ok" {
		t.Errorf("plain port after a bad certificate: %d %q", code, text)
	}
}
