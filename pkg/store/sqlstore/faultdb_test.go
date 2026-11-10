package sqlstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	_ "modernc.org/sqlite"
)

// errInjected is what the faulty database driver answers in place of the statement it was told to fail.
var errInjected = errors.New("injected database failure")

// injector is a database/sql connector around in-memory SQLite that fails exactly one statement, the
// n-th one counted from the moment it is armed. A statement is an Exec, a Query, a Begin or a Commit.
// Running the same operation with n = 1, 2, 3, ... until no statement is left to fail makes every
// "the database said no" branch of that operation run, one after the other.
type injector struct {
	base driver.Driver
	dsn  string

	armed atomic.Bool
	left  atomic.Int64 // statements to let through before the failing one
	fired atomic.Bool
}

func (i *injector) Driver() driver.Driver { return i.base }

func (i *injector) Connect(context.Context) (driver.Conn, error) {
	c, err := i.base.Open(i.dsn)
	if err != nil {
		return nil, err
	}
	return &faultyConn{Conn: c, inj: i}, nil
}

// arm makes the n-th statement from now on (1 is the next one) fail.
func (i *injector) arm(n int) {
	i.fired.Store(false)
	i.left.Store(int64(n - 1))
	i.armed.Store(true)
}

// check returns errInjected when this statement is the one to fail.
func (i *injector) check() error {
	if !i.armed.Load() {
		return nil
	}
	if i.left.Add(-1) < 0 && i.armed.CompareAndSwap(true, false) {
		i.fired.Store(true)
		return errInjected
	}
	return nil
}

type faultyConn struct {
	driver.Conn
	inj *injector
}

func (c *faultyConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.inj.check(); err != nil {
		return nil, err
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}

func (c *faultyConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.inj.check(); err != nil {
		return nil, err
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

func (c *faultyConn) Ping(ctx context.Context) error {
	if err := c.inj.check(); err != nil {
		return err
	}
	return c.Conn.(driver.Pinger).Ping(ctx)
}

func (c *faultyConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
}

func (c *faultyConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	if err := c.inj.check(); err != nil {
		return nil, err
	}
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &faultyTx{Tx: tx, inj: c.inj}, nil
}

type faultyTx struct {
	driver.Tx
	inj *injector
}

func (t *faultyTx) Commit() error {
	if err := t.inj.check(); err != nil {
		t.Tx.Rollback()
		return err
	}
	return t.Tx.Commit()
}

var faultyStoreCount atomic.Int64

// newFaultyStore returns a migrated in-memory store (without encryption unless c is given) whose
// statements can be failed on purpose with the returned injector.
func newFaultyStore(t *testing.T, c *Cipher) (*Store, *injector) {
	t.Helper()
	dsn := sqliteDSN(fmt.Sprintf("file:faulty%d?mode=memory&cache=shared", faultyStoreCount.Add(1)))
	plain, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	inj := &injector{base: plain.Driver(), dsn: dsn}
	plain.Close()
	sqlDB := sql.OpenDB(inj)
	sqlDB.SetMaxOpenConns(1)
	db := bun.NewDB(sqlDB, sqlitedialect.New())
	s := &Store{db: db, driver: DriverSQLite, maxConns: 1, cipher: c}
	s.BodyStore = &BodyStore{db: db, driver: DriverSQLite, maxConns: 1}
	if c != nil {
		s.BodyStore.cipher = c.bodyCipher()
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, inj
}

// failEveryStatement runs op once for every statement it sends to the database, each time with that one
// statement failing, and requires that the failure comes back to the caller. seed prepares the data
// (it runs before anything is armed). absorbed lists the statements whose failure the operation is
// allowed to take in stride (the library underneath treats a failed "does the table exist" as "no"). It returns how many statements the operation sent.
func failEveryStatement[F any](t *testing.T, c *Cipher, seed func(*testing.T, *Store) F, op func(context.Context, *Store, F) error, absorbed ...int) int {
	t.Helper()
	ctx := context.Background()
	for n := 1; n <= 200; n++ {
		s, inj := newFaultyStore(t, c)
		fx := seed(t, s)
		inj.arm(n)
		err := op(ctx, s, fx)
		if !inj.fired.Load() {
			return n - 1
		}
		if !errors.Is(err, errInjected) && !slices.Contains(absorbed, n) {
			t.Errorf("statement %d failed but the operation answered %v", n, err)
		}
	}
	t.Fatal("the operation sends more than 200 statements")
	return 0
}
