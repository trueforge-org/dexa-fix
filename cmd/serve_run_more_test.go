package cmd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestServeWorkerStopsWhenTheBackendRefusesItsSecret(t *testing.T) {
	backend, _, _ := testBackend(t)
	secret := writeSecretFile(t, strings.Repeat("x", 40)+"\n")
	_, _, err := run(t, "serve", "worker", "--backend-url", backend.URL, "--worker-secret-file", secret)
	if err == nil || !strings.Contains(err.Error(), "refused the worker secret") {
		t.Fatalf("a worker with the wrong secret must say so and stop: %v", err)
	}
}

func TestServeAPIKeepsItsStateInValkeyWhenOneIsGiven(t *testing.T) {
	logs := captureLog(t)
	mr := miniredis.RunT(t)
	service := writeSecretFile(t, strings.Repeat("s", 40)+"\n")
	health := freeAddr(t)
	args := append([]string{"serve", "api", "--listen", freeAddr(t), "--internal-listen", freeAddr(t), "--service-secret-file", service,
		"--health-listen", health, "--valkey-url", "redis://" + mr.Addr() + "/0"}, sqliteArgs(t)...)
	if err := runUntilServing(t, "http://"+health+"/readyz", args...); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "kept in Valkey") {
		t.Errorf("the log does not say that Valkey holds the state:\n%s", logs.String())
	}
}

func TestServeBodyOverMutualTLS(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mtls-body")
	if _, _, err := run(t, "secrets", "mtls", "--link", "body", "--name", "127.0.0.1", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	secret := writeSecretFile(t, strings.Repeat("b", 40)+"\n")
	health := freeAddr(t)
	args := append([]string{"serve", "body", "--listen", freeAddr(t), "--body-secret-file", secret,
		"--body-token-public-key-file", bodyPublicKeyFile(t), "--health-listen", health,
		"--body-tls-cert-file", filepath.Join(dir, "body.pem"), "--body-tls-key-file", filepath.Join(dir, "body.key"),
		"--body-tls-client-ca-file", filepath.Join(dir, "ca.pem")}, sqliteArgs(t)...)
	logs := captureLog(t)
	if err := runUntilServing(t, "http://"+health+"/readyz", args...); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"mutual_tls":true`) {
		t.Errorf("the log does not say that the service runs over mutual TLS:\n%s", logs.String())
	}
}

func TestServeCombinedStopsWithAnErrorWhenAConnectionKeepsTheShutdownWaiting(t *testing.T) {
	addr := freeAddr(t)
	args := append([]string{"serve", "--listen", addr}, sqliteArgs(t)...)
	stop, done := serveInBackground(t, "http://"+addr+"/healthz", args...)
	// A request whose body never arrives: the server is still working on it when it is told to stop.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write([]byte("POST /login HTTP/1.1\r\nHost: x\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\nname=")); err != nil {
		t.Fatal(err)
	}
	// An answer on another connection shows that the server is past accepting the first one.
	if code, _ := getStatus(t, "http://"+addr+"/healthz"); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	stop()
	if err := waitEnded(t, done); err == nil {
		t.Fatal("a shutdown that had to give up on a connection was reported as clean")
	}
}

func TestServeBodyReadsAPreviousKeyFileThatIsDamaged(t *testing.T) {
	prev := writeSecretFile(t, "not a key\n")
	secret := writeSecretFile(t, strings.Repeat("b", 40)+"\n")
	args := append([]string{"serve", "body", "--listen", freeAddr(t), "--body-secret-file", secret,
		"--body-token-public-key-file", bodyPublicKeyFile(t), "--encryption-key-previous-file", prev}, sqliteArgs(t)...)
	if _, _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), prev) {
		t.Fatalf("a damaged file of previous keys must be named: %v", err)
	}
}

func TestEnvOrAndEnvIntTakeTheEnvironmentWhenItIsSet(t *testing.T) {
	t.Setenv("DEXAFIX_TEST_TEXT", "from the environment")
	if got := envOr("DEXAFIX_TEST_TEXT", "default"); got != "from the environment" {
		t.Errorf("envOr = %q", got)
	}
	if got := envOr("DEXAFIX_TEST_UNSET", "default"); got != "default" {
		t.Errorf("envOr unset = %q", got)
	}
	t.Setenv("DEXAFIX_TEST_NUMBER", "7")
	if got := envInt("DEXAFIX_TEST_NUMBER", 2); got != 7 {
		t.Errorf("envInt = %d", got)
	}
	t.Setenv("DEXAFIX_TEST_NUMBER", "seven")
	if got := envInt("DEXAFIX_TEST_NUMBER", 2); got != 2 {
		t.Errorf("envInt of text = %d", got)
	}
}

func TestSameDirStillComparesWhenTheWorkingFolderIsGone(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if !sameDir("a.db", "a.key") {
		t.Error("two relative paths in the same folder were told apart")
	}
}

func TestASecretFileOthersCanReadIsReportedButStillUsed(t *testing.T) {
	logs := captureLog(t)
	path := writeSecretFile(t, strings.Repeat("a", 40)+"\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadServiceSecrets(path)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if !strings.Contains(logs.String(), "can be read by other users") {
		t.Errorf("no warning about the file mode:\n%s", logs.String())
	}
}

func TestSigningKeyOthersCanReadIsReportedButStillUsed(t *testing.T) {
	logs := captureLog(t)
	path := writeSigningKey(t)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSigningKey(path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "can be read by other users") {
		t.Errorf("no warning about the file mode:\n%s", logs.String())
	}
}

func TestBodyClientRulesForTheLinkItself(t *testing.T) {
	logs := captureLog(t)
	secret := writeSecretFile(t, strings.Repeat("b", 40)+"\n")
	key := writeSigningKey(t)
	missing := filepath.Join(t.TempDir(), "missing")

	// A plain link to an address that is not private still works, with a warning.
	c, _, remote, err := bodyClient(bodyFlags(t, "body-url", "http://203.0.113.5:8082", "body-secret-file", secret, "body-token-key-file", key))
	if err != nil || !remote || c == nil {
		t.Fatalf("plain link: %v %v %v", c, remote, err)
	}
	if !strings.Contains(logs.String(), "not private") {
		t.Errorf("no warning about the plain link:\n%s", logs.String())
	}

	_, _, _, err = bodyClient(bodyFlags(t, "body-url", "https://body:8082", "body-secret-file", secret, "body-token-key-file", key,
		"body-ca-file", missing, "body-client-cert-file", missing, "body-client-key-file", missing))
	if err == nil || !strings.Contains(err.Error(), "mutual TLS to the body service") {
		t.Errorf("certificates that cannot be read: %v", err)
	}
	if _, _, _, err = bodyClient(bodyFlags(t, "body-url", "http://body:8082", "body-secret-file", secret, "body-token-key-file", missing)); err == nil {
		t.Error("a signing key file that does not exist was accepted")
	}
}

func TestSecretsMTLSStopsCleanlyWhenItCannotWrite(t *testing.T) {
	t.Run("a body link needs a name too", func(t *testing.T) {
		_, _, err := run(t, "secrets", "mtls", "--link", "body", "--dir", filepath.Join(t.TempDir(), "x"))
		if err == nil || !strings.Contains(err.Error(), "body service") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a folder that cannot be made", func(t *testing.T) {
		file := writeSecretFile(t, "in the way\n")
		_, _, err := run(t, "secrets", "mtls", "--name", "api", "--dir", filepath.Join(file, "inside"))
		if err == nil {
			t.Fatal("a folder below a file was made")
		}
	})
	t.Run("a file that cannot be written", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join(dir, "nowhere", "ca.pem"), filepath.Join(dir, "ca.pem")); err != nil {
			t.Fatal(err)
		}
		_, _, err := run(t, "secrets", "mtls", "--name", "api", "--dir", dir)
		if err == nil {
			t.Fatal("a certificate was written through a link that leads nowhere")
		}
	})
}

func TestTheWorkerSecretCommandPrintsAUsableSecret(t *testing.T) {
	out, _, err := run(t, "secrets", "worker-secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := loadWorkerSecrets(writeSecretFile(t, out))
	if err != nil || len(got) != 1 {
		t.Fatalf("the secret does not load: %v %v", got, err)
	}
}

func TestHealthcheckRefusesAnAddressItCannotRequest(t *testing.T) {
	_, _, err := run(t, "healthcheck", "http://a\x7f/readyz")
	if err == nil || !strings.Contains(err.Error(), "healthcheck") {
		t.Fatalf("%v", err)
	}
}

func TestDSNThatCannotBeParsedIsTreatedAsHoldingAPassword(t *testing.T) {
	if !dsnHasPassword("postgres://%zz@host/db?password=x") {
		t.Error("a connection string that cannot be parsed but says password= was let through")
	}
	if dsnHasPassword("postgres://%zz@host/db") {
		t.Error("a connection string that cannot be parsed and has no password was refused")
	}
}

func TestInfoPrintsTheVersionOfTheBinary(t *testing.T) {
	logs := captureLog(t)
	if _, _, err := run(t, "info"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "Version") {
		t.Errorf("info said nothing about the version:\n%s", logs.String())
	}
}
