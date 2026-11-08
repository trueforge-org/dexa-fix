// Package sqlstore implements store.Store on top of database/sql for SQLite and PostgreSQL.
package sqlstore

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // pure-Go sqlite driver

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

//go:embed migrations
var migrationsFS embed.FS

// Supported database drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// Store is a database/sql backed store.Store.
type Store struct {
	db     *sql.DB
	driver string
}

var _ store.Store = (*Store)(nil)

// Open connects to the database and applies pending migrations.
func Open(ctx context.Context, driver, dsn string) (*Store, error) {
	var sqlDriver string
	switch driver {
	case DriverSQLite:
		sqlDriver = "sqlite"
	case DriverPostgres:
		sqlDriver = "pgx"
	default:
		return nil, fmt.Errorf("unsupported db driver %q (use %q or %q)", driver, DriverSQLite, DriverPostgres)
	}
	db, err := sql.Open(sqlDriver, dsn)
	if err != nil {
		return nil, err
	}
	if driver == DriverSQLite {
		db.SetMaxOpenConns(1) // sqlite allows a single writer; avoids "database is locked"
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, driver: driver}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// q rewrites ? placeholders to $n for postgres.
func (s *Store) q(query string) string {
	if s.driver != DriverPostgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	dir := "migrations/" + s.driver
	entries, err := migrationsFS.ReadDir(dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var n int
		if err := s.db.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`), name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile(dir + "/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, stmt := range strings.Split(string(body), ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO schema_migrations (version) VALUES (?)`), name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "UNIQUE constraint failed") || strings.Contains(m, "SQLSTATE 23505")
}

func isFK(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "FOREIGN KEY constraint failed") || strings.Contains(m, "SQLSTATE 23503")
}

func now() time.Time { return time.Now().UTC() }

// ---- users ----

func (s *Store) CreateUser(ctx context.Context, name string) (models.User, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return models.User{}, fmt.Errorf("%w: name must be 1-64 characters", store.ErrInvalid)
	}
	u := models.User{Name: name, CreatedAt: now(), MethodRanking: models.DefaultRanking()}
	err := s.db.QueryRowContext(ctx, s.q(`INSERT INTO users (name, created_at) VALUES (?, ?) RETURNING id`), u.Name, u.CreatedAt).Scan(&u.ID)
	if isUnique(err) {
		return models.User{}, store.ErrExists
	}
	return u, err
}

const userCols = `id, name, created_at, athlete_default, method_ranking`

func scanUser(sc interface{ Scan(...any) error }) (models.User, error) {
	var u models.User
	var ranking string
	if err := sc.Scan(&u.ID, &u.Name, &u.CreatedAt, &u.AthleteDefault, &ranking); err != nil {
		return u, err
	}
	var keys []string
	if ranking != "" {
		keys = strings.Split(ranking, ",")
	}
	u.MethodRanking = models.NormalizeRanking(keys)
	return u, nil
}

// UpdateUserSettings stores the athlete-mode default and the method accuracy ranking.
func (s *Store) UpdateUserSettings(ctx context.Context, userID int64, athleteDefault bool, ranking []string) error {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE users SET athlete_default = ?, method_ranking = ? WHERE id = ?`),
		athleteDefault, strings.Join(models.NormalizeRanking(ranking), ","), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) ListUsers(ctx context.Context) ([]models.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) GetUser(ctx context.Context, id int64) (models.User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, s.q(`SELECT `+userCols+` FROM users WHERE id = ?`), id))
	if err == sql.ErrNoRows {
		return u, store.ErrNotFound
	}
	return u, err
}

// ---- dexa scans ----

const dexaCols = `weight_kg, fat_ratio_pct, fat_mass_kg, fat_free_mass_kg, lean_mass_kg, bone_mass_kg, visceral_fat_mass_kg, bmd_g_cm2, t_score, z_score, android_fat_pct, gynoid_fat_pct`

func dexaFields(d *models.DexaScan) []any {
	return []any{&d.WeightKg, &d.FatRatioPct, &d.FatMassKg, &d.FatFreeMassKg, &d.LeanMassKg, &d.BoneMassKg, &d.VisceralFatMassKg, &d.BMDGCm2, &d.TScore, &d.ZScore, &d.AndroidFatPct, &d.GynoidFatPct}
}

func (s *Store) CreateDexaScan(ctx context.Context, d *models.DexaScan) error {
	d.MeasuredAt = d.MeasuredAt.UTC().Truncate(24 * time.Hour)
	d.CreatedAt = now()
	args := []any{d.UserID, d.MeasuredAt}
	for _, f := range dexaFields(d) {
		args = append(args, *(f.(**float64)))
	}
	args = append(args, d.Notes, d.CreatedAt)
	query := s.q(`INSERT INTO dexa_scans (user_id, measured_at, ` + dexaCols + `, notes, created_at) VALUES (?, ?, ` + strings.Repeat("?, ", 12) + `?, ?) RETURNING id`)
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&d.ID)
	if isFK(err) {
		return store.ErrNotFound
	}
	return err
}

func (s *Store) ListDexaScans(ctx context.Context, userID int64) ([]models.DexaScan, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, user_id, measured_at, `+dexaCols+`, notes, created_at FROM dexa_scans WHERE user_id = ? ORDER BY measured_at DESC, id DESC`), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.DexaScan
	for rows.Next() {
		var d models.DexaScan
		dest := append([]any{&d.ID, &d.UserID, &d.MeasuredAt}, dexaFields(&d)...)
		dest = append(dest, &d.Notes, &d.CreatedAt)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		d.MeasuredAt = d.MeasuredAt.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) DeleteDexaScan(ctx context.Context, userID, id int64) error {
	return s.deleteOwned(ctx, `dexa_scans`, userID, id)
}

// ---- scale readings ----

const scaleCols = `weight_kg, fat_ratio_pct, fat_mass_kg, fat_free_mass_kg, muscle_mass_kg, hydration_kg, bone_mass_kg, visceral_fat_index`

func scaleFields(r *models.ScaleReading) []any {
	return []any{&r.WeightKg, &r.FatRatioPct, &r.FatMassKg, &r.FatFreeMassKg, &r.MuscleMassKg, &r.HydrationKg, &r.BoneMassKg, &r.VisceralFatIndex}
}

var scaleColList = strings.Split(strings.ReplaceAll(scaleCols, " ", ""), ",")

func (s *Store) UpsertScaleReadings(ctx context.Context, userID int64, rs []models.ScaleReading) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	sets := []string{"measured_at = excluded.measured_at", "notes = excluded.notes"}
	for _, c := range scaleColList {
		sets = append(sets, c+" = excluded."+c)
	}
	query := s.q(`INSERT INTO scale_readings (user_id, measured_at, ` + scaleCols + `, source, method, athlete_mode, external_id, notes, created_at) VALUES (?, ?, ` + strings.Repeat("?, ", 8) + `?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, source, external_id) DO UPDATE SET ` + strings.Join(sets, ", "))

	for i := range rs {
		r := &rs[i]
		if r.MeasuredAt.IsZero() {
			return 0, fmt.Errorf("%w: measured_at is required", store.ErrInvalid)
		}
		if r.Source == "" {
			r.Source = models.SourceManual
		}
		args := []any{userID, r.MeasuredAt.UTC()}
		for _, f := range scaleFields(r) {
			args = append(args, *(f.(**float64)))
		}
		if r.Method == "" {
			r.Method = models.MethodScale
		}
		if !models.ValidMethod(r.Method) {
			return 0, fmt.Errorf("%w: unknown method %q", store.ErrInvalid, r.Method)
		}
		args = append(args, r.Source, r.Method, r.AthleteMode, r.ExternalID, r.Notes, now())
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			if isFK(err) {
				return 0, store.ErrNotFound
			}
			return 0, err
		}
	}
	return len(rs), tx.Commit()
}

func (s *Store) ListScaleReadings(ctx context.Context, userID int64) ([]models.ScaleReading, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, user_id, measured_at, `+scaleCols+`, source, method, athlete_mode, external_id, notes, created_at FROM scale_readings WHERE user_id = ? ORDER BY measured_at DESC, id DESC`), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.ScaleReading
	for rows.Next() {
		var r models.ScaleReading
		dest := append([]any{&r.ID, &r.UserID, &r.MeasuredAt}, scaleFields(&r)...)
		dest = append(dest, &r.Source, &r.Method, &r.AthleteMode, &r.ExternalID, &r.Notes, &r.CreatedAt)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		r.MeasuredAt = r.MeasuredAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteScaleReading(ctx context.Context, userID, id int64) error {
	return s.deleteOwned(ctx, `scale_readings`, userID, id)
}

func (s *Store) deleteOwned(ctx context.Context, table string, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM `+table+` WHERE id = ? AND user_id = ?`), id, userID)
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
	var data string
	err := s.db.QueryRowContext(ctx, s.q(`SELECT data FROM user_integrations WHERE user_id = ? AND provider = ?`), userID, provider).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, store.ErrNotFound
	}
	return []byte(data), err
}

func (s *Store) PutIntegration(ctx context.Context, userID int64, provider string, data []byte) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO user_integrations (user_id, provider, data, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, provider) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`),
		userID, provider, string(data), now())
	if isFK(err) {
		return store.ErrNotFound
	}
	return err
}

func (s *Store) DeleteIntegration(ctx context.Context, userID int64, provider string) error {
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM user_integrations WHERE user_id = ? AND provider = ?`), userID, provider)
	return err
}
