//go:build linux

package cmd

import (
	"strings"
	"testing"
	"time"
)

// A listener the operating system takes away ends the process with an error, so that its supervisor
// restarts it, instead of leaving a process that serves nothing.

func TestServeCombinedStopsWithAnErrorWhenItsListenerDies(t *testing.T) {
	addr := freeAddr(t)
	_, done := serveInBackground(t, "http://"+addr+"/healthz", append([]string{"serve", "--listen", addr}, sqliteArgs(t)...)...)
	killListener(t, addr)
	if err := waitEnded(t, done); err == nil {
		t.Fatal("a server without its listener ended without an error")
	}
}

func TestServeWebStopsWithAnErrorWhenItsListenerDies(t *testing.T) {
	backend, service, _ := testBackend(t)
	addr := freeAddr(t)
	_, done := serveInBackground(t, "http://"+addr+"/healthz", "serve", "web", "--listen", addr, "--backend-url", backend.URL,
		"--service-secret-file", writeSecretFile(t, service+"\n"))
	killListener(t, addr)
	if err := waitEnded(t, done); err == nil {
		t.Fatal("a web process without its listener ended without an error")
	}
}

func TestServeAPIStopsWithAnErrorWhenAListenerDies(t *testing.T) {
	service := writeSecretFile(t, strings.Repeat("s", 40)+"\n")
	health := freeAddr(t)
	args := append([]string{"serve", "api", "--listen", freeAddr(t), "--internal-listen", freeAddr(t),
		"--service-secret-file", service, "--health-listen", health}, sqliteArgs(t)...)
	_, done := serveInBackground(t, "http://"+health+"/readyz", args...)
	killListener(t, health)
	if err := waitEnded(t, done); err == nil {
		t.Fatal("an api process without its health listener ended without an error")
	}
}

func TestServeBodyStopsWithAnErrorWhenItsListenerDies(t *testing.T) {
	secret := writeSecretFile(t, strings.Repeat("b", 40)+"\n")
	health, addr := freeAddr(t), freeAddr(t)
	args := append([]string{"serve", "body", "--listen", addr, "--body-secret-file", secret,
		"--body-token-public-key-file", bodyPublicKeyFile(t), "--health-listen", health}, sqliteArgs(t)...)
	_, done := serveInBackground(t, "http://"+health+"/readyz", args...)
	killListener(t, addr)
	if err := waitEnded(t, done); err == nil {
		t.Fatal("a body service without its listener ended without an error")
	}
}

func TestServeWorkerCarriesOnWhenItsHealthListenerDies(t *testing.T) {
	logs := captureLog(t)
	backend, _, worker := testBackend(t)
	health := freeAddr(t)
	stop, done := serveInBackground(t, "http://"+health+"/readyz", "serve", "worker", "--backend-url", backend.URL,
		"--worker-secret-file", writeSecretFile(t, worker+"\n"), "--health-listen", health)
	killListener(t, health)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.String(), "the health listener stopped") {
		select {
		case err := <-done:
			t.Fatalf("the worker stopped with its health listener: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the log never said that the health listener stopped:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	if err := waitEnded(t, done); err != nil {
		t.Fatal(err)
	}
}
