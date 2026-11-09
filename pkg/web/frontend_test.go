package web

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/shared/edge"
)

func TestFrontendFollowsBackendSettings(t *testing.T) {
	bs, srv, _ := newInternalServer(t)
	f, err := NewFrontend(splitClient(t, bs))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	ctx := context.Background()
	if err := f.RefreshConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if f.cfg().baseURL != nil || len(f.cfg().trusted) != 0 {
		t.Fatalf("unexpected settings %+v", f.cfg())
	}

	// An administrator saves settings on the backend.
	for name, v := range map[string]string{settingBaseURL: "https://dexa.example.com", settingTrustedProxies: "10.0.0.0/8", settingProxyProtocol: valueEnabled} {
		if err := srv.store.PutSetting(ctx, name, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.reloadSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.RefreshConfig(ctx); err != nil {
		t.Fatal(err)
	}
	l := f.cfg()
	if l.baseURL == nil || l.baseURL.String() != "https://dexa.example.com" || !l.proxyProtocol || !edge.Trusted(l.trusted, netip.MustParseAddr("10.1.2.3")) {
		t.Errorf("settings not applied: %+v", l)
	}

	// Nothing changed: the same settings stay in force (a "not modified" answer).
	before := f.cfg()
	if err := f.RefreshConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if f.cfg() != before {
		t.Error("settings were replaced although nothing changed")
	}
}

func TestFrontendShipsCertificateOnlyWithAValidSecret(t *testing.T) {
	bs, srv, _ := newInternalServer(t)
	ctx := context.Background()
	if err := srv.store.PutSetting(ctx, settingTLSMode, apiclient.TLSSelfSigned); err != nil {
		t.Fatal(err)
	}
	if err := srv.reloadSettings(ctx); err != nil {
		t.Fatal(err)
	}
	f, _ := NewFrontend(splitClient(t, bs))
	t.Cleanup(f.Close)
	if err := f.RefreshConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if f.cfg().tlsCert == nil || f.cfg().tlsInfo == nil || f.cfg().tlsInfo.Fingerprint != srv.cfg().tlsInfo.Fingerprint {
		t.Fatalf("the frontend did not get the backend's certificate: %+v", f.cfg().tlsInfo)
	}
	// A caller without the secret gets nothing, in particular no private key.
	bad, err := newClientWithSecret(bs.URL, "not-the-secret-0123456789abcdef012345")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bad.FrontendConfig(ctx, ""); err == nil {
		t.Error("the config was served without the service secret")
	}
	f2, _ := NewFrontend(bad)
	t.Cleanup(f2.Close)
	err = f2.WaitForBackend(ctx, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "service secret") {
		t.Errorf("WaitForBackend: %v", err)
	}
}

func TestFrontendPollsForChanges(t *testing.T) {
	bs, srv, _ := newInternalServer(t)
	old := configPollInterval
	configPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { configPollInterval = old })

	f, _ := NewFrontend(splitClient(t, bs))
	ctx := context.Background()
	if err := f.RefreshConfig(ctx); err != nil {
		t.Fatal(err)
	}
	f.StartConfigPoll()
	t.Cleanup(f.Close)

	srv.store.PutSetting(ctx, settingBaseURL, "http://poll.example.com")
	srv.reloadSettings(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for f.cfg().baseURL == nil || f.cfg().baseURL.Host != "poll.example.com" {
		if time.Now().After(deadline) {
			t.Fatal("the poller did not pick up the new base URL")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
