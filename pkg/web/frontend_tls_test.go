package web

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net"
	"net/http"
	"testing"
	"time"
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
	bs, srv, _ := newInternalServer(t)
	ctx := context.Background()
	port := tlsFreePort(t, false)
	for name, v := range map[string]string{settingTLSMode: tlsSelfSigned, settingHTTPS: port} {
		if err := srv.store.PutSetting(ctx, name, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.reloadSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if srv.https != nil {
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

	addr := net.JoinHostPort("127.0.0.1", port)
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
	if want := srv.cfg().tlsInfo.Fingerprint; fp != want {
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
	certPEM, keyPEM, err := generateSelfSigned([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{settingTLSMode: tlsUpload, settingTLSCert: certPEM, settingTLSKey: keyPEM} {
		srv.store.PutSetting(ctx, name, v)
	}
	if err := srv.reloadSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.RefreshConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := peerFingerprint(t, addr), srv.cfg().tlsInfo.Fingerprint; got != want || got == fp {
		t.Errorf("after the upload the frontend serves %s (backend %s, before %s)", got, want, fp)
	}

	// The backend goes back to plain HTTP: the frontend closes HTTPS.
	srv.store.PutSetting(ctx, settingTLSMode, tlsOff)
	srv.reloadSettings(ctx)
	f.RefreshConfig(ctx)
	if conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		conn.Close()
		t.Error("HTTPS is still listening after the backend turned it off")
	}
}
