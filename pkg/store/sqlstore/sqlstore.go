// Package sqlstore implements store.Store on top of bun, for SQLite and PostgreSQL.
// Schema changes are versioned goose migrations (see migrate.go).
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
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
	if driver == DriverSQLite {
		sqlDB.SetMaxOpenConns(1) // sqlite allows a single writer; avoids "database is locked"
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

// sqliteDSN makes sure foreign keys are enforced (they are off by default in SQLite and only
// switchable per connection) and that writers wait for locks instead of failing immediately.
func sqliteDSN(dsn string) string {
	for _, pragma := range []string{"foreign_keys(1)", "busy_timeout(5000)"} {
		name := pragma[:strings.IndexByte(pragma, '(')]
		if strings.Contains(dsn, name+"(") {
			continue
		}
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "_pragma=" + pragma
	}
	return dsn
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func now() time.Time { return time.Now().UTC() }

// ---- users ----

func (s *Store) CreateUser(ctx context.Context, name string) (models.User, error) {
	return s.createUser(ctx, name, nil)
}

// CreateUserWithIdentity creates a user together with a way to sign in.
func (s *Store) CreateUserWithIdentity(ctx context.Context, name string, id models.Identity) (models.User, error) {
	return s.createUser(ctx, name, &id)
}

func (s *Store) createUser(ctx context.Context, name string, id *models.Identity) (models.User, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return models.User{}, fmt.Errorf("%w: name must be 1-64 characters", store.ErrInvalid)
	}
	var row userRow
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		n, err := tx.NewSelect().Model((*userRow)(nil)).Count(ctx)
		if err != nil {
			return err
		}
		row = userRow{Name: name, Role: models.RoleUser, CreatedAt: now()}
		if n == 0 {
			row.Role = models.RoleAdmin
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
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
	return row.toModel(), nil
}

// UpdateUserSettings stores the athlete-mode default and the method accuracy ranking.
func (s *Store) UpdateUserSettings(ctx context.Context, userID int64, athleteDefault bool, ranking []string) error {
	res, err := s.db.NewUpdate().Model((*userRow)(nil)).
		Set("athlete_default = ?", athleteDefault).
		Set("method_ranking = ?", strings.Join(models.NormalizeRanking(ranking), ",")).
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
	if _, err := s.db.NewInsert().Model(&row).Exec(ctx); err != nil {
		if isFK(err) {
			return store.ErrNotFound
		}
		return err
	}
	d.ID = row.ID
	return nil
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

func (s *Store) UpsertScaleReadings(ctx context.Context, userID int64, rs []models.ScaleReading) (int, error) {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for i := range rs {
			r := &rs[i]
			if r.MeasuredAt.IsZero() {
				return fmt.Errorf("%w: measured_at is required", store.ErrInvalid)
			}
			if r.Source == "" {
				r.Source = models.SourceManual
			}
			if r.Method == "" {
				r.Method = models.MethodScale
			}
			if !models.ValidMethod(r.Method) {
				return fmt.Errorf("%w: unknown method %q", store.ErrInvalid, r.Method)
			}
			row := scaleRow{
				UserID: userID, MeasuredAt: r.MeasuredAt.UTC(),
				WeightKg: r.WeightKg, FatRatioPct: r.FatRatioPct, FatMassKg: r.FatMassKg, FatFreeMassKg: r.FatFreeMassKg,
				MuscleMassKg: r.MuscleMassKg, HydrationKg: r.HydrationKg, BoneMassKg: r.BoneMassKg, VisceralFatIndex: r.VisceralFatIndex,
				Source: r.Source, Method: r.Method, AthleteMode: r.AthleteMode, ExternalID: r.ExternalID,
				Notes: r.Notes, CreatedAt: now(),
			}
			q := tx.NewInsert().Model(&row).Returning("NULL").On("CONFLICT (user_id, source, external_id) DO UPDATE")
			for _, c := range scaleValueCols {
				q = q.Set(c + " = EXCLUDED." + c)
			}
			if _, err := q.Exec(ctx); err != nil {
				if isFK(err) {
					return store.ErrNotFound
				}
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(rs), nil
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
	res, err := s.db.NewDelete().Model(model).Where("id = ?", id).Where("user_id = ?", userID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
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
