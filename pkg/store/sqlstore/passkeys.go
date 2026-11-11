package sqlstore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"time"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

type passkeyRow struct {
	bun.BaseModel `bun:"table:passkeys"`

	CredentialID   string     `bun:"credential_id,pk"`
	UserID         int64      `bun:"user_id"`
	Name           string     `bun:"name"`
	Credential     string     `bun:"credential"`
	SignCount      int64      `bun:"sign_count"`
	BackupEligible bool       `bun:"backup_eligible"`
	BackupState    bool       `bun:"backup_state"`
	CreatedAt      time.Time  `bun:"created_at"`
	LastUsedAt     *time.Time `bun:"last_used_at"`
}

// EnsurePasskeyHandle returns the random user handle that identifies the user to their passkeys,
// creating it the first time. It is 64 random bytes and says nothing about the person.
func (s *Store) EnsurePasskeyHandle(ctx context.Context, userID int64) (string, error) {
	var row userRow
	if err := s.db.NewSelect().Model(&row).Column("id", "passkey_handle").Where("id = ?", userID).Scan(ctx); err != nil {
		return "", notFound(err)
	}
	if row.PasskeyHandle != nil && *row.PasskeyHandle != "" {
		return *row.PasskeyHandle, nil
	}
	b := make([]byte, 64)
	_, _ = rand.Read(b) // never fails: since Go 1.24 a broken random source ends the program
	handle := base64.RawURLEncoding.EncodeToString(b)
	// Only set it when it is still empty, so two requests agree on one value.
	if _, err := s.db.NewUpdate().Model((*userRow)(nil)).Set("passkey_handle = ?", handle).
		Where("id = ?", userID).Where("passkey_handle IS NULL OR passkey_handle = ''").Exec(ctx); err != nil {
		return "", err
	}
	if err := s.db.NewSelect().Model(&row).Column("id", "passkey_handle").Where("id = ?", userID).Scan(ctx); err != nil {
		return "", notFound(err)
	}
	return *row.PasskeyHandle, nil
}

func (s *Store) UserByPasskeyHandle(ctx context.Context, handle string) (models.User, error) {
	if handle == "" {
		return models.User{}, store.ErrNotFound
	}
	var row userRow
	if err := s.db.NewSelect().Model(&row).Where("passkey_handle = ?", handle).Scan(ctx); err != nil {
		return models.User{}, notFound(err)
	}
	return s.userModel(&row)
}

func (r *passkeyRow) toModel() models.Passkey {
	p := models.Passkey{CredentialID: r.CredentialID, UserID: r.UserID, Name: r.Name, Credential: r.Credential,
		SignCount: uint32(r.SignCount), CreatedAt: r.CreatedAt}
	if r.LastUsedAt != nil {
		t := r.LastUsedAt.UTC()
		p.LastUsedAt = &t
	}
	return p
}

func (s *Store) ListPasskeys(ctx context.Context, userID int64) ([]models.Passkey, error) {
	var rows []passkeyRow
	if err := s.db.NewSelect().Model(&rows).Where("user_id = ?", userID).OrderExpr("created_at, credential_id").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]models.Passkey, len(rows))
	for i := range rows {
		out[i] = rows[i].toModel()
	}
	return out, nil
}

// AddPasskey stores a new credential. A credential id belongs to one user: a second registration of
// the same authenticator credential fails with ErrExists.
func (s *Store) AddPasskey(ctx context.Context, p models.Passkey) error {
	row := passkeyRow{CredentialID: p.CredentialID, UserID: p.UserID, Name: p.Name, Credential: p.Credential,
		SignCount: int64(p.SignCount), CreatedAt: now()}
	_, err := s.db.NewInsert().Model(&row).Exec(ctx)
	switch {
	case isFK(err):
		return store.ErrNotFound
	case isUnique(err):
		return store.ErrExists
	}
	return err
}

// UpdatePasskeyUse records a sign-in: the new counter, the refreshed credential JSON (flags such as
// backup state can change) and the time.
func (s *Store) UpdatePasskeyUse(ctx context.Context, credentialID, credential string, signCount uint32) error {
	res, err := s.db.NewUpdate().Model((*passkeyRow)(nil)).Set("credential = ?", credential).
		Set("sign_count = ?", int64(signCount)).Set("last_used_at = ?", now()).
		Where("credential_id = ?", credentialID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) RenamePasskey(ctx context.Context, userID int64, credentialID, name string) error {
	res, err := s.db.NewUpdate().Model((*passkeyRow)(nil)).Set("name = ?", name).
		Where("credential_id = ?", credentialID).Where("user_id = ?", userID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeletePasskey(ctx context.Context, userID int64, credentialID string) error {
	res, err := s.db.NewDelete().Model((*passkeyRow)(nil)).Where("credential_id = ?", credentialID).Where("user_id = ?", userID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}
