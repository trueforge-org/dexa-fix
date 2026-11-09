package sqlstore

import (
	"context"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

type resetRow struct {
	bun.BaseModel `bun:"table:password_resets"`

	TokenHash string    `bun:"token_hash,pk"`
	UserID    int64     `bun:"user_id"`
	CreatedBy *int64    `bun:"created_by"`
	CreatedAt time.Time `bun:"created_at"`
	ExpiresAt time.Time `bun:"expires_at"`
}

// CreatePasswordReset stores a reset link for the user. A user has at most one live link: the
// others, and every expired link, are removed first. createdBy is the administrator who made it,
// nil when the user asked for it.
func (s *Store) CreatePasswordReset(ctx context.Context, tokenHash string, userID int64, createdBy *int64, expires time.Time) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*resetRow)(nil)).Where("expires_at < ? OR user_id = ?", now(), userID).Exec(ctx); err != nil {
			return err
		}
		row := resetRow{TokenHash: tokenHash, UserID: userID, CreatedBy: createdBy, CreatedAt: now(), ExpiresAt: expires.UTC()}
		_, err := tx.NewInsert().Model(&row).Exec(ctx)
		if isFK(err) {
			return store.ErrNotFound
		}
		return err
	})
}

// PasswordResetUser returns the user a live reset link belongs to, or ErrNotFound.
func (s *Store) PasswordResetUser(ctx context.Context, tokenHash string) (models.User, error) {
	var row userRow
	err := s.db.NewSelect().Model(&row).
		Where("id = (SELECT user_id FROM password_resets WHERE token_hash = ? AND expires_at > ?)", tokenHash, now()).
		Scan(ctx)
	if err != nil {
		return models.User{}, notFound(err)
	}
	return s.userModel(&row)
}

// CompletePasswordReset uses a live link: in one transaction it deletes the user's links, sets the
// password and signs the user out everywhere (sessions and access tokens), so a link can be used
// once and a changed password never leaves old sessions alive. It returns the user id, or
// ErrNotFound when the link is unknown or expired.
func (s *Store) CompletePasswordReset(ctx context.Context, tokenHash, passwordHash string) (int64, error) {
	var userID int64
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var row resetRow
		if err := tx.NewSelect().Model(&row).Where("token_hash = ?", tokenHash).Where("expires_at > ?", now()).Scan(ctx); err != nil {
			return notFound(err)
		}
		// The delete decides who wins when two requests present the same link.
		res, err := tx.NewDelete().Model((*resetRow)(nil)).Where("user_id = ?", row.UserID).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrNotFound
		}
		id := identityRow{Provider: models.ProviderLocal, Subject: strconv.FormatInt(row.UserID, 10), UserID: row.UserID, Secret: passwordHash, CreatedAt: now()}
		if _, err := tx.NewInsert().Model(&id).On("CONFLICT (provider, subject) DO UPDATE").Set("secret = EXCLUDED.secret").Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*sessionRow)(nil)).Where("user_id = ?", row.UserID).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*tokenRow)(nil)).Where("user_id = ?", row.UserID).Exec(ctx); err != nil {
			return err
		}
		userID = row.UserID
		return nil
	})
	return userID, err
}

// DeletePasswordResets removes the user's outstanding reset links, for example after the password
// or the e-mail address changed.
func (s *Store) DeletePasswordResets(ctx context.Context, userID int64) error {
	_, err := s.db.NewDelete().Model((*resetRow)(nil)).Where("user_id = ?", userID).Exec(ctx)
	return err
}
