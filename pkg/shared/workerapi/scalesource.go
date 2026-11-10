package workerapi

import (
	"encoding/json"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// Roles. A provider does not get its own protocol: it plays a role, and the role fixes what a job
// of a kind carries. A new source of scale readings (Garmin, Fitbit, ...) is a package under
// pkg/workers that handles the kinds below for its own name, plus a descriptor in the backend; the
// payloads, the results and the way the backend applies them do not change.
//
// Scale source: where measurements come from.
//
//	<provider>.connect  ConnectPayload   - exchange an authorisation code for credentials; the worker
//	                                       hands them over with SaveCredentials, the result is empty.
//	<provider>.import   ImportPayload    - fetch measurements; the result is an ImportResult.
//
// Exporter: where estimates go (see ExportPayload in exporter.go).

// Action names, the part of a kind after the provider's name.
const (
	ActionConnect = "connect"
	ActionImport  = "import"
)

// OAuthApp is the application's own registration with a provider (set by an administrator).
type OAuthApp struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURL  string `json:"redirect_url"`
}

// ConnectPayload is the payload of <provider>.connect.
type ConnectPayload struct {
	App  OAuthApp `json:"app"`
	Code string   `json:"code"`
}

// ImportPayload is the payload of <provider>.import.
type ImportPayload struct {
	App OAuthApp `json:"app"`
	// Credentials are what the user's connection stored, as the provider's worker wrote them (an
	// OAuth token pair for example). They are opaque to the backend. A worker that renews them hands
	// the new ones over with SaveCredentials before it uses them.
	Credentials json.RawMessage `json:"credentials"`
	// Since limits a bulk import to measurements at or after this time (zero: everything).
	Since time.Time `json:"since,omitempty"`
	// Day, when set (YYYY-MM-DD), asks for exactly that calendar day in the user's time zone Zone.
	Day string `json:"day,omitempty"`
	// Zone is the user's IANA time zone; it decides where a calendar day begins and ends.
	Zone string `json:"zone,omitempty"`
}

// ImportResult is what an import found. The backend decides what to store: it compares Readings
// with the readings the user already has (and the manual ones it must not disturb).
type ImportResult struct {
	// Readings are the measurements, with Source set to the provider's name and ExternalID to the
	// provider's id of the measurement, which makes repeated imports idempotent.
	Readings []models.ScaleReading `json:"readings"`
	// Seen are the ids of every measurement the provider returned, including the ones left out of
	// Readings (a guest on the scale), so a day import never deletes a reading only because it is
	// filtered out now.
	Seen []string `json:"seen,omitempty"`
}
