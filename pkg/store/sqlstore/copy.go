package sqlstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

// copyBatch is the number of rows inserted per statement.
const copyBatch = 500

// CopyStats reports how many rows Copy transferred per table.
type CopyStats struct {
	Users        int
	DexaScans    int
	ScaleReads   int
	Integrations int
	Identities   int
	AccessTokens int
	Settings     int
	Providers    int
	AuditEvents  int
}

// Copy transfers every row from src to dst, preserving ids. It works between any pair of backends.
// Both databases must be at the same schema version and dst must be empty. The copy is a single
// transaction on dst, so a failure leaves it untouched.
func Copy(ctx context.Context, src, dst *Store) (CopyStats, error) {
	var stats CopyStats

	sv, err := src.SchemaVersion(ctx)
	if err != nil {
		return stats, fmt.Errorf("source: %w", err)
	}
	dv, err := dst.SchemaVersion(ctx)
	if err != nil {
		return stats, fmt.Errorf("target: %w", err)
	}
	if sv != dv {
		return stats, fmt.Errorf("schema versions differ (source %d, target %d): run `dexa-fix db migrate up` on both", sv, dv)
	}
	for _, m := range []any{(*userRow)(nil), (*dexaRow)(nil), (*scaleRow)(nil), (*bodyStateRow)(nil), (*integrationRow)(nil), (*identityRow)(nil), (*totpRow)(nil), (*recoveryRow)(nil), (*passkeyRow)(nil), (*tokenRow)(nil), (*settingRow)(nil), (*providerRow)(nil), (*auditRow)(nil)} {
		q := dst.db.NewSelect().Model(m)
		if _, ok := m.(*settingRow); ok {
			q = q.Where("name <> ?", checkSetting) // written by Open when the target has a key
		}
		n, err := q.Count(ctx)
		if err != nil {
			return stats, fmt.Errorf("target: %w", err)
		}
		if n > 0 {
			return stats, errors.New("target database is not empty")
		}
	}

	err = dst.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if stats.Users, err = copyTable[userRow](ctx, src, tx, "id"); err != nil {
			return fmt.Errorf("users: %w", err)
		}
		if stats.DexaScans, err = copyTable[dexaRow](ctx, src, tx, "id"); err != nil {
			return fmt.Errorf("dexa_scans: %w", err)
		}
		if stats.ScaleReads, err = copyTable[scaleRow](ctx, src, tx, "id"); err != nil {
			return fmt.Errorf("scale_readings: %w", err)
		}
		if _, err = copyTable[bodyStateRow](ctx, src, tx, "user_id"); err != nil {
			return fmt.Errorf("body_user_state: %w", err)
		}
		if stats.Integrations, err = copyTable[integrationRow](ctx, src, tx, "user_id, provider"); err != nil {
			return fmt.Errorf("user_integrations: %w", err)
		}
		// Sessions are not copied: they are short-lived, and users simply sign in again.
		if stats.Identities, err = copyTable[identityRow](ctx, src, tx, "provider, subject"); err != nil {
			return fmt.Errorf("user_identities: %w", err)
		}
		if _, err = copyTable[passkeyRow](ctx, src, tx, "credential_id"); err != nil {
			return fmt.Errorf("passkeys: %w", err)
		}
		if _, err = copyTable[totpRow](ctx, src, tx, "user_id"); err != nil {
			return fmt.Errorf("user_totp: %w", err)
		}
		if _, err = copyTable[recoveryRow](ctx, src, tx, "user_id, code_hash"); err != nil {
			return fmt.Errorf("user_recovery_codes: %w", err)
		}
		if stats.AccessTokens, err = copyTable[tokenRow](ctx, src, tx, "id"); err != nil {
			return fmt.Errorf("access_tokens: %w", err)
		}
		// The source's key check goes with its ciphertext, so the target's own is replaced.
		if _, err := tx.NewDelete().Model((*settingRow)(nil)).Where("name = ?", checkSetting).Exec(ctx); err != nil {
			return fmt.Errorf("app_settings: %w", err)
		}
		if stats.Settings, err = copyTable[settingRow](ctx, src, tx, "name"); err != nil {
			return fmt.Errorf("app_settings: %w", err)
		}
		if stats.Providers, err = copyTable[providerRow](ctx, src, tx, "slug"); err != nil {
			return fmt.Errorf("auth_providers: %w", err)
		}
		if stats.AuditEvents, err = copyTable[auditRow](ctx, src, tx, "id"); err != nil {
			return fmt.Errorf("audit_log: %w", err)
		}
		if dst.driver == DriverPostgres {
			// Explicit ids do not advance the serial sequences; align them so new inserts do not collide.
			for _, table := range []string{"users", "dexa_scans", "scale_readings", "access_tokens", "audit_log"} {
				q := fmt.Sprintf(`SELECT setval(pg_get_serial_sequence('%[1]s', 'id'), COALESCE(MAX(id), 1), MAX(id) IS NOT NULL) FROM %[1]s`, table)
				if _, err := tx.ExecContext(ctx, q); err != nil {
					return fmt.Errorf("reset sequence %s: %w", table, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return CopyStats{}, err
	}
	return stats, nil
}

// copyTable pages through one table of src ordered by orderBy and inserts the rows into tx.
func copyTable[T any](ctx context.Context, src *Store, tx bun.Tx, orderBy string) (int, error) {
	total := 0
	for offset := int64(0); ; offset += copyBatch {
		var rows []T
		if err := src.db.NewSelect().Model(&rows).OrderExpr(orderBy).Limit(copyBatch).Offset(offset).Scan(ctx); err != nil {
			return total, err
		}
		if len(rows) == 0 {
			return total, nil
		}
		if _, err := tx.NewInsert().Model(&rows).Returning("NULL").Exec(ctx); err != nil {
			return total, err
		}
		total += len(rows)
		if len(rows) < copyBatch {
			return total, nil
		}
	}
}
