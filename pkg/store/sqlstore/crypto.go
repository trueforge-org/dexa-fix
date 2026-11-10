package sqlstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// encPrefix marks an encrypted value of any version; values without it are rows from before
// encryption was enabled. Two formats exist:
//
//	enc:v1:<payload>        AES-256-GCM under the master key itself (still readable, no key id)
//	enc:v2:<kid>:<payload>  AES-256-GCM under a key derived from the master key with HKDF; kid
//	                        names the master key, so values from before a rotation can be told apart
//
// Payloads are base64 (nonce followed by ciphertext).
const (
	encPrefix = "enc:"
	encV1     = "enc:v1:"
	encV2     = "enc:v2:"
)

// KeySize is the length of an encryption key in bytes (AES-256).
const KeySize = 32

// checkSetting is the app_settings row holding the key check value. It is not encrypted.
const checkSetting = "crypto.check"

// keyEntry is one master key with everything derived from it.
type keyEntry struct {
	id    string      // first 8 hex characters of SHA-256(master key)
	v1    cipher.AEAD // AES under the master key itself, for enc:v1 values
	v2    cipher.AEAD // AES under an HKDF derived key, for enc:v2 values
	index []byte      // separate key for the e-mail lookup values
	check string      // check value, see checkSetting
	body  *keyEntry   // the body domain's key, derived from this master key (see Cipher.bodyCipher)
}

// The HKDF labels. Each domain derives its own key from a master key, so a value sealed for one
// domain never opens in another, even when a context string happens to repeat.
const (
	mainInfo       = "dexa-fix aes-256-gcm v2"
	bodyMasterInfo = "dexa-fix body master"
	bodyInfo       = "dexa-fix body aes-256-gcm v2"
)

// newKeyEntry derives everything a master key is used for: the main domain's keys, and the key of
// the body domain (scans and readings).
func newKeyEntry(key []byte) (*keyEntry, error) {
	e, err := newKeyEntryInfo(key, mainInfo)
	if err != nil {
		return nil, err
	}
	bodyKey, err := hkdf.Key(sha256.New, key, nil, bodyMasterInfo, KeySize)
	if err != nil {
		return nil, err
	}
	if e.body, err = newKeyEntryInfo(bodyKey, bodyInfo); err != nil {
		return nil, err
	}
	return e, nil
}

// newKeyEntryInfo builds an entry whose v2 key is derived with the given HKDF info string.
func newKeyEntryInfo(key []byte, dekInfo string) (*keyEntry, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("encryption key must be %d bytes, got %d", KeySize, len(key))
	}
	gcm := func(k []byte) (cipher.AEAD, error) {
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	}
	derive := func(info string) ([]byte, error) { return hkdf.Key(sha256.New, key, nil, info, KeySize) }

	e := &keyEntry{}
	var err error
	if e.v1, err = gcm(key); err != nil {
		return nil, err
	}
	dek, err := derive(dekInfo)
	if err != nil {
		return nil, err
	}
	if e.v2, err = gcm(dek); err != nil {
		return nil, err
	}
	if e.index, err = derive("dexa-fix e-mail lookup"); err != nil {
		return nil, err
	}
	checkKey, err := derive("dexa-fix key check")
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, checkKey)
	m.Write([]byte("dexa-fix key check v1"))
	e.check = hex.EncodeToString(m.Sum(nil))
	id := sha256.Sum256(key)
	e.id = hex.EncodeToString(id[:])[:8]
	return e, nil
}

// Cipher encrypts sensitive columns with AES-256-GCM. It seals with the current key and opens
// values made by the current key or by any previous one.
type Cipher struct {
	current  *keyEntry
	previous []*keyEntry
}

// NewCipher returns a Cipher for a key of KeySize bytes. Previous keys only serve to read values
// that were encrypted before a rotation (see Store.Rekey).
func NewCipher(key []byte, previous ...[]byte) (*Cipher, error) {
	cur, err := newKeyEntry(key)
	if err != nil {
		return nil, err
	}
	c := &Cipher{current: cur}
	for _, p := range previous {
		e, err := newKeyEntry(p)
		if err != nil {
			return nil, fmt.Errorf("previous key: %w", err)
		}
		if c.entry(e.id) == nil {
			c.previous = append(c.previous, e)
		}
	}
	return c, nil
}

// bodyCipher is the cipher of the body domain: scans and readings. Its keys derive from the master
// keys (current and previous), so one key file covers both domains and a rotation reaches both.
func (c *Cipher) bodyCipher() *Cipher {
	b := &Cipher{current: c.current.body}
	for _, e := range c.previous {
		b.previous = append(b.previous, e.body)
	}
	return b
}

// entry returns the configured key with the given id, or nil.
func (c *Cipher) entry(id string) *keyEntry {
	if c.current.id == id {
		return c.current
	}
	for _, e := range c.previous {
		if e.id == id {
			return e
		}
	}
	return nil
}

// all lists the current key first, then the previous ones.
func (c *Cipher) all() []*keyEntry { return append([]*keyEntry{c.current}, c.previous...) }

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

// seal encrypts a value with the current key. The context ties the ciphertext to its row, so that
// copying it into another row, such as another user's tokens, makes decryption fail.
func (c *Cipher) seal(value, context string) (string, error) {
	aead := c.current.v2
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(value), []byte(context))
	return encV2 + c.current.id + ":" + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// open decrypts a value and reports which key made it. A v2 value names its key; a v1 value does
// not, so the current key is tried first, then each previous one.
func (c *Cipher) open(value, context string) (string, *keyEntry, error) {
	if rest, ok := strings.CutPrefix(value, encV2); ok {
		kid, payload, ok := strings.Cut(rest, ":")
		if !ok {
			return "", nil, errors.New("malformed encrypted value")
		}
		e := c.entry(kid)
		if e == nil {
			return "", nil, fmt.Errorf("a stored value was encrypted with key %s, which is not configured: add that key to DEXAFIX_ENCRYPTION_KEY_PREVIOUS", kid)
		}
		plain, err := openGCM(e.v2, payload, context)
		return plain, e, err
	}
	payload, ok := strings.CutPrefix(value, encV1)
	if !ok {
		return "", nil, errors.New("unknown encrypted value format")
	}
	var err error
	for _, e := range c.all() {
		var plain string
		if plain, err = openGCM(e.v1, payload, context); err == nil {
			return plain, e, nil
		}
	}
	return "", nil, err
}

func openGCM(aead cipher.AEAD, payload, context string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(payload)
	if err != nil {
		return "", err
	}
	n := aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("ciphertext too short")
	}
	plain, err := aead.Open(nil, raw[:n], raw[n:], []byte(context))
	if err != nil {
		return "", fmt.Errorf("decrypt stored value (is the encryption key correct?): %w", err)
	}
	return string(plain), nil
}

// matchCheck returns the configured key that produced a stored check value, or nil.
func (c *Cipher) matchCheck(stored string) *keyEntry {
	for _, e := range c.all() {
		if hmac.Equal([]byte(e.check), []byte(stored)) {
			return e
		}
	}
	return nil
}

// emailLookup returns a keyed hash of a normalized address. Encrypted addresses differ every time,
// so this is what makes them searchable and unique without storing them readably. The hash key
// derives from the current key, so a rotation has to recompute it (Store.Rekey does).
func (s *Store) emailLookup(email string) *string {
	if email == "" {
		return nil
	}
	var sum []byte
	if s.cipher != nil {
		m := hmac.New(sha256.New, s.cipher.current.index)
		m.Write([]byte(email))
		sum = m.Sum(nil)
	} else {
		h := sha256.Sum256([]byte(email))
		sum = h[:]
	}
	v := hex.EncodeToString(sum)
	return &v
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
	plain, _, err := s.cipher.open(value, context)
	return plain, err
}

// sealWith and openWith handle the values of a domain that has its own cipher (see bodyCipher). A
// nil cipher leaves the value as it is, like encrypt and decrypt do for the main domain.
func sealWith(c *Cipher, value, context string) (string, error) {
	if c == nil || value == "" {
		return value, nil
	}
	return c.seal(value, context)
}

func openWith(c *Cipher, value, context string) (string, error) {
	if !strings.HasPrefix(value, encPrefix) {
		return value, nil
	}
	if c == nil {
		return "", errors.New("the database holds encrypted values but no encryption key is configured")
	}
	plain, _, err := c.open(value, context)
	return plain, err
}

func integrationContext(userID int64, provider string) string {
	return fmt.Sprintf("integration:%d:%s", userID, provider)
}
func settingContext(name string) string { return "setting:" + name }
func scaleContext(userID int64, rowUID string) string {
	return fmt.Sprintf("scale:%d:%s", userID, rowUID)
}
func dexaContext(userID int64, rowUID string) string {
	return fmt.Sprintf("dexa:%d:%s", userID, rowUID)
}
func providerContext(slug string) string { return "provider:" + slug }
func emailContext(userID int64) string   { return fmt.Sprintf("email:%d", userID) }

// userModel converts a row and decrypts its e-mail address.
func (s *Store) userModel(r *userRow) (models.User, error) {
	u := r.toModel()
	var err error
	u.Email, err = s.decrypt(r.Email, emailContext(r.ID))
	return u, err
}

// errWrongKey explains a key that does not match the database.
const errWrongKey = "the encryption key does not match this database: the key file is missing, the wrong file, " +
	"or DEXAFIX_ENCRYPTION_KEY differs from the key that created the database. Restore the original key; " +
	"to switch to a new key keep the old one in DEXAFIX_ENCRYPTION_KEY_PREVIOUS and run `dexa-fix db rekey`"

// queryer is what bun.DB and bun.Tx share.
type queryer interface {
	NewSelect() *bun.SelectQuery
}

// firstEncrypted finds one encrypted value of the main database and the context that seals it.
// found is false when the database holds none. The key check row is not a ciphertext and is
// skipped. (The body domain has its own: firstEncryptedBody.)
func firstEncrypted(ctx context.Context, db queryer) (value, sealCtx string, found bool, err error) {
	var (
		integ []integrationRow
		sets  []settingRow
		provs []providerRow
		users []userRow
		totps []totpRow
	)
	if err = db.NewSelect().Model(&sets).Where("value LIKE ?", encPrefix+"%").Where("name <> ?", checkSetting).Limit(1).Scan(ctx); err != nil {
		return
	}
	if len(sets) > 0 {
		return sets[0].Value, settingContext(sets[0].Name), true, nil
	}
	if err = db.NewSelect().Model(&integ).Where("data LIKE ?", encPrefix+"%").Limit(1).Scan(ctx); err != nil {
		return
	}
	if len(integ) > 0 {
		return integ[0].Data, integrationContext(integ[0].UserID, integ[0].Provider), true, nil
	}
	if err = db.NewSelect().Model(&users).Where("email LIKE ?", encPrefix+"%").Limit(1).Scan(ctx); err != nil {
		return
	}
	if len(users) > 0 {
		return users[0].Email, emailContext(users[0].ID), true, nil
	}
	if err = db.NewSelect().Model(&totps).Where("secret LIKE ?", encPrefix+"%").Limit(1).Scan(ctx); err != nil {
		return
	}
	if len(totps) > 0 {
		return totps[0].Secret, totpContext(totps[0].UserID), true, nil
	}
	if err = db.NewSelect().Model(&provs).Where("client_secret LIKE ?", encPrefix+"%").Limit(1).Scan(ctx); err != nil {
		return
	}
	if len(provs) > 0 {
		return provs[0].ClientSecret, providerContext(provs[0].Slug), true, nil
	}
	return "", "", false, nil
}

// HasEncryptedValues reports whether the database already holds encrypted values, without
// migrating it. A database that does not exist yet, or has no tables, holds none. The serve command
// uses it to avoid inventing a new key for data that depends on an old one.
func HasEncryptedValues(ctx context.Context, driver, dsn string) (bool, error) {
	s, err := Open(ctx, driver, dsn, WithoutMigrate())
	if err != nil {
		return false, err
	}
	defer s.Close()
	_, _, found, err := firstEncrypted(ctx, s.db)
	if err != nil && !isMissingTable(err) {
		return false, err
	}
	if found {
		return true, nil
	}
	if s.BodyStore == nil {
		return false, nil
	}
	return s.BodyStore.HasEncryptedValues(ctx)
}

// HasEncryptedBodyValues is HasEncryptedValues for a database that holds only the body domain.
func HasEncryptedBodyValues(ctx context.Context, driver, dsn string) (bool, error) {
	b, err := OpenBody(ctx, driver, dsn, WithoutMigrate())
	if err != nil {
		return false, err
	}
	defer b.Close()
	return b.HasEncryptedValues(ctx)
}

func isMissingTable(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") || strings.Contains(msg, "does not exist")
}

// verifyKey makes sure the configured key belongs to this database. The check value in
// app_settings is written on first use, and only when no encrypted value exists that could belong
// to another key. A database that predates the check is verified by opening one of its values.
//
// It only decides: a check that has to be written is returned as write, and the caller runs it once
// every domain has accepted the key, so a wrong key leaves no check behind in any of them.
func (s *Store) verifyKey(ctx context.Context) (write func() error, err error) {
	var row settingRow
	err = s.db.NewSelect().Model(&row).Where("name = ?", checkSetting).Scan(ctx)
	switch {
	case err == nil:
		e := s.cipher.matchCheck(row.Value)
		if e == nil {
			return nil, errors.New(errWrongKey)
		}
		if e != s.cipher.current {
			log.Warn().Str("key", e.id).Msg("the database is still encrypted with a previous key: run `dexa-fix db rekey`")
		}
		return nil, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	value, vctx, found, err := firstEncrypted(ctx, s.db)
	if err != nil {
		return nil, err
	}
	if found {
		_, e, err := s.cipher.open(value, vctx)
		if err != nil {
			return nil, fmt.Errorf("%s (%w)", errWrongKey, err)
		}
		if e != s.cipher.current {
			log.Warn().Str("key", e.id).Msg("the database is still encrypted with a previous key: run `dexa-fix db rekey`")
			return nil, nil
		}
	}
	return func() error { return s.writeCheck(ctx, s.db) }, nil
}

type execer interface {
	NewInsert() *bun.InsertQuery
}

func (s *Store) writeCheck(ctx context.Context, db execer) error {
	row := settingRow{Name: checkSetting, Value: s.cipher.current.check, UpdatedAt: now()}
	_, err := db.NewInsert().Model(&row).
		On("CONFLICT (name) DO UPDATE").
		Set("value = EXCLUDED.value").
		// The date marks when this key was introduced: writing the same check again keeps it.
		Set("updated_at = CASE WHEN setting_row.value = EXCLUDED.value THEN setting_row.updated_at ELSE EXCLUDED.updated_at END").
		Exec(ctx)
	return err
}

// keyInfo reports which key the database check belongs to and since when, for the admin page.
func (s *Store) keyInfo(ctx context.Context, st *store.Stats) error {
	if s.cipher == nil {
		return nil
	}
	st.KeyID = s.cipher.current.id
	var row settingRow
	err := s.db.NewSelect().Model(&row).Where("name = ?", checkSetting).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if e := s.cipher.matchCheck(row.Value); e != nil {
		st.KeyPrevious = e != s.cipher.current
		if !st.KeyPrevious {
			st.KeySince = row.UpdatedAt
		}
	}
	return nil
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
			if r.Name == checkSetting || !needs(r.Value) {
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

		var totps []totpRow
		if err := tx.NewSelect().Model(&totps).Scan(ctx); err != nil {
			return err
		}
		for _, r := range totps {
			if !needs(r.Secret) {
				continue
			}
			enc, err := s.cipher.seal(r.Secret, totpContext(r.UserID))
			if err != nil {
				return err
			}
			if _, err := tx.NewUpdate().Model((*totpRow)(nil)).Set("secret = ?", enc).Where("user_id = ?", r.UserID).Exec(ctx); err != nil {
				return err
			}
		}

		var jobs []jobRow
		if err := tx.NewSelect().Model(&jobs).Scan(ctx); err != nil {
			return err
		}
		for _, r := range jobs {
			if needs(r.Payload) {
				enc, err := s.cipher.seal(r.Payload, jobPayloadContext(r.PublicID))
				if err != nil {
					return err
				}
				if _, err := tx.NewUpdate().Model((*jobRow)(nil)).Set("payload = ?", enc).Where("id = ?", r.ID).Exec(ctx); err != nil {
					return err
				}
			}
			if needs(r.Result) {
				enc, err := s.cipher.seal(r.Result, jobResultContext(r.PublicID))
				if err != nil {
					return err
				}
				if _, err := tx.NewUpdate().Model((*jobRow)(nil)).Set("result = ?", enc).Where("id = ?", r.ID).Exec(ctx); err != nil {
					return err
				}
			}
		}

		return s.backfillEmails(ctx, tx)
	})
}

// backfillEmails encrypts e-mail addresses and fills in their lookup values. Addresses were not
// unique before they became the sign-in key, so a repeated one is kept only on the oldest account;
// the others are asked for an address at their next sign-in.
func (s *Store) backfillEmails(ctx context.Context, tx bun.Tx) error {
	var users []userRow
	if err := tx.NewSelect().Model(&users).OrderExpr("created_at, id").Scan(ctx); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range users {
		email, err := s.decrypt(r.Email, emailContext(r.ID))
		if err != nil {
			return err
		}
		email = models.NormalizeEmail(email)
		if email == "" {
			continue
		}
		lookup := s.emailLookup(email)
		enc := r.Email
		switch {
		case seen[*lookup]:
			log.Warn().Int64("user", r.ID).Msg("e-mail address already used by an older account: cleared")
			enc, lookup = "", nil
		case !strings.HasPrefix(r.Email, encPrefix):
			if enc, err = s.cipher.seal(email, emailContext(r.ID)); err != nil {
				return err
			}
		}
		if lookup != nil {
			seen[*lookup] = true
			if r.EmailLookup != nil && *r.EmailLookup == *lookup && enc == r.Email {
				continue
			}
		}
		if _, err := tx.NewUpdate().Model((*userRow)(nil)).Set("email = ?", enc).Set("email_lookup = ?", lookup).Where("id = ?", r.ID).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}
