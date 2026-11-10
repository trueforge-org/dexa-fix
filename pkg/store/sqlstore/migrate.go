package sqlstore

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

// Migrations live in one directory per backend and are applied with goose. Every schema change must add
// a file with the same version to both directories (enforced by TestMigrationParity).
//
//go:embed migrations
var migrationsFS embed.FS

func (s *Store) provider() (*goose.Provider, error) {
	sub, err := fs.Sub(migrationsFS, "migrations/"+s.driver)
	if err != nil {
		return nil, err
	}
	dialect := goose.DialectSQLite3
	if s.driver == DriverPostgres {
		dialect = goose.DialectPostgres
	}
	return goose.NewProvider(dialect, s.db.DB, sub)
}

// latestVersion is the newest migration embedded in this binary.
func latestVersion(p *goose.Provider) int64 {
	var v int64
	for _, src := range p.ListSources() {
		if src.Version > v {
			v = src.Version
		}
	}
	return v
}

// checkVersion refuses to run against a database that a newer binary has already migrated.
func (s *Store) checkVersion(ctx context.Context, p *goose.Provider) error {
	cur, err := p.GetDBVersion(ctx)
	if err != nil {
		return err
	}
	if latest := latestVersion(p); cur > latest {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d): upgrade DexaFix", cur, latest)
	}
	return nil
}

// Migrate applies all pending migrations: the main database's, then the body domain's when this
// store has one (they have separate migration sets and version tables).
func (s *Store) Migrate(ctx context.Context) ([]*goose.MigrationResult, error) {
	p, err := s.provider()
	if err != nil {
		return nil, err
	}
	if err := s.checkVersion(ctx, p); err != nil {
		return nil, err
	}
	res, err := p.Up(ctx)
	if err != nil || s.BodyStore == nil {
		return res, err
	}
	more, err := s.BodyStore.Migrate(ctx)
	return append(res, more...), err
}

// MigrateDown rolls back the most recently applied migration.
func (s *Store) MigrateDown(ctx context.Context) (*goose.MigrationResult, error) {
	p, err := s.provider()
	if err != nil {
		return nil, err
	}
	return p.Down(ctx)
}

// MigrateTo migrates up or down until the database is at the given version (0 = empty).
func (s *Store) MigrateTo(ctx context.Context, version int64) ([]*goose.MigrationResult, error) {
	p, err := s.provider()
	if err != nil {
		return nil, err
	}
	cur, err := p.GetDBVersion(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case version > cur:
		return p.UpTo(ctx, version)
	case version < cur:
		return p.DownTo(ctx, version)
	}
	return nil, nil
}

// MigrationStatus lists every known migration and whether it has been applied.
func (s *Store) MigrationStatus(ctx context.Context) ([]*goose.MigrationStatus, error) {
	p, err := s.provider()
	if err != nil {
		return nil, err
	}
	return p.Status(ctx)
}

// SchemaVersion returns the currently applied migration version (0 = none).
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	p, err := s.provider()
	if err != nil {
		return 0, err
	}
	return p.GetDBVersion(ctx)
}

// HasPending reports whether there are unapplied migrations, in the main database or the body domain.
func (s *Store) HasPending(ctx context.Context) (bool, error) {
	p, err := s.provider()
	if err != nil {
		return false, err
	}
	if pending, err := p.HasPending(ctx); err != nil || pending {
		return pending, err
	}
	if s.BodyStore == nil {
		return false, nil
	}
	return s.BodyStore.HasPending(ctx)
}
