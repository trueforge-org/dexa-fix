package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// ---- single-use values ----

// UseNonce claims nonce until expires. The insert only succeeds, or takes over an expired row, when
// nobody holds it, so two processes racing for the same value cannot both win.
func (s *Store) UseNonce(ctx context.Context, nonce string, expires time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO used_nonces (nonce, expires_at) VALUES (?, ?)
		 ON CONFLICT (nonce) DO UPDATE SET expires_at = EXCLUDED.expires_at WHERE used_nonces.expires_at < ?`,
		nonce, expires.UTC(), now())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) NonceSeen(ctx context.Context, nonce string) (bool, error) {
	n, err := s.db.NewSelect().TableExpr("used_nonces").Where("nonce = ? AND expires_at >= ?", nonce, now()).Count(ctx)
	return n > 0, err
}

func (s *Store) PurgeNonces(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM used_nonces WHERE expires_at < ?`, before.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- failed attempts ----

func (s *Store) RecordFailure(ctx context.Context, key string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_failures (fail_key, failed_at) VALUES (?, ?)`, key, at.UTC())
	return err
}

func (s *Store) CountFailures(ctx context.Context, key string, since time.Time) (int, error) {
	n, err := s.db.NewSelect().TableExpr("auth_failures").Where("fail_key = ? AND failed_at > ?", key, since.UTC()).Count(ctx)
	return int(n), err
}

func (s *Store) ClearFailures(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_failures WHERE fail_key = ?`, key)
	return err
}

func (s *Store) PurgeFailures(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth_failures WHERE failed_at < ?`, before.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- Strength Level sync status ----

func (s *Store) PutStrengthLevelJob(ctx context.Context, userID int64, state string) error {
	row := strengthLevelJobRow{UserID: userID, State: state, UpdatedAt: now()}
	_, err := s.db.NewInsert().Model(&row).
		On("CONFLICT (user_id) DO UPDATE").
		Set("state = EXCLUDED.state").Set("updated_at = EXCLUDED.updated_at").Exec(ctx)
	if isFK(err) {
		return store.ErrNotFound
	}
	return err
}

func (s *Store) GetStrengthLevelJob(ctx context.Context, userID int64) (string, time.Time, error) {
	var row strengthLevelJobRow
	if err := s.db.NewSelect().Model(&row).Where("user_id = ?", userID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", time.Time{}, store.ErrNotFound
		}
		return "", time.Time{}, err
	}
	return row.State, row.UpdatedAt.UTC(), nil
}
