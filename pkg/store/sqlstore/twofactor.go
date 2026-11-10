package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

type totpRow struct {
	bun.BaseModel `bun:"table:user_totp"`

	UserID      int64      `bun:"user_id,pk"`
	Secret      string     `bun:"secret"` // encrypted, context totp:<user id>
	LastStep    int64      `bun:"last_step"`
	ConfirmedAt *time.Time `bun:"confirmed_at"`
	CreatedAt   time.Time  `bun:"created_at"`
}

type recoveryRow struct {
	bun.BaseModel `bun:"table:user_recovery_codes"`

	UserID    int64     `bun:"user_id,pk"`
	CodeHash  string    `bun:"code_hash,pk"`
	CreatedAt time.Time `bun:"created_at"`
}

func totpContext(userID int64) string { return fmt.Sprintf("totp:%d", userID) }

// GetTOTP returns the user's one-time password setup, confirmed or not, or ErrNotFound.
func (s *Store) GetTOTP(ctx context.Context, userID int64) (models.TOTP, error) {
	var row totpRow
	if err := s.db.NewSelect().Model(&row).Where("user_id = ?", userID).Scan(ctx); err != nil {
		return models.TOTP{}, notFound(err)
	}
	secret, err := s.decrypt(row.Secret, totpContext(userID))
	if err != nil {
		return models.TOTP{}, err
	}
	t := models.TOTP{UserID: userID, Secret: secret, LastStep: row.LastStep, CreatedAt: row.CreatedAt}
	if row.ConfirmedAt != nil {
		t.Confirmed, t.ConfirmedAt = true, row.ConfirmedAt.UTC()
	}
	return t, nil
}

// PutPendingTOTP starts (or restarts) the setup with a new secret. The setup counts only after
// ConfirmTOTP, and starting again discards an earlier, unconfirmed secret. A confirmed setup is
// never replaced: it fails with ErrExists.
func (s *Store) PutPendingTOTP(ctx context.Context, userID int64, secret string) error {
	enc := s.encrypt(secret, totpContext(userID))
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var cur totpRow
		err := tx.NewSelect().Model(&cur).Where("user_id = ?", userID).Scan(ctx)
		switch {
		case err == nil && cur.ConfirmedAt != nil:
			return store.ErrExists
		case err == nil:
			_, err = tx.NewUpdate().Model((*totpRow)(nil)).Set("secret = ?", enc).Set("last_step = 0").
				Set("created_at = ?", now()).Where("user_id = ?", userID).Exec(ctx)
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		row := totpRow{UserID: userID, Secret: enc, CreatedAt: now()}
		_, err = tx.NewInsert().Model(&row).Exec(ctx)
		if isFK(err) {
			return store.ErrNotFound
		}
		return err
	})
}

// ConfirmTOTP turns a pending setup into a confirmed one. step is the code step the user proved
// the secret with; it becomes the replay guard. It fails with ErrNotFound without a pending setup.
func (s *Store) ConfirmTOTP(ctx context.Context, userID, step int64) error {
	res, err := s.db.NewUpdate().Model((*totpRow)(nil)).Set("confirmed_at = ?", now()).Set("last_step = ?", step).
		Where("user_id = ?", userID).Where("confirmed_at IS NULL").Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// UseTOTPStep records step as used and reports whether it was fresh. It is one conditional update,
// so of two requests presenting the same code only one wins.
func (s *Store) UseTOTPStep(ctx context.Context, userID, step int64) (bool, error) {
	res, err := s.db.NewUpdate().Model((*totpRow)(nil)).Set("last_step = ?", step).
		Where("user_id = ?", userID).Where("confirmed_at IS NOT NULL").Where("last_step < ?", step).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteTOTP removes the setup and the recovery codes. It succeeds when there is none.
func (s *Store) DeleteTOTP(ctx context.Context, userID int64) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*recoveryRow)(nil)).Where("user_id = ?", userID).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewDelete().Model((*totpRow)(nil)).Where("user_id = ?", userID).Exec(ctx)
		return err
	})
}

// ReplaceRecoveryCodes stores the hashes of a new set of recovery codes and drops the old set.
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID int64, hashes []string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*recoveryRow)(nil)).Where("user_id = ?", userID).Exec(ctx); err != nil {
			return err
		}
		if len(hashes) == 0 {
			return nil
		}
		rows := make([]recoveryRow, 0, len(hashes))
		for _, h := range hashes {
			rows = append(rows, recoveryRow{UserID: userID, CodeHash: h, CreatedAt: now()})
		}
		_, err := tx.NewInsert().Model(&rows).Exec(ctx)
		if isFK(err) {
			return store.ErrNotFound
		}
		return err
	})
}

// UseRecoveryCode deletes the code with this hash and reports whether the user had it.
func (s *Store) UseRecoveryCode(ctx context.Context, userID int64, hash string) (bool, error) {
	res, err := s.db.NewDelete().Model((*recoveryRow)(nil)).Where("user_id = ?", userID).Where("code_hash = ?", hash).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CountRecoveryCodes is the number of recovery codes the user has left.
func (s *Store) CountRecoveryCodes(ctx context.Context, userID int64) (int, error) {
	n, err := s.db.NewSelect().Model((*recoveryRow)(nil)).Where("user_id = ?", userID).Count(ctx)
	return int(n), err
}

// TwoFactorUsers returns the ids of the users who have confirmed two-step verification.
func (s *Store) TwoFactorUsers(ctx context.Context) (map[int64]bool, error) {
	var ids []int64
	if err := s.db.NewSelect().Model((*totpRow)(nil)).Column("user_id").Where("confirmed_at IS NOT NULL").Scan(ctx, &ids); err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}
