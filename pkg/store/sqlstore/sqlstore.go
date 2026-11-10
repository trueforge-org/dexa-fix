// Package sqlstore implements store.Store on top of bun, for SQLite and PostgreSQL.
// Schema changes are versioned goose migrations (see migrate.go).
package sqlstore

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/schema"
	_ "modernc.org/sqlite" // pure-Go sqlite driver

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// Supported database drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// Store is a bun backed store.Store.
type Store struct {
	db       *bun.DB
	driver   string
	maxConns int     // size of the connection pool
	cipher   *Cipher // encrypts sensitive columns; nil leaves new values unencrypted

	// BodyStore is the body domain (scans and readings) on the same connection pool, or nil when
	// the store was opened WithoutBody: then another process holds that data. Its methods are
	// promoted, so a combined process and the tests use one value for both domains; the main
	// backend only sees the store.Store interface, which does not have them.
	*BodyStore
}

var _ store.Store = (*Store)(nil)
var _ store.BodyStore = (*Store)(nil)

// MaxConns is the size of the connection pool. A server uses it to keep one class of requests from
// taking every connection.
func (s *Store) MaxConns() int { return s.maxConns }

type migrateMode int

const (
	migrateUp      migrateMode = iota // apply pending migrations (default)
	migrateSkip                       // leave the schema alone
	migrateRequire                    // fail if migrations are pending
)

type openConfig struct {
	migrate     migrateMode
	cipher      *Cipher
	withoutBody bool
}

// Option customises Open.
type Option func(*openConfig)

// WithoutMigrate opens the database without touching the schema (used by the db management commands).
func WithoutMigrate() Option { return func(c *openConfig) { c.migrate = migrateSkip } }

// WithoutBody opens the main database without the body domain (scans and readings): its tables are
// not created or migrated, and the BodyStore methods are not available. For a backend that calls a
// body service of its own.
func WithoutBody() Option { return func(c *openConfig) { c.withoutBody = true } }

// RequireMigrated fails Open when there are pending migrations instead of applying them.
func RequireMigrated() Option { return func(c *openConfig) { c.migrate = migrateRequire } }

// connect opens a pool for the driver and checks that the database answers. It returns the pool's
// size, which a server uses to keep one class of requests from taking every connection.
func connect(ctx context.Context, driver, dsn string) (db *bun.DB, maxConns int, err error) {
	var (
		sqlDB   *sql.DB
		dialect schema.Dialect
	)
	switch driver {
	case DriverSQLite:
		secureSQLiteFile(dsn)
		sqlDB, err = sql.Open("sqlite", sqliteDSN(dsn))
		dialect = sqlitedialect.New()
	case DriverPostgres:
		sqlDB, err = sql.Open("pgx", dsn)
		dialect = pgdialect.New()
	default:
		return nil, 0, fmt.Errorf("unsupported db driver %q (use %q or %q)", driver, DriverSQLite, DriverPostgres)
	}
	if err != nil {
		return nil, 0, err
	}
	switch {
	case driver == DriverSQLite && sqliteInMemory(dsn):
		maxConns = 1
		sqlDB.SetMaxOpenConns(1) // every connection to an in-memory database would be a separate database
	case driver == DriverSQLite:
		maxConns = sqliteMaxConns
		// WAL lets readers run next to the single writer, so a long import no longer blocks page
		// loads. Writers queue on busy_timeout; transactions start with BEGIN IMMEDIATE (see
		// sqliteDSN) so they never fail while upgrading from reader to writer.
		sqlDB.SetMaxOpenConns(sqliteMaxConns)
	case driver == DriverPostgres:
		maxConns = postgresMaxConns
		sqlDB.SetMaxOpenConns(postgresMaxConns)
		sqlDB.SetMaxIdleConns(postgresMaxConns)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, 0, err
	}
	return bun.NewDB(sqlDB, dialect), maxConns, nil
}

// Open connects to the database and, unless told otherwise, applies pending migrations.
func Open(ctx context.Context, driver, dsn string, opts ...Option) (*Store, error) {
	cfg := openConfig{}
	for _, o := range opts {
		o(&cfg)
	}

	db, maxConns, err := connect(ctx, driver, dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, driver: driver, maxConns: maxConns, cipher: cfg.cipher}
	if !cfg.withoutBody {
		s.BodyStore = &BodyStore{db: db, driver: driver, maxConns: maxConns}
		if cfg.cipher != nil {
			s.BodyStore.cipher = cfg.cipher.bodyCipher() // derived: one key file covers both domains
		}
	}

	switch cfg.migrate {
	case migrateUp:
		if _, err := s.Migrate(ctx); err != nil {
			s.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	case migrateRequire:
		p, err := s.provider()
		if err == nil {
			err = s.checkVersion(ctx, p)
		}
		if err == nil {
			var pending bool
			if pending, err = s.HasPending(ctx); err == nil && pending {
				err = errors.New("database has pending migrations: run `dexafix db migrate up` or enable --auto-migrate")
			}
		}
		if err != nil {
			s.Close()
			return nil, err
		}
	}
	if cfg.cipher != nil && cfg.migrate != migrateSkip {
		// Every domain decides whether the key is the right one before any of them writes a check, so
		// a wrong key leaves none behind (the right key would be refused at the next start).
		writes := make([]func() error, 0, 2)
		write, err := s.verifyKey(ctx)
		if err != nil {
			s.Close()
			return nil, err
		}
		writes = append(writes, write)
		if s.BodyStore != nil {
			if write, err = s.BodyStore.verifyKey(ctx); err != nil {
				s.Close()
				return nil, err
			}
			writes = append(writes, write)
		}
		for _, w := range writes {
			if w == nil {
				continue
			}
			if err := w(); err != nil {
				s.Close()
				return nil, err
			}
		}
		if err := s.encryptExisting(ctx); err != nil {
			s.Close()
			return nil, fmt.Errorf("encrypt existing data: %w", err)
		}
		if s.BodyStore != nil {
			if err := s.BodyStore.encryptExisting(ctx); err != nil {
				s.Close()
				return nil, fmt.Errorf("encrypt existing readings and scans: %w", err)
			}
		}
	}
	return s, nil
}

const (
	sqliteMaxConns   = 4
	postgresMaxConns = 20
)

// sqliteFilePath returns the database file named by a SQLite DSN ("file:x.db?_pragma=..." or a
// plain path), or "" for in-memory databases.
func sqliteFilePath(dsn string) string {
	if sqliteInMemory(dsn) {
		return ""
	}
	p := strings.TrimPrefix(dsn, "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if strings.HasPrefix(p, "//") { // file:///abs/path
		p = strings.TrimPrefix(p, "//")
		if !strings.HasPrefix(p, "/") {
			p = p[strings.IndexByte(p+"/", '/'):] // drop a host part
		}
	}
	return p
}

// secureSQLiteFile makes sure a new database file is private to its owner (SQLite gives its -wal
// and -shm files the same permissions). The file holds password hashes and health data. An
// existing file is not changed, since it may be shared on purpose, but a warning is logged when
// other users can read it.
func secureSQLiteFile(dsn string) {
	path := sqliteFilePath(dsn)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		f.Close() // created empty with mode 0600; SQLite fills it in
		return
	}
	if info, serr := os.Stat(path); serr == nil && info.Mode().Perm()&0o077 != 0 {
		log.Warn().Str("file", path).Str("mode", info.Mode().Perm().String()).Msg("the database file can be read by other users: run chmod 600 on it")
	}
}

func sqliteInMemory(dsn string) bool {
	return strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")
}

// sqliteDSN makes sure foreign keys are enforced (they are off by default in SQLite and only
// switchable per connection), that writers wait for locks instead of failing immediately and, for
// files, that the database runs in WAL mode with transactions that take the write lock up front.
func sqliteDSN(dsn string) string {
	pragmas := []string{"foreign_keys(1)", "busy_timeout(5000)"}
	if !sqliteInMemory(dsn) {
		pragmas = append(pragmas, "journal_mode(WAL)", "synchronous(NORMAL)")
	}
	add := func(kv string) {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + kv
	}
	for _, pragma := range pragmas {
		name := pragma[:strings.IndexByte(pragma, '(')]
		if !strings.Contains(dsn, name+"(") {
			add("_pragma=" + pragma)
		}
	}
	if !sqliteInMemory(dsn) && !strings.Contains(dsn, "_txlock=") {
		add("_txlock=immediate")
	}
	return dsn
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database answers. It is defined here and not promoted from the BodyStore,
// which is nil in a store opened WithoutBody.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func now() time.Time { return time.Now().UTC() }

// ---- users ----

// Ids are below 2^53, so an id stays exact as a JSON number and in JavaScript. Ids start at 1; 0
// means "nobody" to the body service.
// newUserID returns a uniformly random user id in [1, 2^53). Ids are random instead of counted so
// that nobody can guess another user's id or tell how many users there are.
func newUserID() int64 {
	var b [8]byte
	for {
		_, _ = crand.Read(b[:]) // never fails: since Go 1.24 a broken random source ends the program
		// 53 random bits, rejecting 0, so every id is equally likely.
		if id := int64(binary.BigEndian.Uint64(b[:]) >> 11); id > 0 {
			return id
		}
	}
}

// userIDSource makes the ids; tests replace it to force collisions.
var userIDSource = newUserID

// sequentialUserIDs is only for tests, which read better with the ids 1, 2, 3.
var sequentialUserIDs atomic.Bool

// SequentialUserIDsForTests makes new users get the ids 1, 2, 3... instead of random ones. Call it
// from TestMain of a test binary, never from production code: random ids are a defence.
func SequentialUserIDsForTests() { sequentialUserIDs.Store(true) }

// SetSequentialUserIDsForTests turns the sequential ids of SequentialUserIDsForTests on or off, for
// the test that wants random ids in a binary that otherwise uses counted ones.
func SetSequentialUserIDsForTests(on bool) { sequentialUserIDs.Store(on) }

// ExecForTests runs one statement for a test that has to break the database on purpose, such as
// renaming a table so that every query on it fails. Never call it from production code.
func (s *Store) ExecForTests(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

// freeUserID picks a random id that no user has yet. A collision is 1 in 10^15 for a store of
// millions of users, so a few tries are plenty.
func (s *Store) freeUserID(ctx context.Context, tx bun.Tx) (int64, error) {
	if sequentialUserIDs.Load() {
		var max sql.NullInt64
		if err := tx.NewSelect().Model((*userRow)(nil)).ColumnExpr("MAX(id)").Scan(ctx, &max); err != nil {
			return 0, err
		}
		return max.Int64 + 1, nil
	}
	for range 5 {
		id := userIDSource()
		taken, err := tx.NewSelect().Model((*userRow)(nil)).Where("id = ?", id).Exists(ctx)
		if err != nil {
			return 0, err
		}
		if !taken {
			return id, nil
		}
	}
	return 0, errors.New("could not find a free user id")
}

func (s *Store) CreateUser(ctx context.Context, name string) (models.User, error) {
	return s.createUser(ctx, name, "", nil)
}

// CreateUserWithIdentity creates a user together with a way to sign in.
func (s *Store) CreateUserWithIdentity(ctx context.Context, name, email string, id models.Identity) (models.User, error) {
	return s.createUser(ctx, name, email, &id)
}

func (s *Store) createUser(ctx context.Context, name, email string, id *models.Identity) (models.User, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return models.User{}, fmt.Errorf("%w: name must be 1-64 characters", store.ErrInvalid)
	}
	email = models.NormalizeEmail(email)
	var row userRow
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		n, err := tx.NewSelect().Model((*userRow)(nil)).Count(ctx)
		if err != nil {
			return err
		}
		uid, err := s.freeUserID(ctx, tx)
		if err != nil {
			return err
		}
		row = userRow{ID: uid, Name: name, Role: models.RoleUser, Timezone: "UTC", CreatedAt: now(), EmailLookup: s.emailLookup(email), JobPriority: models.DefaultJobPriority}
		if n == 0 {
			row.Role = models.RoleAdmin
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		enc := s.encrypt(email, emailContext(row.ID)) // the context needs the new id
		if _, err := tx.NewUpdate().Model((*userRow)(nil)).Set("email = ?", enc).Where("id = ?", row.ID).Exec(ctx); err != nil {
			return err
		}
		if id == nil {
			return nil
		}
		ir := identityRow{Provider: id.Provider, Subject: id.Subject, UserID: row.ID, Secret: id.Secret, CreatedAt: now()}
		if ir.Subject == "" {
			ir.Subject = strconv.FormatInt(row.ID, 10)
		}
		_, err = tx.NewInsert().Model(&ir).Exec(ctx)
		return err
	})
	if err != nil {
		if isUnique(err) {
			return models.User{}, store.ErrExists
		}
		return models.User{}, err
	}
	u := row.toModel()
	u.Email = email
	return u, nil
}

// UpdateUserSettings stores the athlete-mode default and the method accuracy ranking.
func (s *Store) UpdateUserSettings(ctx context.Context, userID int64, athleteDefault bool, ranking []string) error {
	res, err := s.db.NewUpdate().Model((*userRow)(nil)).
		Set("athlete_default = ?", athleteDefault).
		Set("method_ranking = ?", strings.Join(models.NormalizeRanking(ranking), ",")).
		Set("data_version = data_version + 1").
		Where("id = ?", userID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// UpdateUserHiddenColumns stores which daily table columns the user has hidden.
func (s *Store) UpdateUserHiddenColumns(ctx context.Context, userID int64, keys []string) error {
	res, err := s.db.NewUpdate().Model((*userRow)(nil)).
		Set("hidden_columns = ?", strings.Join(keys, ",")).
		Where("id = ?", userID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) ListUsers(ctx context.Context) ([]models.User, error) {
	var rows []userRow
	if err := s.db.NewSelect().Model(&rows).OrderExpr("name").Scan(ctx); err != nil {
		return nil, err
	}
	var out []models.User
	for i := range rows {
		u, err := s.userModel(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	return s.db.NewSelect().Model((*userRow)(nil)).Exists(ctx)
}

func (s *Store) GetUser(ctx context.Context, id int64) (models.User, error) {
	var row userRow
	if err := s.db.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return models.User{}, store.ErrNotFound
		}
		return models.User{}, err
	}
	return s.userModel(&row)
}

// ---- integrations ----

func (s *Store) GetIntegration(ctx context.Context, userID int64, provider string) ([]byte, error) {
	var row integrationRow
	if err := s.db.NewSelect().Model(&row).Where("user_id = ?", userID).Where("provider = ?", provider).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	data, err := s.decrypt(row.Data, integrationContext(userID, provider))
	if err != nil {
		return nil, err
	}
	return []byte(data), nil
}

func (s *Store) PutIntegration(ctx context.Context, userID int64, provider string, data []byte) error {
	enc := s.encrypt(string(data), integrationContext(userID, provider))
	row := integrationRow{UserID: userID, Provider: provider, Data: enc, UpdatedAt: now()}
	_, err := s.db.NewInsert().Model(&row).
		On("CONFLICT (user_id, provider) DO UPDATE").
		Set("data = EXCLUDED.data").Set("updated_at = EXCLUDED.updated_at").Exec(ctx)
	if isFK(err) {
		return store.ErrNotFound
	}
	return err
}

func (s *Store) DeleteIntegration(ctx context.Context, userID int64, provider string) error {
	_, err := s.db.NewDelete().Model((*integrationRow)(nil)).Where("user_id = ?", userID).Where("provider = ?", provider).Exec(ctx)
	return err
}

// SetUserStrengthLevelAuto allows or forbids automatic Strength Level sync for a user. Forbidding
// it deletes the stored login in the same transaction.
func (s *Store) SetUserStrengthLevelAuto(ctx context.Context, userID int64, enabled bool) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model((*userRow)(nil)).
			Set("strengthlevel_auto = ?", enabled).Set("strengthlevel_bulk = strengthlevel_bulk AND ?", enabled).
			Where("id = ?", userID).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrNotFound
		}
		if !enabled {
			_, err = tx.NewDelete().Model((*integrationRow)(nil)).
				Where("user_id = ?", userID).Where("provider = ?", "strengthlevel").Exec(ctx)
		}
		return err
	})
}

// SetUserStrengthLevelBulk allows or forbids the multi-day sync for a user who may sync.
func (s *Store) SetUserStrengthLevelBulk(ctx context.Context, userID int64, enabled bool) error {
	res, err := s.db.NewUpdate().Model((*userRow)(nil)).
		Set("strengthlevel_bulk = ?", enabled).Where("id = ?", userID).Where("strengthlevel_auto = ?", true).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}
