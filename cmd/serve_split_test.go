package cmd

import (
	"strings"
	"testing"
)

func runServe(t *testing.T, args ...string) error {
	t.Helper()
	RootCmd.SetArgs(append([]string{"serve"}, args...))
	RootCmd.SetOut(new(strings.Builder))
	RootCmd.SetErr(new(strings.Builder))
	defer RootCmd.SetArgs(nil)
	_, err := RootCmd.ExecuteC()
	return err
}

func TestServeRejectsUnknownMode(t *testing.T) {
	if err := runServe(t, "wbe"); err == nil {
		t.Error("`serve wbe` was accepted: a typo must not start the combined server")
	}
}

func TestServeWebNeedsBackendAndSecret(t *testing.T) {
	err := runServe(t, "web")
	if err == nil || !strings.Contains(err.Error(), "--backend-url") {
		t.Errorf("no backend URL: %v", err)
	}
	err = runServe(t, "web", "--backend-url", "http://api:8081")
	if err == nil || !strings.Contains(err.Error(), "service-secret") {
		t.Errorf("no secret: %v", err)
	}
	p := writeSecretFile(t, "short\n")
	err = runServe(t, "web", "--backend-url", "http://api:8081", "--service-secret-file", p)
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Errorf("short secret: %v", err)
	}
}

func TestServeAPINeedsAListenerAndASecret(t *testing.T) {
	if err := runServe(t, "api", "--listen", "", "--internal-listen", ""); err == nil {
		t.Error("an api process with no listener was accepted")
	}
	err := runServe(t, "api", "--internal-listen", ":0")
	if err == nil || !strings.Contains(err.Error(), "service-secret") {
		t.Errorf("internal listener without a secret: %v", err)
	}
}

func TestPrivateHost(t *testing.T) {
	for h, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "10.1.2.3": true, "192.168.0.9": true, "api": true,
		"8.8.8.8": false, "203.0.113.5": false} {
		if got := privateHost(h); got != want {
			t.Errorf("privateHost(%q) = %v", h, got)
		}
	}
}
