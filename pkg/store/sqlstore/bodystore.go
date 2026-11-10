package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// BodyStore is the body domain: a user's scale readings and DEXA scans, and the counter of their
// changes. It has its own migrations (migrations/body, with a version table of its own), its own
// key check and a cipher of its own, so it can live in a database of its own, in the process of the
// body service. Inside a Store it shares the connection pool of the main database.
type BodyStore struct {
	db       *bun.DB
	driver   string
	maxConns int
	cipher   *Cipher // the body domain's cipher; nil leaves new values unencrypted
	ownsDB   bool    // Close closes the pool (OpenBody); inside a Store the Store does
}

var _ store.BodyStore = (*BodyStore)(nil)

// Ping checks that the database answers.
func (b *BodyStore) Ping(ctx context.Context) error { return b.db.PingContext(ctx) }

// bodyMigrationTable is the goose version table of the body domain, so that its migrations and the
// main database's count separately even in one database.
const bodyMigrationTable = "goose_body_version"

// checkBody is the body_meta row holding the key check value, like checkSetting for the main database.
const checkBody = "crypto.check"

// OpenBody opens a database that holds only the body domain, for the body service. The cipher
// option (WithCipher) is a master key: the body key derives from it, so a body process may share
// the key file of the main backend, or have one of its own. WithoutMigrate and RequireMigrated work
// as for Open.
func OpenBody(ctx context.Context, driver, dsn string, opts ...Option) (*BodyStore, error) {
	cfg := openConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	db, maxConns, err := connect(ctx, driver, dsn)
	if err != nil {
		return nil, err
	}
	b := &BodyStore{db: db, driver: driver, maxConns: maxConns, ownsDB: true}
	if cfg.cipher != nil {
		b.cipher = cfg.cipher.bodyCipher()
	}
	switch cfg.migrate {
	case migrateUp:
		if _, err := b.Migrate(ctx); err != nil {
			b.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	case migrateRequire:
		pending, err := b.HasPending(ctx)
		if err == nil && pending {
			err = errors.New("the body database has pending migrations: enable --auto-migrate")
		}
		if err != nil {
			b.Close()
			return nil, err
		}
	}
	if cfg.cipher != nil && cfg.migrate != migrateSkip {
		write, err := b.verifyKey(ctx)
		if err != nil {
			b.Close()
			return nil, err
		}
		if write != nil {
			if err := write(); err != nil {
				b.Close()
				return nil, err
			}
		}
		if err := b.encryptExisting(ctx); err != nil {
			b.Close()
			return nil, fmt.Errorf("encrypt existing data: %w", err)
		}
	}
	return b, nil
}

// Close closes the connection pool when this store opened it. Inside a Store it does nothing: the
// Store closes the pool.
func (b *BodyStore) Close() error {
	if b.ownsDB {
		return b.db.Close()
	}
	return nil
}

// MaxConns is the size of the connection pool.
func (b *BodyStore) MaxConns() int { return b.maxConns }

// ---- migrations ----

func (b *BodyStore) provider() (*goose.Provider, error) {
	sub, err := fs.Sub(migrationsFS, "migrations/body/"+b.driver)
	if err != nil {
		return nil, err
	}
	dialect := goose.DialectSQLite3
	if b.driver == DriverPostgres {
		dialect = goose.DialectPostgres
	}
	return goose.NewProvider(dialect, b.db.DB, sub, goose.WithTableName(bodyMigrationTable))
}

// Migrate applies the pending migrations of the body domain.
func (b *BodyStore) Migrate(ctx context.Context) ([]*goose.MigrationResult, error) {
	p, err := b.provider()
	if err != nil {
		return nil, err
	}
	cur, err := p.GetDBVersion(ctx)
	if err != nil {
		return nil, err
	}
	if latest := latestVersion(p); cur > latest {
		return nil, fmt.Errorf("the body database schema version %d is newer than this binary supports (%d): upgrade dexa-fix", cur, latest)
	}
	return p.Up(ctx)
}

// HasPending reports whether the body domain has unapplied migrations.
func (b *BodyStore) HasPending(ctx context.Context) (bool, error) {
	p, err := b.provider()
	if err != nil {
		return false, err
	}
	return p.HasPending(ctx)
}

// SchemaVersion is the applied migration version of the body domain (0 = none).
func (b *BodyStore) SchemaVersion(ctx context.Context) (int64, error) {
	p, err := b.provider()
	if err != nil {
		return 0, err
	}
	return p.GetDBVersion(ctx)
}

// ---- key check, encryption of plaintext rows, rotation ----

// firstEncryptedBody finds one encrypted row of the body domain and the context that seals it.
func firstEncryptedBody(ctx context.Context, db queryer) (value, sealCtx string, found bool, err error) {
	var (
		scale []scaleRow
		dexa  []dexaRow
	)
	if err = db.NewSelect().Model(&scale).Where("data LIKE ?", encPrefix+"%").Limit(1).Scan(ctx); err != nil {
		return
	}
	if len(scale) > 0 {
		return scale[0].Data, scaleContext(scale[0].UserID, scale[0].RowUID), true, nil
	}
	if err = db.NewSelect().Model(&dexa).Where("data LIKE ?", encPrefix+"%").Limit(1).Scan(ctx); err != nil {
		return
	}
	if len(dexa) > 0 {
		return dexa[0].Data, dexaContext(dexa[0].UserID, dexa[0].RowUID), true, nil
	}
	return "", "", false, nil
}

// errWrongBodyKey explains a body key that does not match the body data.
const errWrongBodyKey = "the body encryption key does not match this database: the key file is missing, the wrong file, " +
	"or a different key created the readings and scans. Restore the original key; to switch to a new key keep the " +
	"old one as a previous key and run `dexa-fix db rekey` for the body data"

// verifyKey makes sure the configured key belongs to this body data: it compares the check value in
// body_meta, writes it on first use (only when no encrypted row exists that could belong to another
// key), and for a database that has none opens one of the rows. Like the main store's, it only
// decides; the caller writes the check it returns once every domain has accepted the key.
func (b *BodyStore) verifyKey(ctx context.Context) (write func() error, err error) {
	var row bodyMetaRow
	err = b.db.NewSelect().Model(&row).Where("name = ?", checkBody).Scan(ctx)
	switch {
	case err == nil:
		e := b.cipher.matchCheck(row.Value)
		if e == nil {
			return nil, errors.New(errWrongBodyKey)
		}
		if e != b.cipher.current {
			log.Warn().Str("key", e.id).Msg("the readings and scans are still encrypted with a previous key: run `dexa-fix db rekey`")
		}
		return nil, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	value, vctx, found, err := firstEncryptedBody(ctx, b.db)
	if err != nil {
		return nil, err
	}
	if found {
		_, e, err := b.cipher.open(value, vctx)
		if err != nil {
			return nil, fmt.Errorf("%s (%w)", errWrongBodyKey, err)
		}
		if e != b.cipher.current {
			log.Warn().Str("key", e.id).Msg("the readings and scans are still encrypted with a previous key: run `dexa-fix db rekey`")
			return nil, nil
		}
	}
	return func() error { return b.writeCheck(ctx, b.db) }, nil
}

func (b *BodyStore) writeCheck(ctx context.Context, db execer) error {
	row := bodyMetaRow{Name: checkBody, Value: b.cipher.current.check, UpdatedAt: now()}
	_, err := db.NewInsert().Model(&row).
		On("CONFLICT (name) DO UPDATE").
		Set("value = EXCLUDED.value").
		// The date marks when this key was introduced: writing the same check again keeps it.
		Set("updated_at = CASE WHEN body_meta_row.value = EXCLUDED.value THEN body_meta_row.updated_at ELSE EXCLUDED.updated_at END").
		Exec(ctx)
	return err
}

// HasEncryptedValues reports whether the body data holds encrypted rows.
func (b *BodyStore) HasEncryptedValues(ctx context.Context) (bool, error) {
	_, _, found, err := firstEncryptedBody(ctx, b.db)
	if err != nil && isMissingTable(err) {
		return false, nil
	}
	return found, err
}

// encryptExisting encrypts rows written while there was no key. It runs in one transaction and
// skips rows that are already encrypted, so it is safe on every start.
func (b *BodyStore) encryptExisting(ctx context.Context) error {
	return b.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		needs := func(v string) bool { return v != "" && !strings.HasPrefix(v, encPrefix) }
		var scales []scaleRow
		if err := tx.NewSelect().Model(&scales).Scan(ctx); err != nil {
			return err
		}
		for _, r := range scales {
			if !needs(r.Data) {
				continue
			}
			enc, err := b.cipher.seal(r.Data, scaleContext(r.UserID, r.RowUID))
			if err != nil {
				return err
			}
			if _, err := tx.NewUpdate().Model((*scaleRow)(nil)).Set("data = ?", enc).Where("id = ?", r.ID).Exec(ctx); err != nil {
				return err
			}
		}
		var dexas []dexaRow
		if err := tx.NewSelect().Model(&dexas).Scan(ctx); err != nil {
			return err
		}
		for _, r := range dexas {
			if !needs(r.Data) {
				continue
			}
			enc, err := b.cipher.seal(r.Data, dexaContext(r.UserID, r.RowUID))
			if err != nil {
				return err
			}
			if _, err := tx.NewUpdate().Model((*dexaRow)(nil)).Set("data = ?", enc).Where("id = ?", r.ID).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

// BodyRekeyStats counts what Rekey changed (or, in a dry run, would change) in the body data.
type BodyRekeyStats struct {
	Changed int // rows re-encrypted
	Current int // rows already under the current key
}

// Rekey re-encrypts every row with the current body key and writes the new check value, in one
// transaction: it completes or leaves the data untouched. Rows already under the current key are
// skipped, so a run can be repeated. With dryRun nothing is written.
func (b *BodyStore) Rekey(ctx context.Context, dryRun bool) (BodyRekeyStats, error) {
	var stats BodyRekeyStats
	if b.cipher == nil {
		return stats, errors.New("rekey needs an encryption key")
	}
	err := b.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		stats = BodyRekeyStats{}
		// The context (user and row_uid) stays the same, so only the data column changes.
		reseal := func(kind, value, sealCtx string) (string, error) {
			if !strings.HasPrefix(value, encPrefix) {
				return "", nil
			}
			plain, e, err := b.cipher.open(value, sealCtx)
			if err != nil {
				return "", fmt.Errorf("%s: %w", kind, err)
			}
			if e == b.cipher.current && strings.HasPrefix(value, encV2) {
				stats.Current++
				return "", nil
			}
			return b.cipher.seal(plain, sealCtx)
		}
		var scales []scaleRow
		if err := tx.NewSelect().Model(&scales).OrderExpr("id").Scan(ctx); err != nil {
			return err
		}
		for _, r := range scales {
			enc, err := reseal(fmt.Sprintf("scale reading %d", r.ID), r.Data, scaleContext(r.UserID, r.RowUID))
			if err != nil {
				return err
			}
			if enc == "" {
				continue
			}
			stats.Changed++
			if dryRun {
				continue
			}
			if _, err := tx.NewUpdate().Model((*scaleRow)(nil)).Set("data = ?", enc).Where("id = ?", r.ID).Exec(ctx); err != nil {
				return err
			}
		}
		var dexas []dexaRow
		if err := tx.NewSelect().Model(&dexas).OrderExpr("id").Scan(ctx); err != nil {
			return err
		}
		for _, r := range dexas {
			enc, err := reseal(fmt.Sprintf("dexa scan %d", r.ID), r.Data, dexaContext(r.UserID, r.RowUID))
			if err != nil {
				return err
			}
			if enc == "" {
				continue
			}
			stats.Changed++
			if dryRun {
				continue
			}
			if _, err := tx.NewUpdate().Model((*dexaRow)(nil)).Set("data = ?", enc).Where("id = ?", r.ID).Exec(ctx); err != nil {
				return err
			}
		}
		if dryRun {
			return nil
		}
		return b.writeCheck(ctx, tx)
	})
	if err != nil {
		return BodyRekeyStats{}, err
	}
	return stats, nil
}
