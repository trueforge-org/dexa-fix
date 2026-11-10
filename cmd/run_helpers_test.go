package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	bodybackend "github.com/trueforge-org/dexafix/pkg/backend/body"
	mainbackend "github.com/trueforge-org/dexafix/pkg/backend/main"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

// restoreFlag puts one flag (a *pflag.Flag, which this package does not import directly) back to
// its default value and marks it as not given on the command line.
func restoreFlag[F any](f F) {
	v := reflect.ValueOf(f).Elem()
	value := v.FieldByName("Value").Interface()
	if sv, ok := value.(interface{ Replace([]string) error }); ok {
		sv.Replace(nil)
	} else {
		value.(interface{ Set(string) error }).Set(v.FieldByName("DefValue").String())
	}
	v.FieldByName("Changed").SetBool(false)
}

func visitFlags[F any](visit func(func(F)), fn func(F)) { visit(fn) }

// resetAllFlags puts every flag of every command back to its default: cobra keeps flag values
// between runs, and a run must not see what the one before it was given.
func resetAllFlags() {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		visitFlags(c.Flags().VisitAll, restoreFlag)
		c.SetOut(nil) // a test that gave a command its own writers must not keep them
		c.SetErr(nil)
		c.SetContext(nil)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(RootCmd)
	RootCmd.SetArgs(nil)
	RootCmd.SetOut(nil)
	RootCmd.SetErr(nil)
}

// resetFlags resets the flags now and again when the test ends.
func resetFlags(t *testing.T) {
	t.Helper()
	resetAllFlags()
	t.Cleanup(resetAllFlags)
}

// execute runs the real command line with args and returns what it printed to stdout and stderr.
func execute(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	resetFlags(t)
	var out, errOut bytes.Buffer
	RootCmd.SetOut(&out)
	RootCmd.SetErr(&errOut)
	RootCmd.SetArgs(args)
	// cobra hands a command the context of the first run that reached it and keeps it, so a
	// cancelled context would stay with the command: give every command this run's.
	var give func(c *cobra.Command)
	give = func(c *cobra.Command) {
		c.SetContext(ctx)
		for _, sub := range c.Commands() {
			give(sub)
		}
	}
	give(RootCmd)
	err = RootCmd.ExecuteContext(ctx)
	return out.String(), errOut.String(), err
}

// run is execute for a command that returns by itself.
func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return execute(t, context.Background(), args...)
}

// freeAddr returns a loopback address nobody listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// takeAddr holds an address, so that a process asked to listen there fails.
func takeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLog collects what the program logs until the test ends.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := new(lockedBuffer)
	old := log.Logger
	log.Logger = zerolog.New(buf)
	t.Cleanup(func() { log.Logger = old })
	return buf
}

// serveInBackground starts a serving command and waits until url answers 200. It returns a function
// that ends the command as a signal would, and the channel that receives how the command ended.
func serveInBackground(t *testing.T, url string, args ...string) (stop context.CancelFunc, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ended := make(chan error, 1)
	go func() {
		_, _, err := execute(t, ctx, args...)
		ended <- err
	}()
	deadline := time.Now().Add(20 * time.Second)
	for served := false; !served; {
		select {
		case err := <-ended:
			t.Fatalf("%v stopped before it served: %v", args[:2], err)
		default:
		}
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			served = resp.StatusCode == http.StatusOK
		}
		if !served {
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("%v never answered at %s", args[:2], url)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return cancel, ended
}

// getStatus returns the status code and the body of a GET.
func getStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// waitEnded returns how a command ended, or fails the test when it does not end.
func waitEnded(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the command did not stop")
		return nil
	}
}

// runUntilServing runs a serving command until url answers 200, then cancels it, as a signal would,
// and returns how the command ended.
func runUntilServing(t *testing.T, url string, args ...string) error {
	t.Helper()
	stop, done := serveInBackground(t, url, args...)
	stop()
	return waitEnded(t, done)
}

// databaseWithoutSettings makes a migrated database whose settings table is gone, so that a backend
// started on it cannot read its settings. It returns the database and key flags.
func databaseWithoutSettings(t *testing.T) []string {
	t.Helper()
	args := sqliteArgs(t)
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, args[3])
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ExecForTests(context.Background(), "DROP TABLE app_settings"); err != nil {
		t.Fatal(err)
	}
	return args
}

// cancelledContext is the context of a process that was told to stop before it began.
func cancelledContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx, cancel
}

// bodyPublicKeyFile makes a signing key with the real command and returns a file with its public key.
func bodyPublicKeyFile(t *testing.T) string {
	t.Helper()
	out, _, err := run(t, "secrets", "body-token-public", "--key-file", writeSigningKey(t))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "body-token.pub")
	if err := os.WriteFile(p, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// sqliteArgs are the database and key flags for a database in a new folder.
func sqliteArgs(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	return []string{"--db-driver", "sqlite", "--dsn", "file:" + filepath.Join(dir, "d.db"), "--encryption-key-file", filepath.Join(dir, "k.key")}
}

// databaseWithDamagedSettings makes a database whose stored base URL cannot be opened with its key,
// so that a backend can open the database but not read its settings. It returns the flags.
func databaseWithDamagedSettings(t *testing.T) []string {
	t.Helper()
	d := newRecoveryDB(t)
	ctx := context.Background()
	name := mainbackend.NetworkSettingNames[0]
	if err := d.st.PutSetting(ctx, name, "https://dexafix.example"); err != nil {
		t.Fatal(err)
	}
	if err := d.st.ExecForTests(ctx, "UPDATE app_settings SET value = 'enc:damaged' WHERE name = '"+name+"'"); err != nil {
		t.Fatal(err)
	}
	d.st.Close()
	return d.flags
}

// testBackend serves the internal API of a backend with a service and a worker secret.
func testBackend(t *testing.T) (ts *httptest.Server, serviceSecret, workerSecret string) {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bodyClt, _, err := bodybackend.NewInProcess(st)
	if err != nil {
		t.Fatal(err)
	}
	b, err := mainbackend.NewBackend(st, mainbackend.WithBody(bodyClt))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	serviceSecret, workerSecret = strings.Repeat("s", 40), strings.Repeat("w", 40)
	b.SetServiceSecrets(serviceSecret)
	b.SetWorkerSecrets(workerSecret)
	ts = httptest.NewServer(b.InternalHandler())
	t.Cleanup(ts.Close)
	return ts, serviceSecret, workerSecret
}
