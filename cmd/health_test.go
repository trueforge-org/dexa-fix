package cmd

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestHealthHandler(t *testing.T) {
	var ready error
	srv := httptest.NewServer(healthHandler(func(context.Context) error { return ready }))
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, b := get("/healthz"); code != 200 || b != "ok" {
		t.Errorf("/healthz: %d %q", code, b)
	}
	if code, _ := get("/readyz"); code != 200 {
		t.Errorf("/readyz when ready: %d", code)
	}
	ready = errors.New("dial tcp db.internal.example.com:5432: connection refused")
	code, b := get("/readyz")
	if code != 503 {
		t.Errorf("/readyz when not ready: %d", code)
	}
	if strings.Contains(b, "db.internal") || strings.Contains(b, "refused") {
		t.Errorf("the reason leaked into the answer: %q", b)
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Errorf("/healthz must not depend on readiness: %d", code)
	}
	if code, _ := get("/anything-else"); code != 404 {
		t.Errorf("the health listener answers nothing else: %d", code)
	}
	if code, _ := get("/session"); code != 404 {
		t.Errorf("the health listener answers nothing else: %d", code)
	}
}

func TestHealthcheckCommand(t *testing.T) {
	healthcheckCmd.SetContext(context.Background())
	t.Cleanup(func() { healthcheckCmd.SetContext(nil) })
	run := func(url string) error {
		healthcheckCmd.Flags().Set("timeout", "2s")
		return healthcheckCmd.RunE(healthcheckCmd, []string{url})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not ready", 503) })
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", 302) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if err := run(srv.URL + "/ok"); err != nil {
		t.Errorf("healthy: %v", err)
	}
	for _, path := range []string{"/down", "/moved", "/missing"} {
		if err := run(srv.URL + path); err == nil {
			t.Errorf("%s must be unhealthy", path)
		}
	}
	srv.Close()
	if err := run(srv.URL + "/ok"); err == nil {
		t.Error("an unreachable process is unhealthy")
	}
	if err := run("not a url"); err == nil {
		t.Error("a bad address is an error")
	}
}

func TestStartHealthServesOnlyWhenAsked(t *testing.T) {
	c := &cobra.Command{Use: "x"}
	addHealthFlag(c)
	errCh := make(chan error, 1)
	if srv, err := startHealth(c, nil, errCh); srv != nil || err != nil {
		t.Fatalf("no --health-listen must start nothing: %v %v", srv, err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	c.Flags().Set("health-listen", addr)
	srv, err := startHealth(c, func(context.Context) error { return nil }, errCh)
	if err != nil || srv == nil {
		t.Fatalf("start: %v %v", srv, err)
	}
	t.Cleanup(func() { shutdown(srv) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		healthcheckCmd.SetContext(context.Background())
		if err := healthcheckCmd.RunE(healthcheckCmd, []string{"http://" + addr + "/readyz"}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the health listener never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// An address that is taken is an error at start, not a silent no-op.
	c.Flags().Set("health-listen", addr)
	if _, err := startHealth(c, nil, errCh); err == nil {
		t.Error("a taken address must fail at start")
	}
}

func TestLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8083": true, "[::1]:8083": true, "localhost:8083": true,
		":8083": false, "0.0.0.0:8083": false, "[::]:8083": false, "10.0.0.5:8083": false, "nonsense": false,
	} {
		if got := loopbackAddr(addr); got != want {
			t.Errorf("loopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestPprofOnlyOnLoopbackHealthListener(t *testing.T) {
	c := &cobra.Command{Use: "x"}
	addHealthFlag(c)
	errCh := make(chan error, 1)

	c.Flags().Set("pprof", "true")
	if _, err := startHealth(c, nil, errCh); err == nil {
		t.Error("--pprof without --health-listen must fail")
	}
	c.Flags().Set("health-listen", "0.0.0.0:0")
	if _, err := startHealth(c, nil, errCh); err == nil {
		t.Error("--pprof on a non-loopback address must fail")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	c.Flags().Set("health-listen", addr)
	srv, err := startHealth(c, nil, errCh)
	if err != nil || srv == nil {
		t.Fatalf("start: %v %v", srv, err)
	}
	t.Cleanup(func() { shutdown(srv) })
	var resp *http.Response
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if resp, err = http.Get("http://" + addr + "/debug/pprof/cmdline"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the listener never answered")
		}
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/debug/pprof/cmdline = %d", resp.StatusCode)
	}

	// Without --pprof the same listener has no profiling pages.
	plain := httptest.NewServer(healthHandler(nil))
	defer plain.Close()
	r, err := http.Get(plain.URL + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("health handler alone served /debug/pprof/: %d", r.StatusCode)
	}
}
