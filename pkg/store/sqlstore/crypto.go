package sqlstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// encPrefix marks an encrypted value. The version leaves room for another format later, and values
// without the prefix are rows from before encryption was enabled.
const encPrefix = "enc:v1:"

// KeySize is the length of an encryption key in bytes (AES-256).
const KeySize = 32

// Cipher encrypts sensitive columns with AES-256-GCM.
type Cipher struct{ aead cipher.AEAD }

// NewCipher returns a Cipher for a key of KeySize bytes.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("encryption key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// ParseKey decodes a key written as 64 hexadecimal characters or as base64.
func ParseKey(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if key, err := hex.DecodeString(text); err == nil && len(key) == KeySize {
		return key, nil
	}
	if key, err := base64.StdEncoding.DecodeString(text); err == nil && len(key) == KeySize {
		return key, nil
	}
	return nil, fmt.Errorf("encryption key must be %d bytes, written as hexadecimal or base64", KeySize)
}

// WithCipher encrypts sensitive columns: provider secrets, integration tokens, server settings and
// e-mail addresses. Values stored before it was enabled are encrypted when the store opens.
func WithCipher(c *Cipher) Option { return func(o *openConfig) { o.cipher = c } }

// seal encrypts a value. The context ties the ciphertext to its row, so that copying it into
// another row, such as another user's tokens, makes decryption fail.
func (c *Cipher) seal(value, context string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(value), []byte(context))
	return encPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

func (c *Cipher) open(value, context string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, encPrefix))
	if err != nil {
		return "", err
	}
	n := c.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("ciphertext too short")
	}
	plain, err := c.aead.Open(nil, raw[:n], raw[n:], []byte(context))
	if err != nil {
		return "", fmt.Errorf("decrypt stored value (is the encryption key correct?): %w", err)
	}
	return string(plain), nil
}

func (s *Store) encrypt(value, context string) (string, error) {
	if s.cipher == nil || value == "" {
		return value, nil
	}
	return s.cipher.seal(value, context)
}

func (s *Store) decrypt(value, context string) (string, error) {
	if !strings.HasPrefix(value, encPrefix) {
		return value, nil
	}
	if s.cipher == nil {
		return "", errors.New("the database holds encrypted values but no encryption key is configured")
	}
	return s.cipher.open(value, context)
}

func integrationContext(userID int64, provider string) string {
	return fmt.Sprintf("integration:%d:%s", userID, provider)
}
func settingContext(name string) string  { return "setting:" + name }
func providerContext(slug string) string { return "provider:" + slug }
func emailContext(userID int64) string   { return fmt.Sprintf("email:%d", userID) }

// userModel converts a row and decrypts its e-mail address.
func (s *Store) userModel(r *userRow) (models.User, error) {
	u := r.toModel()
	var err error
	u.Email, err = s.decrypt(r.Email, emailContext(r.ID))
	return u, err
}

// encryptExisting encrypts values stored before encryption was enabled. It runs in one transaction
// and skips values that are already encrypted, so it is safe on every start.
func (s *Store) encryptExisting(ctx context.Context) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		needs := func(v string) bool { return v != "" && !strings.HasPrefix(v, encPrefix) }

		var integrations []integrationRow
		if err := tx.NewSelect().Model(&integrations).Scan(ctx); err != nil {
			return err
		}
		for _, r := range integrations {
			if !needs(r.Data) {
				continue
			}
			enc, err := s.cipher.seal(r.Data, integrationContext(r.UserID, r.Provider))
			if err != nil {
				return err
			}
			if _, err := tx.NewUpdate().Model((*integrationRow)(nil)).Set("data = ?", enc).
				Where("user_id = ?", r.UserID).Where("provider = ?", r.Provider).Exec(ctx); err != nil {
				return err
			}
		}

		var settings []settingRow
		if err := tx.NewSelect().Model(&settings).Scan(ctx); err != nil {
			return err
		}
		for _, r := range settings {
			if !needs(r.Value) {
				continue
			}
			enc, err := s.cipher.seal(r.Value, settingContext(r.Name))
			if err != nil {
				return err
			}
			if _, err := tx.NewUpdate().Model((*settingRow)(nil)).Set("value = ?", enc).Where("name = ?", r.Name).Exec(ctx); err != nil {
				return err
			}
		}

		var providers []providerRow
		if err := tx.NewSelect().Model(&providers).Scan(ctx); err != nil {
			return err
		}
		for _, r := range providers {
			if !needs(r.ClientSecret) {
				continue
			}
			enc, err := s.cipher.seal(r.ClientSecret, providerContext(r.Slug))
			if err != nil {
				return err
			}
			if _, err := tx.NewUpdate().Model((*providerRow)(nil)).Set("client_secret = ?", enc).Where("slug = ?", r.Slug).Exec(ctx); err != nil {
				return err
			}
		}

		var users []userRow
		if err := tx.NewSelect().Model(&users).Scan(ctx); err != nil {
			return err
		}
		for _, r := range users {
			if !needs(r.Email) {
				continue
			}
			enc, err := s.cipher.seal(r.Email, emailContext(r.ID))
			if err != nil {
				return err
			}
			if _, err := tx.NewUpdate().Model((*userRow)(nil)).Set("email = ?", enc).Where("id = ?", r.ID).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}
