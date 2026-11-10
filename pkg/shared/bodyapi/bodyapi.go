// Package bodyapi is the contract between the main backend and the body service, the part that
// holds a user's scale readings and DEXA scans and computes their estimates.
//
// Every call carries two headers. X-Dexafix-Body is the body secret that the two services share.
// X-Dexafix-Body-Token is a short-lived token that the main backend signs with an Ed25519 key for
// one user and one scope; the body service only holds the public key, so it can check tokens but
// never make one. The token also carries the settings of the user's row that the estimates depend
// on, so the body service does not have to trust a request body for them.
//
// The package uses nothing but the standard library and pkg/models and pkg/estimate (pure data and
// maths); it imports neither a backend nor the store.
package bodyapi

import (
	"net/http"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
)

const (
	// Prefix is the path every endpoint lives under.
	Prefix = "/body/v1"
	// HeaderSecret carries the secret the main backend and the body service share.
	HeaderSecret = "X-Dexafix-Body"
	// HeaderToken carries the signed token for the user and scope of one call.
	HeaderToken = "X-Dexafix-Body-Token"
	// Audience is what a token must name, so a token made for another service is not accepted.
	Audience = "DexaFix body"
	// MaxTokenAge is the longest a token may live. The signer sets it; the verifier refuses more.
	MaxTokenAge = 60 * time.Second
)

// Scope says what a token allows. Each scope includes the ones before it.
type Scope string

const (
	// ScopeRead reads one user's data.
	ScopeRead Scope = "read"
	// ScopeWrite also changes one user's data.
	ScopeWrite Scope = "write"
	// ScopeAdmin also deletes a user's data and, with subject 0, reads totals and lists users.
	ScopeAdmin Scope = "admin"
)

func (s Scope) rank() int {
	switch s {
	case ScopeRead:
		return 1
	case ScopeWrite:
		return 2
	case ScopeAdmin:
		return 3
	}
	return 0
}

// Allows reports whether a token of scope s may do what need asks for.
func (s Scope) Allows(need Scope) bool { return s.rank() > 0 && s.rank() >= need.rank() }

// EstimateParams are the settings of a user's row that the estimates and the import of readings
// depend on. The main backend puts them in the token; the body service uses nothing else.
type EstimateParams struct {
	Timezone       string   `json:"timezone"`
	Ranking        []string `json:"ranking"`
	AthleteDefault bool     `json:"athlete_default"`
	HeightCm       *float64 `json:"height_cm,omitempty"`
}

// Location is the user's time zone; an empty or unknown name is UTC, as for a user.
func (p EstimateParams) Location() *time.Location {
	return models.User{Timezone: p.Timezone}.Location()
}

// Subject is who a call is for: the user's id and the settings the call may need.
type Subject struct {
	ID     int64
	Params EstimateParams
}

// Page selects a window of scans or readings, newest first. The zero value means all of them.
type Page struct {
	Limit        int       // at most this many; 0 means all
	Before       string    // continue after the cursor an earlier page returned
	Since, Until time.Time // inclusive bounds on the measurement time; zero means no bound
}

// ReconcileRequest is what a source (Withings, ...) returned for a user. The body service decides
// what to store (see the rules on plan in pkg/backend/body).
type ReconcileRequest struct {
	Source  string                `json:"source"`
	Fetched []models.ScaleReading `json:"fetched"`
	Seen    []string              `json:"seen"`
	DayMode bool                  `json:"day_mode"`
	Day     string                `json:"day,omitempty"` // YYYY-MM-DD, set when DayMode is
}

// ImportResult reports what an import did.
type ImportResult struct {
	Imported    int      `json:"imported"`               // new readings stored
	Updated     int      `json:"updated"`                // existing readings whose values changed
	Unchanged   int      `json:"unchanged"`              // readings already stored with identical values
	Removed     int      `json:"removed"`                // stored readings the source no longer returns (day re-import only)
	Skipped     int      `json:"skipped"`                // days left alone because they hold manual readings
	SkippedDays []string `json:"skipped_days,omitempty"` // those days as YYYY-MM-DD
}

// Export is all of a user's scans and readings.
type Export struct {
	DexaScans     []models.DexaScan     `json:"dexa_scans"`
	ScaleReadings []models.ScaleReading `json:"scale_readings"`
}

// Stats counts what the body service holds.
type Stats struct {
	DexaScans     int `json:"dexa_scans"`
	ScaleReadings int `json:"scale_readings"`
}

// Error codes in an error answer.
const (
	CodeNotFound     = "not_found"
	CodeInvalid      = "invalid"
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodeUnavailable  = "unavailable"
	CodeInternal     = "internal"
)

// Error is an error answer. Compare with errors.Is against the Err* values below.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Is lets errors.Is match any *Error with the same code to the sentinels below.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code && t.Message == ""
}

// Sentinels for errors.Is.
var (
	ErrNotFound     = &Error{Code: CodeNotFound}
	ErrInvalid      = &Error{Code: CodeInvalid}
	ErrUnauthorized = &Error{Code: CodeUnauthorized}
	ErrForbidden    = &Error{Code: CodeForbidden}
	ErrUnavailable  = &Error{Code: CodeUnavailable}
)

// StatusFor is the HTTP status that goes with a code.
func StatusFor(code string) int {
	switch code {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeInvalid:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}
