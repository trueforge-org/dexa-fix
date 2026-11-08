// Package sqlstore implements store.Store on top of bun, for SQLite and PostgreSQL.
// Schema changes are versioned goose migrations (see migrate.go).
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/schema"
	_ "modernc.org/sqlite" // pure-Go sqlite driver

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// Supported database drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// Store is a bun backed store.Store.
type Store struct {
	db     *bun.DB
	driver string
	cipher *Cipher // encrypts sensitive columns; nil leaves new values unencrypted
}

var _ store.Store = (*Store)(nil)

type migrateMode int

const (
	migrateUp      migrateMode = iota // apply pending migrations (default)
	migrateSkip                       // leave the schema alone
	migrateRequire                    // fail if migrations are pending
)

type openConfig struct {
	migrate migrateMode
	cipher  *Cipher
}

// Option customises Open.
type Option func(*openConfig)

// WithoutMigrate opens the database without touching the schema (used by the db management commands).
func WithoutMigrate() Option { return func(c *openConfig) { c.migrate = migrateSkip } }

// RequireMigrated fails Open when there are pending migrations instead of applying them.
func RequireMigrated() Option { return func(c *openConfig) { c.migrate = migrateRequire } }

// Open connects to the database and, unless told otherwise, applies pending migrations.
func Open(ctx context.Context, driver, dsn string, opts ...Option) (*Store, error) {
	cfg := openConfig{}
	for _, o := range opts {
		o(&cfg)
	}

	var (
		sqlDB   *sql.DB
		dialect schema.Dialect
		err     error
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
		return nil, fmt.Errorf("unsupported db driver %q (use %q or %q)", driver, DriverSQLite, DriverPostgres)
	}
	if err != nil {
		return nil, err
	}
	switch {
	case driver == DriverSQLite && sqliteInMemory(dsn):
		sqlDB.SetMaxOpenConns(1) // every connection to an in-memory database would be a separate database
	case driver == DriverSQLite:
		// WAL lets readers run next to the single writer, so a long import no longer blocks page
		// loads. Writers queue on busy_timeout; transactions start with BEGIN IMMEDIATE (see
		// sqliteDSN) so they never fail while upgrading from reader to writer.
		sqlDB.SetMaxOpenConns(sqliteMaxConns)
	case driver == DriverPostgres:
		sqlDB.SetMaxOpenConns(postgresMaxConns)
		sqlDB.SetMaxIdleConns(postgresMaxConns)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	s := &Store{db: bun.NewDB(sqlDB, dialect), driver: driver, cipher: cfg.cipher}

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
			if pending, err = p.HasPending(ctx); err == nil && pending {
				err = errors.New("database has pending migrations: run `dexa-fix db migrate up` or enable --auto-migrate")
			}
		}
		if err != nil {
			s.Close()
			return nil, err
		}
	}
	if cfg.cipher != nil && cfg.migrate != migrateSkip {
		if err := s.encryptExisting(ctx); err != nil {
			s.Close()
			return nil, fmt.Errorf("encrypt existing data: %w", err)
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

func now() time.Time { return time.Now().UTC() }

// ---- users ----

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
		row = userRow{Name: name, Role: models.RoleUser, CreatedAt: now(), EmailLookup: s.emailLookup(email)}
		if n == 0 {
			row.Role = models.RoleAdmin
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		enc, err := s.encrypt(email, emailContext(row.ID)) // the context needs the new id
		if err != nil {
			return err
		}
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

// ---- dexa scans ----

func (s *Store) CreateDexaScan(ctx context.Context, d *models.DexaScan) error {
	d.MeasuredAt = d.MeasuredAt.UTC().Truncate(24 * time.Hour)
	d.CreatedAt = now()
	row := dexaRow{
		UserID: d.UserID, MeasuredAt: d.MeasuredAt,
		WeightKg: d.WeightKg, FatRatioPct: d.FatRatioPct, FatMassKg: d.FatMassKg, FatFreeMassKg: d.FatFreeMassKg,
		LeanMassKg: d.LeanMassKg, BoneMassKg: d.BoneMassKg, VisceralFatMassKg: d.VisceralFatMassKg,
		BMDGCm2: d.BMDGCm2, TScore: d.TScore, ZScore: d.ZScore, AndroidFatPct: d.AndroidFatPct, GynoidFatPct: d.GynoidFatPct,
		Notes: d.Notes, CreatedAt: d.CreatedAt,
	}
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		return bumpVersion(ctx, tx, d.UserID)
	})
	if err != nil {
		if isFK(err) {
			return store.ErrNotFound
		}
		return err
	}
	d.ID = row.ID
	return nil
}

// bumpVersion records that something the user's estimates depend on has changed. It runs in the
// same transaction as the change, so a reader never sees new data under an old version or the
// other way round. The version lives in the database, so every process sees it, whether the change
// came from the web app, the API, an import or a command.
func bumpVersion(ctx context.Context, db bun.IDB, userID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE users SET data_version = data_version + 1 WHERE id = ?`, userID)
	return err
}

func (s *Store) ListDexaScans(ctx context.Context, userID int64) ([]models.DexaScan, error) {
	var rows []dexaRow
	if err := s.db.NewSelect().Model(&rows).Where("user_id = ?", userID).
		OrderExpr("measured_at DESC, id DESC").Scan(ctx); err != nil {
		return nil, err
	}
	var out []models.DexaScan
	for i := range rows {
		out = append(out, rows[i].toModel())
	}
	return out, nil
}

func (s *Store) DeleteDexaScan(ctx context.Context, userID, id int64) error {
	return s.deleteOwned(ctx, (*dexaRow)(nil), userID, id)
}

// ---- scale readings ----

// upsertChunk is how many readings go into one INSERT. 500 rows of 16 columns stay far below the
// parameter limits of both SQLite and Postgres.
const upsertChunk = 500

func (s *Store) UpsertScaleReadings(ctx context.Context, userID int64, rs []models.ScaleReading) (int, error) {
	// Check everything first, so a bad reading late in a large batch fails before any work is done.
	rows := make([]scaleRow, 0, len(rs))
	for i := range rs {
		r := &rs[i]
		if r.MeasuredAt.IsZero() {
			return 0, fmt.Errorf("%w: measured_at is required", store.ErrInvalid)
		}
		if r.Source == "" {
			r.Source = models.SourceManual
		}
		if r.Method == "" {
			r.Method = models.MethodScale
		}
		if !models.ValidMethod(r.Method) {
			return 0, fmt.Errorf("%w: unknown method %q", store.ErrInvalid, r.Method)
		}
		rows = append(rows, scaleRow{
			UserID: userID, MeasuredAt: r.MeasuredAt.UTC(),
			WeightKg: r.WeightKg, FatRatioPct: r.FatRatioPct, FatMassKg: r.FatMassKg, FatFreeMassKg: r.FatFreeMassKg,
			MuscleMassKg: r.MuscleMassKg, HydrationKg: r.HydrationKg, BoneMassKg: r.BoneMassKg, VisceralFatIndex: r.VisceralFatIndex,
			Source: r.Source, Method: r.Method, AthleteMode: r.AthleteMode, ExternalID: r.ExternalID,
			Notes: r.Notes, CreatedAt: now(),
		})
	}
	rows = lastPerExternalID(rows)

	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// A re-delivered reading without notes (every Withings import) must not wipe the note
		// the user added in the app, so those rows are upserted without refreshing notes.
		var withNotes, withoutNotes []scaleRow
		for _, r := range rows {
			if r.Notes == "" {
				withoutNotes = append(withoutNotes, r)
			} else {
				withNotes = append(withNotes, r)
			}
		}
		for _, part := range []struct {
			rows []scaleRow
			cols []string
		}{{withNotes, scaleValueCols}, {withoutNotes, scaleValueColsKeepNotes}} {
			for start := 0; start < len(part.rows); start += upsertChunk {
				chunk := part.rows[start:min(start+upsertChunk, len(part.rows))]
				q := tx.NewInsert().Model(&chunk).Returning("NULL").On("CONFLICT (user_id, source, external_id) DO UPDATE")
				for _, c := range part.cols {
					q = q.Set(c + " = EXCLUDED." + c)
				}
				if _, err := q.Exec(ctx); err != nil {
					return err
				}
			}
		}
		if len(rows) == 0 {
			return nil
		}
		return bumpVersion(ctx, tx, userID)
	})
	if err != nil {
		if isFK(err) {
			return 0, store.ErrNotFound
		}
		return 0, err
	}
	return len(rs), nil
}

// lastPerExternalID drops earlier readings that share (source, external id) with a later one in
// the same batch. One INSERT must not touch the same row twice: Postgres refuses it, and SQLite
// would silently apply them in order. Readings without an external id never conflict.
func lastPerExternalID(rows []scaleRow) []scaleRow {
	type key struct{ source, id string }
	last := map[key]int{}
	for i, r := range rows {
		if r.ExternalID != nil {
			last[key{r.Source, *r.ExternalID}] = i
		}
	}
	if len(last) == 0 {
		return rows
	}
	out := rows[:0:0]
	for i, r := range rows {
		if r.ExternalID == nil || last[key{r.Source, *r.ExternalID}] == i {
			out = append(out, r)
		}
	}
	return out
}

func (s *Store) ListScaleReadings(ctx context.Context, userID int64) ([]models.ScaleReading, error) {
	var rows []scaleRow
	if err := s.db.NewSelect().Model(&rows).Where("user_id = ?", userID).
		OrderExpr("measured_at DESC, id DESC").Scan(ctx); err != nil {
		return nil, err
	}
	var out []models.ScaleReading
	for i := range rows {
		out = append(out, rows[i].toModel())
	}
	return out, nil
}

func (s *Store) DeleteScaleReading(ctx context.Context, userID, id int64) error {
	return s.deleteOwned(ctx, (*scaleRow)(nil), userID, id)
}

func (s *Store) deleteOwned(ctx context.Context, model any, userID, id int64) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewDelete().Model(model).Where("id = ?", id).Where("user_id = ?", userID).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrNotFound
		}
		return bumpVersion(ctx, tx, userID)
	})
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
	enc, err := s.encrypt(string(data), integrationContext(userID, provider))
	if err != nil {
		return err
	}
	row := integrationRow{UserID: userID, Provider: provider, Data: enc, UpdatedAt: now()}
	_, err = s.db.NewInsert().Model(&row).
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
