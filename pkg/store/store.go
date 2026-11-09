// Package store defines the persistence interface used by the application.
package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
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

	KeyID       string    // id of the configured encryption key (empty when encryption is off)
	KeySince    time.Time // when this key was introduced, at least (zero when unknown)
	KeyPrevious bool      // the database still belongs to a previous key: `db rekey` is due
}

// Cursor marks a position in a listing that is sorted newest first, by measurement time and id.
// A page is continued with the cursor the previous page returned, which stays correct when
// readings are added or deleted in between (unlike an offset).
type Cursor struct {
	MeasuredAt time.Time
	ID         int64
}

// String is the form used in URLs and API responses.
func (c Cursor) String() string {
	return strconv.FormatInt(c.MeasuredAt.UnixNano(), 10) + "." + strconv.FormatInt(c.ID, 10)
}

// ParseCursor reverses Cursor.String.
func ParseCursor(s string) (Cursor, error) {
	ts, id, ok := strings.Cut(s, ".")
	if !ok {
		return Cursor{}, ErrInvalid
	}
	n, err1 := strconv.ParseInt(ts, 10, 64)
	i, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil || i < 0 {
		return Cursor{}, ErrInvalid
	}
	return Cursor{MeasuredAt: time.Unix(0, n).UTC(), ID: i}, nil
}

// ListFilter selects a window of scans or readings, newest first.
type ListFilter struct {
	Since, Until time.Time // inclusive bounds on the measurement time; the zero time means no bound
	After        *Cursor   // continue after this position
	Limit        int       // at most this many; 0 means all of them
}

// AuditFilter selects audit events. The zero value lists the latest ones.
type AuditFilter struct {
	UserID   int64 // only events this user did or that concern them; 0 for all
	BeforeID int64 // only events older than this id, for paging; 0 for the newest
	Limit    int   // at most this many; 0 means 100
}

// Store persists users, DEXA scans, scale readings and per-user integration data.
type Store interface {
	// CreateUser creates a user without a way to sign in. The first user becomes an administrator.
	CreateUser(ctx context.Context, name string) (models.User, error)
	// CreateUserWithIdentity creates a user and its identity atomically. An empty Subject means
	// the new user's id, and an e-mail address already in use returns ErrExists.
	CreateUserWithIdentity(ctx context.Context, name, email string, id models.Identity) (models.User, error)
	ListUsers(ctx context.Context) ([]models.User, error)
	// HasUsers reports whether any user exists, without loading or decrypting them.
	HasUsers(ctx context.Context) (bool, error)
	GetUser(ctx context.Context, id int64) (models.User, error)
	GetUserByEmail(ctx context.Context, email string) (models.User, error)
	// UpdateUserProfile stores the name, e-mail address and height (nil clears it). An e-mail
	// address already in use returns ErrExists.
	UpdateUserProfile(ctx context.Context, userID int64, name, email string, heightCm *float64) error
	// UpdateUserTimezone sets the user's IANA time zone. An unknown name is ErrInvalid.
	UpdateUserTimezone(ctx context.Context, userID int64, timezone string) error
	// SetUserRole changes a role; demoting the last administrator returns ErrLastAdmin.
	SetUserRole(ctx context.Context, userID int64, role string) error
	// DeleteUser removes a user and all their data; removing the last administrator returns ErrLastAdmin.
	DeleteUser(ctx context.Context, userID int64) error
	// UpdateUserSettings stores the athlete-mode default and the method accuracy ranking.
	UpdateUserSettings(ctx context.Context, userID int64, athleteDefault bool, ranking []string) error
	// SetUserStrengthLevelAuto lets an administrator allow or forbid Strength Level sync. Forbidding
	// it also clears the multi-day permission and deletes the user's stored Strength Level login.
	SetUserStrengthLevelAuto(ctx context.Context, userID int64, enabled bool) error
	// SetUserStrengthLevelBulk allows or forbids the multi-day sync on the settings page. It only
	// has an effect for users who may sync at all.
	SetUserStrengthLevelBulk(ctx context.Context, userID int64, enabled bool) error
	// UpdateUserHiddenColumns stores which daily table columns the user has hidden.
	UpdateUserHiddenColumns(ctx context.Context, userID int64, keys []string) error

	CreateDexaScan(ctx context.Context, s *models.DexaScan) error
	ListDexaScans(ctx context.Context, userID int64) ([]models.DexaScan, error)
	// ListDexaScansPage lists one page. The cursor is non-nil when there are older scans.
	ListDexaScansPage(ctx context.Context, userID int64, f ListFilter) ([]models.DexaScan, *Cursor, error)
	DeleteDexaScan(ctx context.Context, userID, id int64) error

	// UpsertScaleReadings inserts readings; readings with a non-nil ExternalID are
	// updated in place when (user, source, external_id) already exists. An update changes the
	// measured values and notes only, so a reading keeps its method and athlete mode.
	UpsertScaleReadings(ctx context.Context, userID int64, rs []models.ScaleReading) (int, error)
	ListScaleReadings(ctx context.Context, userID int64) ([]models.ScaleReading, error)
	// ListScaleReadingsPage lists one page. The cursor is non-nil when there are older readings.
	ListScaleReadingsPage(ctx context.Context, userID int64, f ListFilter) ([]models.ScaleReading, *Cursor, error)
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
	// ListUserIdentities returns one user's identities, oldest first. Secret is always empty.
	ListUserIdentities(ctx context.Context, userID int64) ([]models.Identity, error)
	// UserProviders returns the provider names of one user's identities, in name order.
	UserProviders(ctx context.Context, userID int64) ([]string, error)
	// IdentityProviders returns the provider names of every user's identities.
	IdentityProviders(ctx context.Context) (map[int64][]string, error)

	CreateSession(ctx context.Context, tokenHash string, userID int64, method string, expires time.Time) error
	// UserBySession returns the user of an unexpired session, or ErrNotFound.
	UserBySession(ctx context.Context, tokenHash string) (models.User, error)
	// GetSession returns an unexpired session, or ErrNotFound.
	GetSession(ctx context.Context, tokenHash string) (models.Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteUserSessions(ctx context.Context, userID int64) error

	CreateAccessToken(ctx context.Context, t *models.AccessToken) error
	ListAccessTokens(ctx context.Context, userID int64) ([]models.AccessToken, error)
	DeleteAccessToken(ctx context.Context, userID, id int64) error
	// DeleteUserAccessTokens revokes every access token of a user.
	DeleteUserAccessTokens(ctx context.Context, userID int64) error
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

	// AddAuditEvent records an event; ID and At are set.
	AddAuditEvent(ctx context.Context, e *models.AuditEvent) error
	// ListAuditEvents returns events newest first.
	ListAuditEvents(ctx context.Context, f AuditFilter) ([]models.AuditEvent, error)
	// PurgeAuditEvents deletes events older than the cutoff and returns how many.
	PurgeAuditEvents(ctx context.Context, before time.Time) (int64, error)

	Stats(ctx context.Context) (Stats, error)

	Close() error
}

// ParseDay parses a YYYY-MM-DD date as UTC midnight.
func ParseDay(s string) (time.Time, error) { return time.Parse("2006-01-02", s) }
