package cmd

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestServeRunsTheCombinedServerUntilItIsStopped(t *testing.T) {
	logs := captureLog(t)
	t.Setenv("DEXAFIX_WITHINGS_CLIENT_ID", "old")
	t.Setenv("DEXAFIX_BASE_URL", "http://old")
	addr := freeAddr(t)
	args := append([]string{"serve", "--listen", addr, "--force-password-login"}, sqliteArgs(t)...)
	if err := runUntilServing(t, "http://"+addr+"/healthz", args...); err != nil {
		t.Fatalf("a stopped server must end without an error: %v", err)
	}
	for _, want := range []string{"DEXAFIX_WITHINGS_CLIENT_ID", "DEXAFIX_BASE_URL", "shutting down", "stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not mention %q:\n%s", want, logs.String())
		}
	}
}

func TestServeStopsWithAnErrorWhenItCannotStart(t *testing.T) {
	missingFolder := filepath.Join(t.TempDir(), "nope", "k.key")
	cases := map[string]struct {
		args []string
		want string
	}{
		"a password in the connection string": {
			[]string{"--dsn", "postgres://u:hunter2hunter2@localhost/x"}, "contains a password"},
		"an encryption key file in a folder that does not exist": {
			[]string{"--dsn", "file:" + filepath.Join(t.TempDir(), "d.db"), "--encryption-key-file", missingFolder}, "create encryption key file"},
		"a database driver that does not exist": {
			append([]string{"--db-driver", "oracle"}, sqliteArgs(t)[2:]...), "oracle"},
		"a database that was not migrated and may not be": {
			append([]string{"--auto-migrate=false"}, sqliteArgs(t)...), "migrat"},
		"an address that is taken": {
			append([]string{"--listen", takeAddr(t)}, sqliteArgs(t)...), "address already in use"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := run(t, append([]string{"serve"}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestServeAPIRunsUntilItIsStopped(t *testing.T) {
	logs := captureLog(t)
	t.Setenv("DEXAFIX_TRUSTED_PROXIES", "10.0.0.0/8")
	service := writeSecretFile(t, strings.Repeat("s", 40)+"\n")
	worker := writeSecretFile(t, strings.Repeat("w", 40)+"\n")
	health := freeAddr(t)
	args := append([]string{"serve", "api", "--listen", freeAddr(t), "--internal-listen", freeAddr(t),
		"--service-secret-file", service, "--worker-secret-file", worker, "--health-listen", health,
		"--maintenance=false", "--force-password-login"}, sqliteArgs(t)...)
	if err := runUntilServing(t, "http://"+health+"/readyz", args...); err != nil {
		t.Fatalf("a stopped api process must end without an error: %v", err)
	}
	for _, want := range []string{"DEXAFIX_TRUSTED_PROXIES", "serving the backend", "stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not mention %q:\n%s", want, logs.String())
		}
	}
}

func TestServeAPIWithABodyServiceOfItsOwnRunsOnlyTheMainDatabase(t *testing.T) {
	service := writeSecretFile(t, strings.Repeat("s", 40)+"\n")
	bodySecret := writeSecretFile(t, strings.Repeat("b", 40)+"\n")
	health := freeAddr(t)
	args := append([]string{"serve", "api", "--listen", "", "--internal-listen", freeAddr(t), "--service-secret-file", service,
		"--health-listen", health, "--body-url", "http://127.0.0.1:1", "--body-secret-file", bodySecret,
		"--body-token-key-file", writeSigningKey(t), "--without-body"}, sqliteArgs(t)...)
	if err := runUntilServing(t, "http://"+health+"/readyz", args...); err != nil {
		t.Fatal(err)
	}
}

func TestServeAPIStopsWithAnErrorWhenItCannotStart(t *testing.T) {
	service := writeSecretFile(t, strings.Repeat("s", 40)+"\n")
	missing := filepath.Join(t.TempDir(), "missing")
	mtlsDir := makeSet(t)
	cases := map[string]struct {
		args []string
		want string
	}{
		"internal TLS files without an internal listener": {
			[]string{"--internal-listen", "", "--internal-tls-cert-file", filepath.Join(mtlsDir, "api.pem"), "--internal-tls-key-file", filepath.Join(mtlsDir, "api.key"),
				"--internal-tls-client-ca-file", filepath.Join(mtlsDir, "ca.pem")},
			"--internal-listen is empty"},
		"a worker secret file that does not exist": {
			[]string{"--service-secret-file", service, "--worker-secret-file", missing}, "worker secret"},
		"the same secret for the service and the workers": {
			[]string{"--service-secret-file", service, "--worker-secret-file", service}, "service"},
		"a body url without its secret": {
			[]string{"--service-secret-file", service, "--body-url", "http://127.0.0.1:1"}, "body-secret-file"},
		"a password in the connection string": {
			[]string{"--service-secret-file", service, "--dsn", "postgres://u:hunter2hunter2@localhost/x"}, "contains a password"},
		"an encryption key file in a folder that does not exist": {
			[]string{"--service-secret-file", service, "--dsn", "file:" + filepath.Join(t.TempDir(), "d.db"), "--encryption-key-file", filepath.Join(missing, "k.key")}, "create encryption key file"},
		"a database that was not migrated and may not be": {
			append([]string{"--service-secret-file", service, "--auto-migrate=false"}, sqliteArgs(t)...), "migrat"},
		"a valkey address that does not work": {
			append([]string{"--service-secret-file", service, "--valkey-url", "redis://127.0.0.1:1/0"}, sqliteArgs(t)...), "127.0.0.1:1"},
		"a valkey url with a password on the command line": {
			append([]string{"--service-secret-file", service, "--valkey-url", "redis://:pw12345@127.0.0.1:1/0"}, sqliteArgs(t)...), "contains a password"},
		"a public address that is taken": {
			append([]string{"--service-secret-file", service, "--listen", takeAddr(t), "--internal-listen", freeAddr(t)}, sqliteArgs(t)...), "address already in use"},
		"an internal address that is taken": {
			append([]string{"--service-secret-file", service, "--listen", freeAddr(t), "--internal-listen", takeAddr(t)}, sqliteArgs(t)...), "address already in use"},
		"a health address that is taken": {
			append([]string{"--service-secret-file", service, "--listen", freeAddr(t), "--internal-listen", freeAddr(t), "--health-listen", takeAddr(t)}, sqliteArgs(t)...), "health listener"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := run(t, append([]string{"serve", "api"}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestServeWebRunsUntilItIsStopped(t *testing.T) {
	logs := captureLog(t)
	t.Setenv("DEXAFIX_DSN_FILE", "/nowhere")
	backend, service, _ := testBackend(t)
	secret := writeSecretFile(t, service+"\n")
	addr := freeAddr(t)
	err := runUntilServing(t, "http://"+addr+"/healthz", "serve", "web", "--listen", addr, "--backend-url", backend.URL,
		"--service-secret-file", secret, "--api-upstream", "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("a stopped web process must end without an error: %v", err)
	}
	for _, want := range []string{"DEXAFIX_DSN_FILE", "serving the web frontend", "stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not mention %q:\n%s", want, logs.String())
		}
	}
}

func TestServeWebStopsWithAnErrorWhenItCannotStart(t *testing.T) {
	backend, service, worker := testBackend(t)
	secret := writeSecretFile(t, service+"\n")
	cases := map[string]struct {
		args []string
		want string
	}{
		"a backend address that is not an address": {
			[]string{"--backend-url", "api", "--service-secret-file", secret}, "--backend-url"},
		"an https backend without mutual TLS": {
			[]string{"--backend-url", "https://api:8081", "--service-secret-file", secret}, "mutual TLS"},
		"an upstream that is not an address": {
			[]string{"--backend-url", backend.URL, "--service-secret-file", secret, "--api-upstream", "api:8080"}, "--api-upstream"},
		"the secret of the workers instead of the service secret": {
			[]string{"--backend-url", backend.URL, "--service-secret-file", writeSecretFile(t, worker+"\n")}, "refused the service secret"},
		"an address that is taken": {
			[]string{"--backend-url", backend.URL, "--service-secret-file", secret, "--listen", takeAddr(t)}, "address already in use"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := run(t, append([]string{"serve", "web"}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestServeWebWarnsAboutAPlainBackendOnAPublicAddressAndGivesUpWhenStopped(t *testing.T) {
	logs := captureLog(t)
	secret := writeSecretFile(t, strings.Repeat("s", 40)+"\n")
	ctx, cancel := cancelledContext()
	defer cancel()
	_, _, err := execute(t, ctx, "serve", "web", "--backend-url", "http://203.0.113.5:8081", "--service-secret-file", secret)
	if err == nil {
		t.Fatal("a web process that was stopped while it waited for its backend must say so")
	}
	if !strings.Contains(logs.String(), "not private") {
		t.Errorf("no warning about the plain link:\n%s", logs.String())
	}
}

func TestServeWorkerRunsUntilItIsStopped(t *testing.T) {
	logs := captureLog(t)
	t.Setenv("DEXAFIX_ENCRYPTION_KEY", "hex")
	backend, _, worker := testBackend(t)
	secret := writeSecretFile(t, worker+"\n")
	health := freeAddr(t)
	err := runUntilServing(t, "http://"+health+"/healthz", "serve", "worker", "--backend-url", backend.URL,
		"--worker-secret-file", secret, "--providers", "withings", "--worker-id", "test-worker", "--health-listen", health)
	if err != nil {
		t.Fatalf("a stopped worker must end without an error: %v", err)
	}
	for _, want := range []string{"DEXAFIX_ENCRYPTION_KEY", "running the worker", "test-worker", "stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not mention %q:\n%s", want, logs.String())
		}
	}
}

func TestServeWorkerStopsWithAnErrorWhenItCannotStart(t *testing.T) {
	backend, _, worker := testBackend(t)
	secret := writeSecretFile(t, worker+"\n")
	cases := map[string]struct {
		args []string
		want string
	}{
		"a backend address that is not an address": {
			[]string{"--backend-url", "api", "--worker-secret-file", secret}, "--backend-url"},
		"a provider that does not exist": {
			[]string{"--backend-url", backend.URL, "--worker-secret-file", secret, "--providers", "nosuch"}, "nosuch"},
		"a pprof listener that is not on loopback": {
			[]string{"--backend-url", backend.URL, "--worker-secret-file", secret, "--pprof"}, "--pprof"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := cancelledContext()
			defer cancel()
			_, _, err := execute(t, ctx, append([]string{"serve", "worker"}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestServeWorkerWarnsAboutAPlainBackendOnAPublicAddress(t *testing.T) {
	logs := captureLog(t)
	secret := writeSecretFile(t, strings.Repeat("w", 40)+"\n")
	ctx, cancel := cancelledContext()
	defer cancel()
	execute(t, ctx, "serve", "worker", "--backend-url", "http://203.0.113.5:8081", "--worker-secret-file", secret)
	if !strings.Contains(logs.String(), "not private") {
		t.Errorf("no warning about the plain link:\n%s", logs.String())
	}
}

func TestServeBodyRunsUntilItIsStopped(t *testing.T) {
	logs := captureLog(t)
	secret := writeSecretFile(t, strings.Repeat("b", 40)+"\n")
	pub := bodyPublicKeyFile(t)
	health := freeAddr(t)
	args := append([]string{"serve", "body", "--listen", freeAddr(t), "--body-secret-file", secret,
		"--body-token-public-key-file", pub, "--health-listen", health}, sqliteArgs(t)...)
	if err := runUntilServing(t, "http://"+health+"/readyz", args...); err != nil {
		t.Fatalf("a stopped body service must end without an error: %v", err)
	}
	for _, want := range []string{"serving the body service", "stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not mention %q:\n%s", want, logs.String())
		}
	}
}

func TestServeBodyStopsWithAnErrorWhenItCannotStart(t *testing.T) {
	secret := writeSecretFile(t, strings.Repeat("b", 40)+"\n")
	pub := bodyPublicKeyFile(t)
	base := []string{"--body-secret-file", secret, "--body-token-public-key-file", pub}
	cases := map[string]struct {
		args []string
		want string
	}{
		"a public key file that does not exist": {
			[]string{"--body-secret-file", secret, "--body-token-public-key-file", filepath.Join(t.TempDir(), "none")}, "public key"},
		"mutual TLS with one file of three": {
			append([]string{"--body-tls-cert-file", "c.pem"}, base...), "mutual TLS"},
		"a password in the connection string": {
			append([]string{"--dsn", "postgres://u:hunter2hunter2@localhost/x"}, base...), "contains a password"},
		"an encryption key file in a folder that does not exist": {
			append([]string{"--dsn", "file:" + filepath.Join(t.TempDir(), "d.db"), "--encryption-key-file", filepath.Join(t.TempDir(), "no", "k.key")}, base...), "create encryption key file"},
		"a database that was not migrated and may not be": {
			append(append([]string{"--auto-migrate=false"}, sqliteArgs(t)...), base...), "migrat"},
		"an address that is taken": {
			append(append([]string{"--listen", takeAddr(t)}, sqliteArgs(t)...), base...), "address already in use"},
		"a health address that is taken": {
			append(append([]string{"--listen", freeAddr(t), "--health-listen", takeAddr(t)}, sqliteArgs(t)...), base...), "health listener"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := run(t, append([]string{"serve", "body", "--listen", freeAddr(t)}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestServeBodyDoesNotCreateAKeyForADatabaseThatHoldsEncryptedValues(t *testing.T) {
	// Covered where the data is made; here the key file is simply unreadable (a folder).
	secret := writeSecretFile(t, strings.Repeat("b", 40)+"\n")
	dir := t.TempDir()
	args := []string{"serve", "body", "--listen", freeAddr(t), "--body-secret-file", secret, "--body-token-public-key-file", bodyPublicKeyFile(t),
		"--dsn", "file:" + filepath.Join(dir, "d.db"), "--encryption-key-file", dir}
	if _, _, err := run(t, args...); err == nil {
		t.Fatal("a key file that is a folder was accepted")
	}
}

func TestListenAndServeReportsAnAddressThatIsTaken(t *testing.T) {
	srv := newHTTPServer(takeAddr(t), http.NotFoundHandler())
	if err := listenAndServe(srv, nil, make(chan error, 1)); err == nil {
		t.Fatal("a taken address was accepted")
	}
}

// failingCloseListener is a listener whose Close fails, so that shutting its server down fails.
type failingCloseListener struct {
	net.Listener
	accepting chan struct{}
	once      *sync.Once
}

func (l failingCloseListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.accepting) })
	return l.Listener.Accept()
}

func (l failingCloseListener) Close() error {
	l.Listener.Close()
	return os.ErrClosed
}

func TestShutdownSkipsMissingServersAndReportsTheFirstFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := newHTTPServer("", http.NotFoundHandler())
	fl := failingCloseListener{ln, make(chan struct{}), new(sync.Once)}
	go srv.Serve(fl)
	<-fl.accepting
	if err := shutdown(nil, srv); err == nil {
		t.Fatal("a server that could not be shut down was reported as stopped")
	}
}
