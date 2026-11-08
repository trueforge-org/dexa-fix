// Package importer defines how external services (e.g. Withings) feed scale readings into the store.
package importer

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/store"
)

var (
	// ErrNotImplemented is returned by importers that are registered but not functional yet.
	ErrNotImplemented = errors.New("not implemented")
	// ErrNotConfigured is returned when the provider has no credentials configured on the server.
	ErrNotConfigured = errors.New("not configured")
	// ErrNotConnected is returned when the user has not linked their account with the provider.
	ErrNotConnected = errors.New("not connected")
)

// Options selects what an import fetches.
type Options struct {
	// Since limits a bulk import to measurements taken at or after this time (zero: everything).
	Since time.Time
	// Day, when non-zero, re-imports exactly this UTC day. It replaces the provider's readings for
	// that day and also imports on days where manually entered readings exist.
	Day time.Time
}

// Result reports what an import did.
type Result struct {
	Imported    int      `json:"imported"`               // new readings stored
	Updated     int      `json:"updated"`                // existing readings whose values changed
	Unchanged   int      `json:"unchanged"`              // readings already stored with identical values
	Removed     int      `json:"removed"`                // stored readings the provider no longer returns (day re-import only)
	Skipped     int      `json:"skipped"`                // days left alone because they hold manual readings
	SkippedDays []string `json:"skipped_days,omitempty"` // those days as YYYY-MM-DD
}

// Importer imports measurements for a single user from an external provider.
// Implementations write through store.Store.UpsertScaleReadings with a stable Source and
// ExternalID so that repeated imports are idempotent.
type Importer interface {
	// Name is the provider name used in URLs and as the readings' Source, e.g. "withings".
	Name() string
	// Import fetches measurements for the user according to o and stores them.
	Import(ctx context.Context, st store.Store, userID int64, o Options) (Result, error)
}

// Credentials are a provider's server-wide application settings.
type Credentials struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// Configurable is implemented by providers whose credentials can be changed while running.
type Configurable interface {
	// BaseCredentials returns the credentials from the server configuration.
	BaseCredentials() Credentials
	// SetCredentials applies overrides; an empty field falls back to the server configuration.
	SetCredentials(Credentials)
}

// OAuthProvider is implemented by importers that need the user to authorise access first.
type OAuthProvider interface {
	// Configured reports whether the server has the credentials to talk to the provider.
	Configured() bool
	// Connected reports whether the user has linked their account.
	Connected(ctx context.Context, st store.Store, userID int64) bool
	// AuthURL is where the user is sent to grant access; state is echoed back on the callback.
	AuthURL(state string) string
	// Connect exchanges the authorisation code from the callback and stores the user's tokens.
	Connect(ctx context.Context, st store.Store, userID int64, code string) error
	// Disconnect forgets the user's tokens.
	Disconnect(ctx context.Context, st store.Store, userID int64) error
}

// Registry holds the known importers by name.
type Registry struct {
	byName map[string]Importer
}

// NewRegistry returns a registry containing the given importers.
func NewRegistry(imps ...Importer) *Registry {
	r := &Registry{byName: map[string]Importer{}}
	for _, i := range imps {
		r.byName[i.Name()] = i
	}
	return r
}

// Get returns the importer registered under name.
func (r *Registry) Get(name string) (Importer, bool) {
	if r == nil {
		return nil, false
	}
	i, ok := r.byName[name]
	return i, ok
}

// Names lists the registered provider names in alphabetical order.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
