package frontend

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
)

func peerFingerprint(t *testing.T, addr string) string {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) // the test trusts the server's own certificate
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sum := sha256.Sum256(conn.ConnectionState().PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}

// The frontend owns the HTTPS listener: the certificate and the port come from the backend's
// settings, and a new certificate is served without restarting anything.
func TestSplitFrontendServesHTTPSWithTheBackendsCertificate(t *testing.T) {
	bs, b, st := newBackendServer(t)
	ctx := context.Background()
	port := tlsFreePort(t, false)
	addr := net.JoinHostPort("127.0.0.1", port)
	putSettings(t, b, st, map[string]string{settingTLSMode: apiclient.TLSSelfSigned, settingHTTPS: port})
	if conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("the backend started an HTTPS listener of its own")
	}

	f, err := NewFrontend(splitClient(t, bs))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handler := f.Handler()
	hs := &http.Server{Handler: handler}
	go hs.Serve(f.WrapListener(ln))
	t.Cleanup(func() { hs.Close() })
	f.StartListeners(handler)
	if err := f.RefreshConfig(ctx); err != nil { // the settings arrive, the listener starts
		t.Fatal(err)
	}

	var fp string
	for deadline := time.Now().Add(3 * time.Second); ; {
		if conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}); err == nil {
			conn.Close()
			fp = peerFingerprint(t, addr)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the frontend did not start HTTPS")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if want := backendFingerprint(t, bs); fp != want {
		t.Errorf("the frontend serves certificate %s, the backend has %s", fp, want)
	}
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}).Get("https://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Strict-Transport-Security") == "" {
		t.Errorf("healthz over HTTPS: %d %v", resp.StatusCode, resp.Header)
	}

	// The administrator uploads another certificate: the frontend serves it on the next fetch.
	certPEM, keyPEM := selfSignedPEM(t, "127.0.0.1", "localhost")
	putSettings(t, b, st, map[string]string{settingTLSMode: apiclient.TLSUpload, settingTLSCert: certPEM, settingTLSKey: keyPEM})
	if err := f.RefreshConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := peerFingerprint(t, addr), backendFingerprint(t, bs); got != want || got == fp {
		t.Errorf("after the upload the frontend serves %s (backend %s, before %s)", got, want, fp)
	}

	// The backend goes back to plain HTTP: the frontend closes HTTPS.
	putSettings(t, b, st, map[string]string{settingTLSMode: apiclient.TLSOff})
	f.RefreshConfig(ctx)
	if conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		conn.Close()
		t.Error("HTTPS is still listening after the backend turned it off")
	}
}
