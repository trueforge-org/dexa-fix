package app

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/store"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

func openStore(t *testing.T) *sqlstore.Store {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// withoutBody is a store that does not hold the scans and readings: only the main interface shows.
type withoutBody struct{ store.Store }

func TestCombinedServerNeedsAStoreThatHoldsTheScansAndReadingsToo(t *testing.T) {
	_, err := New(withoutBody{openStore(t)})
	if err == nil || !strings.Contains(err.Error(), "scans and readings") {
		t.Fatalf("%v", err)
	}
}

// lateFailure is a store whose settings generation can be read for a number of calls and then fails,
// as a database that goes away while the server starts does.
type lateFailure struct {
	*sqlstore.Store
	okCalls atomic.Int64
	broken  atomic.Bool
}

var errGone = errors.New("the database went away")

func (s *lateFailure) SettingsGeneration(ctx context.Context) (int64, error) {
	if s.broken.Load() || s.okCalls.Add(-1) < 0 {
		return 0, errGone
	}
	return s.Store.SettingsGeneration(ctx)
}

func TestCombinedServerStopsWhenTheDatabaseFailsWhileItStarts(t *testing.T) {
	t.Run("before the backend has its settings", func(t *testing.T) {
		st := &lateFailure{Store: openStore(t)}
		if _, err := New(st); !errors.Is(err, errGone) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("before the frontend has its settings", func(t *testing.T) {
		st := &lateFailure{Store: openStore(t)}
		st.okCalls.Store(1) // the backend reads them once, the frontend asks the backend for them next
		if _, err := New(st); err == nil {
			t.Fatal("a frontend that could not get its settings was started")
		}
	})
}

func TestCombinedServerLogsWhenAChangedSettingCannotReachTheFrontend(t *testing.T) {
	var logs bytes.Buffer
	old := log.Logger
	log.Logger = zerolog.New(&logs)
	t.Cleanup(func() { log.Logger = old })

	st := &lateFailure{Store: openStore(t)}
	st.okCalls.Store(2) // the backend, then the frontend's first ask
	srv, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	st.broken.Store(true)
	srv.SetBaseURL(nil) // the Admin page changed a setting: the frontend is told, and cannot ask
	if !strings.Contains(logs.String(), "could not pass the changed settings to the frontend") {
		t.Errorf("the failure was not logged:\n%s", logs.String())
	}
}

func TestWrapListenerKeepsServingTheListener(t *testing.T) {
	srv, _ := newApp(t)
	t.Cleanup(srv.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := srv.WrapListener(ln)
	t.Cleanup(func() { wrapped.Close() })
	if wrapped.Addr().String() != ln.Addr().String() {
		t.Errorf("the wrapped listener answers at %s, not %s", wrapped.Addr(), ln.Addr())
	}
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	accepted, err := wrapped.Accept()
	if err != nil {
		t.Fatalf("a connection was not accepted through the wrapper: %v", err)
	}
	accepted.Close()
}
