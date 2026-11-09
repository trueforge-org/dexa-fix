package cmd

import (
	"bytes"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func apiFlags(t *testing.T, kv ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "api"}
	addInternalTLSFlags(c)
	for i := 0; i < len(kv); i += 2 {
		if err := c.Flags().Set(kv[i], kv[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func webFlags(t *testing.T, kv ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "web"}
	addBackendTLSFlags(c)
	for i := 0; i < len(kv); i += 2 {
		if err := c.Flags().Set(kv[i], kv[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// makeSet writes a certificate set with the real command and returns its directory.
func makeSet(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mtls")
	c := &cobra.Command{Use: "mtls"}
	c.Flags().String("dir", dir, "")
	c.Flags().StringSlice("name", []string{"api"}, "")
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&bytes.Buffer{})
	if err := secretsMTLSCmd.RunE(c, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--backend-url https://api:8081") {
		t.Errorf("no usage hint: %q", out.String())
	}
	return dir
}

func TestSecretsMTLSCreatesASetAndNeverOverwrites(t *testing.T) {
	dir := makeSet(t)
	for name, mode := range map[string]os.FileMode{"ca.pem": 0o644, "api.pem": 0o644, "api.key": 0o600, "web.pem": 0o644, "web.key": 0o600} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", name, fi, err)
		}
	}
	before, _ := os.ReadFile(filepath.Join(dir, "api.key"))
	c := &cobra.Command{Use: "mtls"}
	c.Flags().String("dir", dir, "")
	c.Flags().StringSlice("name", []string{"api"}, "")
	c.SetErr(&bytes.Buffer{})
	if err := secretsMTLSCmd.RunE(c, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second run: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "api.key")); !bytes.Equal(before, after) {
		t.Error("an existing key was overwritten")
	}
	c.Flags().Set("name", "")
	c2 := &cobra.Command{Use: "mtls"}
	c2.Flags().String("dir", t.TempDir(), "")
	c2.Flags().StringSlice("name", nil, "")
	if err := secretsMTLSCmd.RunE(c2, nil); err == nil {
		t.Error("a set without any name was created")
	}
}

func TestInternalServerTLSNeedsAllThreeFiles(t *testing.T) {
	if cfg, err := internalServerTLS(apiFlags(t)); cfg != nil || err != nil {
		t.Errorf("no flags: %v %v", cfg, err)
	}
	if _, err := internalServerTLS(apiFlags(t, "internal-tls-cert-file", "x.pem")); err == nil {
		t.Error("one file accepted")
	}
	dir := makeSet(t)
	cfg, err := internalServerTLS(apiFlags(t,
		"internal-tls-cert-file", filepath.Join(dir, "api.pem"),
		"internal-tls-key-file", filepath.Join(dir, "api.key"),
		"internal-tls-client-ca-file", filepath.Join(dir, "ca.pem")))
	if err != nil || cfg == nil || cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("%v %v", cfg, err)
	}
}

func TestBackendHTTPClientRules(t *testing.T) {
	dir := makeSet(t)
	all := []string{"backend-ca-file", filepath.Join(dir, "ca.pem"), "backend-client-cert-file", filepath.Join(dir, "web.pem"), "backend-client-key-file", filepath.Join(dir, "web.key")}

	if _, secured, err := backendHTTPClient(webFlags(t), "http://api:8081"); err != nil || secured {
		t.Errorf("plain: secured=%v err=%v", secured, err)
	}
	if _, secured, err := backendHTTPClient(webFlags(t, all...), "https://api:8081"); err != nil || !secured {
		t.Errorf("mutual TLS: secured=%v err=%v", secured, err)
	}
	if _, _, err := backendHTTPClient(webFlags(t), "https://api:8081"); err == nil {
		t.Error("an https address without certificates was accepted")
	}
	if _, _, err := backendHTTPClient(webFlags(t, all[:2]...), "https://api:8081"); err == nil {
		t.Error("a CA without a client certificate was accepted")
	}
	if _, _, err := backendHTTPClient(webFlags(t, all...), "http://api:8081"); err == nil {
		t.Error("certificates with an http address were accepted: they would be silently unused")
	}
	if _, _, err := backendHTTPClient(webFlags(t), "nonsense"); err == nil {
		t.Error("nonsense accepted")
	}
}

// The files `secrets mtls` writes, loaded by the helpers the commands use, secure a real listener.
func TestGeneratedSetSecuresTheInternalLink(t *testing.T) {
	dir := makeSet(t)
	srvCfg, err := internalServerTLS(apiFlags(t,
		"internal-tls-cert-file", filepath.Join(dir, "api.pem"),
		"internal-tls-key-file", filepath.Join(dir, "api.key"),
		"internal-tls-client-ca-file", filepath.Join(dir, "ca.pem")))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	ts.TLS = srvCfg
	ts.StartTLS()
	t.Cleanup(ts.Close)

	good, _, err := backendHTTPClient(webFlags(t,
		"backend-ca-file", filepath.Join(dir, "ca.pem"),
		"backend-client-cert-file", filepath.Join(dir, "web.pem"),
		"backend-client-key-file", filepath.Join(dir, "web.key")), ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := good.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// A client that does not present the certificate is refused by the listener.
	bare := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if resp, err := bare.Get(ts.URL); err == nil {
		resp.Body.Close()
		t.Error("a client without a certificate was served")
	}
}
