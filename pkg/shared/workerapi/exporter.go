package workerapi

// Exporter: where estimates go. The user's own login at the service is kept by the backend and
// handed to a worker inside a lease; the estimates are computed by the backend at that moment, so a
// worker never sees more of a user's data than the job needs.
//
//	<provider>.check_login  CheckLoginPayload - sign in once to see whether the login works. A wrong
//	                                            login is an answer, not a failure: the result says so.
//	<provider>.day          ExportPayload     - send one day, adding it or overwriting the entry there.
//	<provider>.sync         ExportPayload     - send many days; progress goes with the heartbeats.

// Actions of an exporter, the part of a kind after the provider's name.
const (
	ActionCheckLogin = "check_login"
	ActionDay        = "day"
	ActionSync       = "sync"
)

// Login is a user's login at an outside service.
type Login struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// CheckLoginPayload is the payload of <provider>.check_login.
type CheckLoginPayload struct {
	Login Login `json:"login"`
}

// CheckLoginResult says whether the service accepted the login. Message is for the person.
type CheckLoginResult struct {
	LoginOK bool   `json:"login_ok"`
	Message string `json:"message,omitempty"`
}

// ExportEntry is the body weight and body fat of one calendar day.
type ExportEntry struct {
	Day      string   `json:"day"` // YYYY-MM-DD
	WeightKg float64  `json:"weight_kg"`
	FatPct   *float64 `json:"fat_pct,omitempty"` // nil when the day has no body fat estimate
}

// ExportPayload is the payload of <provider>.day and <provider>.sync.
type ExportPayload struct {
	Login Login `json:"login"`
	// Entries are the days to send, newest first.
	Entries []ExportEntry `json:"entries"`
	// Since limits a sync to entries from this day on (YYYY-MM-DD; empty: all of them).
	Since string `json:"since,omitempty"`
	// Update also changes days that exist at the service with other values. Without it they are
	// left as they are. Nothing is ever deleted.
	Update bool `json:"update,omitempty"`
}

// ExportProgress is what a sync reports with its heartbeats.
type ExportProgress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// ExportResult is what an export did.
type ExportResult struct {
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"` // already at the service with the same values (or left alone)
	Failed    int `json:"failed"`    // days that could not be written
	// FailedMessages are a few of the reasons, for the log.
	FailedMessages []string `json:"failed_messages,omitempty"`
}
