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
)

// Store persists users, DEXA scans, scale readings and per-user integration data.
type Store interface {
	CreateUser(ctx context.Context, name string) (models.User, error)
	ListUsers(ctx context.Context) ([]models.User, error)
	GetUser(ctx context.Context, id int64) (models.User, error)

	CreateDexaScan(ctx context.Context, s *models.DexaScan) error
	ListDexaScans(ctx context.Context, userID int64) ([]models.DexaScan, error)
	DeleteDexaScan(ctx context.Context, userID, id int64) error

	// UpsertScaleReadings inserts readings; readings with a non-nil ExternalID are
	// updated in place when (user, source, external_id) already exists.
	UpsertScaleReadings(ctx context.Context, userID int64, rs []models.ScaleReading) (int, error)
	ListScaleReadings(ctx context.Context, userID int64) ([]models.ScaleReading, error)
	DeleteScaleReading(ctx context.Context, userID, id int64) error

	// GetIntegration returns the provider data (e.g. OAuth tokens) stored for a user, or ErrNotFound.
	GetIntegration(ctx context.Context, userID int64, provider string) ([]byte, error)
	PutIntegration(ctx context.Context, userID int64, provider string, data []byte) error
	// DeleteIntegration removes the provider data; it is not an error if there is none.
	DeleteIntegration(ctx context.Context, userID int64, provider string) error

	Close() error
}

// ParseDay parses a YYYY-MM-DD date as UTC midnight.
func ParseDay(s string) (time.Time, error) { return time.Parse("2006-01-02", s) }
