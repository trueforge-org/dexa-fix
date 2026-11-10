package cmd

import (
	"bytes"
	"context"
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

// resetFlags puts every flag of every command back to its default when the test ends: cobra keeps
// flag values between runs, and the next test must not see these.
func resetFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		var walk func(c *cobra.Command)
		walk = func(c *cobra.Command) {
			visitFlags(c.Flags().VisitAll, restoreFlag)
			for _, sub := range c.Commands() {
				walk(sub)
			}
		}
		walk(RootCmd)
		RootCmd.SetArgs(nil)
		RootCmd.SetOut(nil)
		RootCmd.SetErr(nil)
	})
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

// runUntilServing runs a serving command until url answers 200, then cancels it, as a signal would,
// and returns how the command ended.
func runUntilServing(t *testing.T, url string, args ...string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := execute(t, ctx, args...)
		done <- err
	}()
	deadline := time.Now().Add(20 * time.Second)
	for served := false; !served; {
		select {
		case err := <-done:
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
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the command did not stop after its context ended")
		return nil
	}
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
