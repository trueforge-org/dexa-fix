// Package store defines the persistence interface used by the application.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

var (
	// ErrNotFound is returned when a record does not exist (or belongs to another user).
	ErrNotFound = errors.New("not found")
	// ErrExists is returned when a unique constraint is violated (e.g. duplicate user name).
	ErrExists = errors.New("already exists")
	// ErrInvalid is returned for input that fails validation.
	ErrInvalid = errors.New("invalid input")
	// ErrLastAdmin is returned when a change would leave no administrator.
	ErrLastAdmin = errors.New("cannot remove the last administrator")
)

// Stats summarises the contents of the database.
type Stats struct {
	Driver        string
	Encrypted     bool // sensitive columns are encrypted
	SchemaVersion int64
	Users         int
	Admins        int
	DexaScans     int
	ScaleReadings int
	Sessions      int
	AccessTokens  int
}

// Store persists users, DEXA scans, scale readings and per-user integration data.
type Store interface {
	// CreateUser creates a user without a way to sign in. The first user becomes an administrator.
	CreateUser(ctx context.Context, name string) (models.User, error)
	// CreateUserWithIdentity creates a user and its identity atomically. An empty Subject means
	// the new user's id, and an e-mail address already in use returns ErrExists.
	CreateUserWithIdentity(ctx context.Context, name, email string, id models.Identity) (models.User, error)
	ListUsers(ctx context.Context) ([]models.User, error)
	GetUser(ctx context.Context, id int64) (models.User, error)
	GetUserByEmail(ctx context.Context, email string) (models.User, error)
	// GetUserWithoutEmailByName finds an account from before e-mail sign-in.
	GetUserWithoutEmailByName(ctx context.Context, name string) (models.User, error)
	// UpdateUserProfile stores the name, e-mail address and height (nil clears it). An e-mail
	// address already in use returns ErrExists.
	UpdateUserProfile(ctx context.Context, userID int64, name, email string, heightCm *float64) error
	// SetUserRole changes a role; demoting the last administrator returns ErrLastAdmin.
	SetUserRole(ctx context.Context, userID int64, role string) error
	// DeleteUser removes a user and all their data; removing the last administrator returns ErrLastAdmin.
	DeleteUser(ctx context.Context, userID int64) error
	// UpdateUserSettings stores the athlete-mode default and the method accuracy ranking.
	UpdateUserSettings(ctx context.Context, userID int64, athleteDefault bool, ranking []string) error
	// UpdateUserHiddenColumns stores which daily table columns the user has hidden.
	UpdateUserHiddenColumns(ctx context.Context, userID int64, keys []string) error

	CreateDexaScan(ctx context.Context, s *models.DexaScan) error
	ListDexaScans(ctx context.Context, userID int64) ([]models.DexaScan, error)
	DeleteDexaScan(ctx context.Context, userID, id int64) error

	// UpsertScaleReadings inserts readings; readings with a non-nil ExternalID are
	// updated in place when (user, source, external_id) already exists. An update changes the
	// measured values and notes only, so a reading keeps its method and athlete mode.
	UpsertScaleReadings(ctx context.Context, userID int64, rs []models.ScaleReading) (int, error)
	ListScaleReadings(ctx context.Context, userID int64) ([]models.ScaleReading, error)
	DeleteScaleReading(ctx context.Context, userID, id int64) error

	// GetIntegration returns the provider data (e.g. OAuth tokens) stored for a user, or ErrNotFound.
	GetIntegration(ctx context.Context, userID int64, provider string) ([]byte, error)
	PutIntegration(ctx context.Context, userID int64, provider string, data []byte) error
	// DeleteIntegration removes the provider data; it is not an error if there is none.
	DeleteIntegration(ctx context.Context, userID int64, provider string) error

	// GetIdentity finds an identity by provider and subject.
	GetIdentity(ctx context.Context, provider, subject string) (models.Identity, error)
	// PutIdentity adds an identity, or updates the secret of an existing one.
	PutIdentity(ctx context.Context, id models.Identity) error
	DeleteIdentity(ctx context.Context, userID int64, provider string) error
	// IdentityProviders returns the provider names of every user's identities.
	IdentityProviders(ctx context.Context) (map[int64][]string, error)

	CreateSession(ctx context.Context, tokenHash string, userID int64, method string, expires time.Time) error
	// UserBySession returns the user of an unexpired session, or ErrNotFound.
	UserBySession(ctx context.Context, tokenHash string) (models.User, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteUserSessions(ctx context.Context, userID int64) error

	CreateAccessToken(ctx context.Context, t *models.AccessToken) error
	ListAccessTokens(ctx context.Context, userID int64) ([]models.AccessToken, error)
	DeleteAccessToken(ctx context.Context, userID, id int64) error
	// UserByTokenHash returns the user of an unexpired token and records its use, or ErrNotFound.
	UserByTokenHash(ctx context.Context, tokenHash string) (models.User, error)

	// GetSetting returns a server-wide setting, or ErrNotFound.
	GetSetting(ctx context.Context, name string) (string, error)
	PutSetting(ctx context.Context, name, value string) error
	DeleteSetting(ctx context.Context, name string) error

	ListAuthProviders(ctx context.Context) ([]models.AuthProvider, error)
	GetAuthProvider(ctx context.Context, slug string) (models.AuthProvider, error)
	PutAuthProvider(ctx context.Context, p models.AuthProvider) error
	// DeleteAuthProvider also removes the identities of that provider, so a later provider with
	// the same slug cannot inherit them.
	DeleteAuthProvider(ctx context.Context, slug string) error

	Stats(ctx context.Context) (Stats, error)

	Close() error
}

// ParseDay parses a YYYY-MM-DD date as UTC midnight.
func ParseDay(s string) (time.Time, error) { return time.Parse("2006-01-02", s) }
