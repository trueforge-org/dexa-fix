package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// tokenTouchInterval limits how often a token's last-used time is written.
const tokenTouchInterval = 5 * time.Minute

// moreThanOneAdmin guards updates and deletes that could remove the last administrator.
const moreThanOneAdmin = `(role <> 'admin' OR (SELECT COUNT(*) FROM users WHERE role = 'admin') > 1)`

type identityRow struct {
	bun.BaseModel `bun:"table:user_identities"`

	Provider  string    `bun:"provider,pk"`
	Subject   string    `bun:"subject,pk"`
	UserID    int64     `bun:"user_id"`
	Secret    string    `bun:"secret"`
	CreatedAt time.Time `bun:"created_at"`
}

type sessionRow struct {
	bun.BaseModel `bun:"table:sessions"`

	TokenHash string    `bun:"token_hash,pk"`
	UserID    int64     `bun:"user_id"`
	Method    string    `bun:"method"`
	CreatedAt time.Time `bun:"created_at"`
	ExpiresAt time.Time `bun:"expires_at"`
}

type tokenRow struct {
	bun.BaseModel `bun:"table:access_tokens"`

	ID         int64      `bun:"id,pk,autoincrement"`
	UserID     int64      `bun:"user_id"`
	Name       string     `bun:"name"`
	Prefix     string     `bun:"prefix"`
	TokenHash  string     `bun:"token_hash"`
	CreatedAt  time.Time  `bun:"created_at"`
	LastUsedAt *time.Time `bun:"last_used_at"`
	ExpiresAt  *time.Time `bun:"expires_at"`
}

type settingRow struct {
	bun.BaseModel `bun:"table:app_settings"`

	Name      string    `bun:"name,pk"`
	Value     string    `bun:"value"`
	UpdatedAt time.Time `bun:"updated_at"`
}

type providerRow struct {
	bun.BaseModel `bun:"table:auth_providers"`

	Slug         string    `bun:"slug,pk"`
	Label        string    `bun:"label"`
	Kind         string    `bun:"kind"`
	Issuer       string    `bun:"issuer"`
	AuthURL      string    `bun:"auth_url"`
	TokenURL     string    `bun:"token_url"`
	UserinfoURL  string    `bun:"userinfo_url"`
	ClientID     string    `bun:"client_id"`
	ClientSecret string    `bun:"client_secret"`
	AppleTeamID  string    `bun:"apple_team_id"`
	AppleKeyID   string    `bun:"apple_key_id"`
	Scopes       string    `bun:"scopes"`
	SubjectClaim string    `bun:"subject_claim"`
	NameClaim    string    `bun:"name_claim"`
	AllowSignup  bool      `bun:"allow_signup"`
	CreatedAt    time.Time `bun:"created_at"`
}

func (r providerRow) toModel() models.AuthProvider {
	return models.AuthProvider{Slug: r.Slug, Label: r.Label, Kind: r.Kind, Issuer: r.Issuer, AuthURL: r.AuthURL, TokenURL: r.TokenURL,
		UserinfoURL: r.UserinfoURL, ClientID: r.ClientID, ClientSecret: r.ClientSecret, AppleTeamID: r.AppleTeamID, AppleKeyID: r.AppleKeyID,
		Scopes: r.Scopes, SubjectClaim: r.SubjectClaim, NameClaim: r.NameClaim, AllowSignup: r.AllowSignup, CreatedAt: r.CreatedAt}
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

// ---- users ----

func (s *Store) GetUserByEmail(ctx context.Context, email string) (models.User, error) {
	var row userRow
	if err := s.db.NewSelect().Model(&row).Where("email_lookup = ?", s.emailLookup(models.NormalizeEmail(email))).Scan(ctx); err != nil {
		return models.User{}, notFound(err)
	}
	return s.userModel(&row)
}

func (s *Store) UpdateUserProfile(ctx context.Context, userID int64, name, email string, heightCm *float64) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return fmt.Errorf("%w: name must be 1-64 characters", store.ErrInvalid)
	}
	email = models.NormalizeEmail(email)
	enc, err := s.encrypt(email, emailContext(userID))
	if err != nil {
		return err
	}
	res, err := s.db.NewUpdate().Model((*userRow)(nil)).
		Set("name = ?", name).Set("email = ?", enc).Set("email_lookup = ?", s.emailLookup(email)).Set("height_cm = ?", heightCm).
		Set("data_version = data_version + 1").
		Where("id = ?", userID).Exec(ctx)
	if err != nil {
		if isUnique(err) {
			return store.ErrExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// guarded runs a statement that affects no row when the guard fails, and tells a missing user
// from a refused change afterwards.
func (s *Store) guarded(ctx context.Context, userID int64, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	if _, err := s.GetUser(ctx, userID); err != nil {
		return err
	}
	return store.ErrLastAdmin
}

func (s *Store) SetUserRole(ctx context.Context, userID int64, role string) error {
	if role != models.RoleUser && role != models.RoleAdmin {
		return store.ErrInvalid
	}
	if role == models.RoleAdmin {
		return s.guarded(ctx, userID, `UPDATE users SET role = ? WHERE id = ?`, role, userID)
	}
	return s.guarded(ctx, userID, `UPDATE users SET role = ? WHERE id = ? AND `+moreThanOneAdmin, role, userID)
}

// DeleteUser removes the user and, with them, the IP addresses in the audit events about them. The
// events themselves stay, without a reference to the person (ON DELETE SET NULL).
func (s *Store) DeleteUser(ctx context.Context, userID int64) error {
	refused := errors.New("delete refused")
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE audit_log SET ip = '' WHERE actor_id = ? OR target_id = ?`, userID, userID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ? AND `+moreThanOneAdmin, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return refused // roll back the IP clearing too
		}
		return nil
	})
	if errors.Is(err, refused) {
		if _, err := s.GetUser(ctx, userID); err != nil {
			return err
		}
		return store.ErrLastAdmin
	}
	return err
}

// ---- identities ----

func (s *Store) GetIdentity(ctx context.Context, provider, subject string) (models.Identity, error) {
	var r identityRow
	if err := s.db.NewSelect().Model(&r).Where("provider = ?", provider).Where("subject = ?", subject).Scan(ctx); err != nil {
		return models.Identity{}, notFound(err)
	}
	return models.Identity{UserID: r.UserID, Provider: r.Provider, Subject: r.Subject, Secret: r.Secret, CreatedAt: r.CreatedAt}, nil
}

func (s *Store) ListUserIdentities(ctx context.Context, userID int64) ([]models.Identity, error) {
	var rows []identityRow
	if err := s.db.NewSelect().Model(&rows).Where("user_id = ?", userID).OrderExpr("created_at, provider").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]models.Identity, 0, len(rows))
	for _, r := range rows {
		out = append(out, models.Identity{UserID: r.UserID, Provider: r.Provider, Subject: r.Subject, CreatedAt: r.CreatedAt}) // the secret stays behind
	}
	return out, nil
}

func (s *Store) PutIdentity(ctx context.Context, id models.Identity) error {
	row := identityRow{Provider: id.Provider, Subject: id.Subject, UserID: id.UserID, Secret: id.Secret, CreatedAt: now()}
	_, err := s.db.NewInsert().Model(&row).
		On("CONFLICT (provider, subject) DO UPDATE").
		Set("secret = EXCLUDED.secret").Exec(ctx)
	if isFK(err) {
		return store.ErrNotFound
	}
	return err
}

func (s *Store) DeleteIdentity(ctx context.Context, userID int64, provider string) error {
	_, err := s.db.NewDelete().Model((*identityRow)(nil)).Where("user_id = ?", userID).Where("provider = ?", provider).Exec(ctx)
	return err
}

func (s *Store) IdentityProviders(ctx context.Context) (map[int64][]string, error) {
	var rows []identityRow
	if err := s.db.NewSelect().Model(&rows).OrderExpr("provider").Scan(ctx); err != nil {
		return nil, err
	}
	out := map[int64][]string{}
	for _, r := range rows {
		out[r.UserID] = append(out[r.UserID], r.Provider)
	}
	return out, nil
}

// ---- sessions ----

func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, method string, expires time.Time) error {
	if _, err := s.db.NewDelete().Model((*sessionRow)(nil)).Where("expires_at < ?", now()).Exec(ctx); err != nil {
		return err
	}
	row := sessionRow{TokenHash: tokenHash, UserID: userID, Method: method, CreatedAt: now(), ExpiresAt: expires.UTC()}
	_, err := s.db.NewInsert().Model(&row).Exec(ctx)
	return err
}

func (s *Store) UserBySession(ctx context.Context, tokenHash string) (models.User, error) {
	var row userRow
	err := s.db.NewSelect().Model(&row).
		Where("id = (SELECT user_id FROM sessions WHERE token_hash = ? AND expires_at > ?)", tokenHash, now()).
		Scan(ctx)
	if err != nil {
		return models.User{}, notFound(err)
	}
	return s.userModel(&row)
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.NewDelete().Model((*sessionRow)(nil)).Where("token_hash = ?", tokenHash).Exec(ctx)
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.db.NewDelete().Model((*sessionRow)(nil)).Where("user_id = ?", userID).Exec(ctx)
	return err
}

// ---- access tokens ----

func (s *Store) CreateAccessToken(ctx context.Context, t *models.AccessToken) error {
	t.CreatedAt = now()
	row := tokenRow{UserID: t.UserID, Name: t.Name, Prefix: t.Prefix, TokenHash: t.Hash, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt}
	if _, err := s.db.NewInsert().Model(&row).Exec(ctx); err != nil {
		if isFK(err) {
			return store.ErrNotFound
		}
		return err
	}
	t.ID = row.ID
	return nil
}

func (s *Store) ListAccessTokens(ctx context.Context, userID int64) ([]models.AccessToken, error) {
	var rows []tokenRow
	if err := s.db.NewSelect().Model(&rows).Where("user_id = ?", userID).OrderExpr("created_at DESC, id DESC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]models.AccessToken, 0, len(rows))
	for _, r := range rows {
		out = append(out, models.AccessToken{ID: r.ID, UserID: r.UserID, Name: r.Name, Prefix: r.Prefix, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt})
	}
	return out, nil
}

func (s *Store) DeleteAccessToken(ctx context.Context, userID, id int64) error {
	res, err := s.db.NewDelete().Model((*tokenRow)(nil)).Where("id = ?", id).Where("user_id = ?", userID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) UserByTokenHash(ctx context.Context, tokenHash string) (models.User, error) {
	var tok tokenRow
	t := now()
	err := s.db.NewSelect().Model(&tok).Where("token_hash = ?", tokenHash).
		Where("expires_at IS NULL OR expires_at > ?", t).Scan(ctx)
	if err != nil {
		return models.User{}, notFound(err)
	}
	if _, err := s.db.NewUpdate().Model((*tokenRow)(nil)).Set("last_used_at = ?", t).Where("id = ?", tok.ID).
		Where("last_used_at IS NULL OR last_used_at < ?", t.Add(-tokenTouchInterval)).Exec(ctx); err != nil {
		return models.User{}, err
	}
	return s.GetUser(ctx, tok.UserID)
}

// ---- settings ----

func (s *Store) GetSetting(ctx context.Context, name string) (string, error) {
	var row settingRow
	if err := s.db.NewSelect().Model(&row).Where("name = ?", name).Scan(ctx); err != nil {
		return "", notFound(err)
	}
	return s.decrypt(row.Value, settingContext(name))
}

func (s *Store) PutSetting(ctx context.Context, name, value string) error {
	enc, err := s.encrypt(value, settingContext(name))
	if err != nil {
		return err
	}
	row := settingRow{Name: name, Value: enc, UpdatedAt: now()}
	_, err = s.db.NewInsert().Model(&row).
		On("CONFLICT (name) DO UPDATE").
		Set("value = EXCLUDED.value").Set("updated_at = EXCLUDED.updated_at").Exec(ctx)
	return err
}

func (s *Store) DeleteSetting(ctx context.Context, name string) error {
	_, err := s.db.NewDelete().Model((*settingRow)(nil)).Where("name = ?", name).Exec(ctx)
	return err
}

// ---- providers ----

func (s *Store) ListAuthProviders(ctx context.Context) ([]models.AuthProvider, error) {
	var rows []providerRow
	if err := s.db.NewSelect().Model(&rows).OrderExpr("label").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]models.AuthProvider, 0, len(rows))
	for _, r := range rows {
		p, err := s.providerModel(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Store) GetAuthProvider(ctx context.Context, slug string) (models.AuthProvider, error) {
	var row providerRow
	if err := s.db.NewSelect().Model(&row).Where("slug = ?", slug).Scan(ctx); err != nil {
		return models.AuthProvider{}, notFound(err)
	}
	return s.providerModel(row)
}

func (s *Store) providerModel(r providerRow) (models.AuthProvider, error) {
	p := r.toModel()
	var err error
	p.ClientSecret, err = s.decrypt(r.ClientSecret, providerContext(r.Slug))
	return p, err
}

func (s *Store) PutAuthProvider(ctx context.Context, p models.AuthProvider) error {
	var err error
	if p.ClientSecret, err = s.encrypt(p.ClientSecret, providerContext(p.Slug)); err != nil {
		return err
	}
	row := providerRow{Slug: p.Slug, Label: p.Label, Kind: p.Kind, Issuer: p.Issuer, AuthURL: p.AuthURL, TokenURL: p.TokenURL,
		UserinfoURL: p.UserinfoURL, ClientID: p.ClientID, ClientSecret: p.ClientSecret, AppleTeamID: p.AppleTeamID, AppleKeyID: p.AppleKeyID,
		Scopes: p.Scopes, SubjectClaim: p.SubjectClaim, NameClaim: p.NameClaim, AllowSignup: p.AllowSignup, CreatedAt: now()}
	q := s.db.NewInsert().Model(&row).On("CONFLICT (slug) DO UPDATE")
	for _, col := range []string{"label", "kind", "issuer", "auth_url", "token_url", "userinfo_url", "client_id", "client_secret",
		"apple_team_id", "apple_key_id", "scopes", "subject_claim", "name_claim", "allow_signup"} {
		q = q.Set(col + " = EXCLUDED." + col)
	}
	_, err = q.Exec(ctx)
	return err
}

func (s *Store) DeleteAuthProvider(ctx context.Context, slug string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*identityRow)(nil)).Where("provider = ?", slug).Exec(ctx); err != nil {
			return err
		}
		res, err := tx.NewDelete().Model((*providerRow)(nil)).Where("slug = ?", slug).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrNotFound
		}
		return nil
	})
}

// ---- stats ----

// Stats counts the main tables.
func (s *Store) Stats(ctx context.Context) (store.Stats, error) {
	st := store.Stats{Driver: s.driver, Encrypted: s.cipher != nil}
	var err error
	if st.SchemaVersion, err = s.SchemaVersion(ctx); err != nil {
		return st, err
	}
	counts := []struct {
		dst   *int
		model any
		where string
	}{
		{&st.Users, (*userRow)(nil), ""},
		{&st.Admins, (*userRow)(nil), "role = 'admin'"},
		{&st.DexaScans, (*dexaRow)(nil), ""},
		{&st.ScaleReadings, (*scaleRow)(nil), ""},
		{&st.AccessTokens, (*tokenRow)(nil), ""},
	}
	for _, c := range counts {
		q := s.db.NewSelect().Model(c.model)
		if c.where != "" {
			q = q.Where(c.where)
		}
		n, err := q.Count(ctx)
		if err != nil {
			return st, err
		}
		*c.dst = int(n)
	}
	n, err := s.db.NewSelect().Model((*sessionRow)(nil)).Where("expires_at > ?", now()).Count(ctx)
	st.Sessions = int(n)
	return st, err
}
