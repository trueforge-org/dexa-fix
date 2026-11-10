//go:build linux

package frontend

import (
	"net"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

func TestHTTPSThatLosesItsListenerLogsThatItStopped(t *testing.T) {
	out := captureLog(t)
	f := bareFrontend(t)
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plain.Close() })
	f.WrapListener(plain)
	f.StartListeners(f.Handler())
	port := tlsFreePort(t, false)
	cert, key := selfSignedPEM(t, "localhost", "127.0.0.1")
	if err := f.applyFrontendConfig(apiclient.FrontendConfig{TLSMode: apiclient.TLSUpload, CertPEM: cert, KeyPEM: key, HTTPSPort: port}); err != nil {
		t.Fatal(err)
	}
	killListener(t, port)
	waitForLog(t, out, "HTTPS stopped")
}
