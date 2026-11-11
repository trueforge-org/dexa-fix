package mainbackend

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// Imports from a source of scale readings are jobs: the backend queues one, a worker fetches, and
// the backend stores the result (see workerprovider_withings.go and reconcile.go). A person who
// pressed "Import" waits a little for the answer; the public API waits as long as the caller asked.

var (
	// importWait is how long a page waits for a job it queued before it says the job goes on in
	// the background.
	importWait = 20 * time.Second
	// connectExpiry: the authorisation code of an OAuth connection is short-lived, so a connect
	// job that no worker picked up by then is dropped instead of run with a dead code.
	connectExpiry = 25 * time.Second
)

const (
	defaultAPIWait = 60
	maxAPIWait     = 120
)

// parseOptions reads the optional since (RFC 3339) and day (YYYY-MM-DD) values.
func parseOptions(since, day string) (importPayload, error) {
	var o importPayload
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return o, errors.New("since must be an RFC 3339 timestamp")
		}
		o.Since = t.UTC()
	}
	if day != "" {
		t, err := time.Parse(dateLayout, day)
		if err != nil {
			return o, errors.New("day must be a date like 2026-03-01")
		}
		o.Day = t.Format(dateLayout)
	}
	return o, nil
}

// summary renders an import result for the flash message.
func summary(provider string, res importResult, day string) string {
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(res.Imported, "imported")
	add(res.Updated, "updated")
	add(res.Removed, "removed")
	add(res.Unchanged, "unchanged")
	if res.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d day(s) skipped because they have manual readings (%s)", res.Skipped, strings.Join(res.SkippedDays, ", ")))
	}
	if len(parts) == 0 {
		parts = append(parts, "nothing found")
	}
	scope := ""
	if day != "" {
		scope = " for " + day
	}
	return provider + scope + ": " + strings.Join(parts, ", ")
}

// importProvider returns the provider a path names if it can import.
func (s *Backend) importProvider(name string) (*workerProvider, bool) {
	p, ok := s.providerNamed(name)
	if !ok || p.kind(workerapi.ActionImport) == nil {
		return nil, false
	}
	return p, true
}

// importBlocker says why a user cannot import from the provider now, or returns "".
func (s *Backend) importBlocker(r *http.Request, p *workerProvider, userID int64) (status int, msg string) {
	if p.OAuth == nil {
		return 0, ""
	}
	if ok, _ := p.OAuth.Configured(r.Context(), s); !ok {
		return http.StatusNotImplemented, p.Label + " is not configured on this server."
	}
	if !s.connected(r.Context(), p, userID) {
		return http.StatusConflict, p.Label + " is not connected."
	}
	return 0, ""
}

// queueImport queues an import. interactive says that somebody waits for the answer, which needs a
// running worker.
func (s *Backend) queueImport(r *http.Request, p *workerProvider, userID int64, in importPayload, interactive bool) (models.Job, error) {
	origin := models.JobOriginUser
	if strings.HasPrefix(r.URL.Path, "/api/") {
		origin = models.JobOriginAPI
	}
	job, _, err := s.enqueueJob(r.Context(), enqueueSpec{
		UserID: userID, Kind: p.kind(workerapi.ActionImport).Kind, Origin: origin, Interactive: interactive, Payload: in,
	})
	if err == nil {
		s.audit(r, userID, auditImport, p.Name)
	}
	return job, err
}

// queueFailure turns a refusal to queue into a sentence for the person.
func queueFailure(label string, err error) (string, bool) {
	switch {
	case errors.Is(err, errKindPaused):
		return label + " is paused by an administrator.", true
	case errors.Is(err, errNoWorker):
		return "No " + label + " worker is running, so this cannot start now. Ask an administrator.", true
	}
	return "", false
}

// importMessage describes how an import ended, for the flash message.
func (s *Backend) importMessage(p *workerProvider, day string, j models.Job) string {
	switch j.State {
	case models.JobSucceeded:
		var res importResult
		_ = json.Unmarshal([]byte(j.Result), &res) // a result that does not parse never got here: applying it failed the job
		return summary(p.Label, res, day)
	case models.JobCanceled:
		return p.Label + " import was canceled."
	case models.JobFailed, models.JobExpired:
		log.Error().Str("provider", p.Name).Str("job", j.PublicID).Str("error", j.Error).Msg("import failed")
		return p.Label + " import failed. Try again later; the details are in the server log."
	default:
		return p.Label + " import is running in the background. Reload in a moment to see the new readings."
	}
}

// apiJob is a job as the public API shows it.
type apiJob struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	State       string          `json:"state"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	Error       string          `json:"error,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
}

func jobForAPI(j models.Job) apiJob {
	v := apiJob{ID: j.PublicID, Kind: j.Kind, State: j.State, Attempts: j.Attempts, MaxAttempts: j.MaxAttempts,
		Error: j.Error, CreatedAt: j.CreatedAt, FinishedAt: j.FinishedAt}
	if j.State == models.JobSucceeded && json.Valid([]byte(j.Result)) && j.Result != "" {
		v.Result = json.RawMessage(j.Result)
	}
	return v
}

// integrationAPIRoutes registers the public API for imports and their jobs; the backend serves it.
func (s *Backend) integrationAPIRoutes(mux *http.ServeMux) {
	// POST /api/v1/users/{id}/import/{provider}?since=<RFC 3339>&day=<YYYY-MM-DD>&wait=<seconds>
	//
	// The import runs in a worker. The call waits up to wait seconds (default 60, at most 120) for it
	// and answers 200 with the result, or 202 with the job when it is still running. wait=0 queues the
	// job and answers 202 at once.
	mux.HandleFunc("POST /api/v1/users/{id}/import/{provider}", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		p, ok := s.importProvider(r.PathValue("provider"))
		if !ok {
			apiError(w, store.ErrNotFound)
			return
		}
		in, err := parseOptions(r.URL.Query().Get("since"), r.URL.Query().Get("day"))
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		wait := defaultAPIWait
		if v := r.URL.Query().Get("wait"); v != "" {
			if wait, err = strconv.Atoi(v); err != nil || wait < 0 || wait > maxAPIWait {
				badRequest(w, fmt.Sprintf("wait must be a number of seconds from 0 to %d", maxAPIWait))
				return
			}
		}
		if status, msg := s.importBlocker(r, p, u.ID); status != 0 {
			writeJSON(w, status, map[string]string{"error": strings.ToLower(strings.TrimSuffix(msg, "."))})
			return
		}
		job, err := s.queueImport(r, p, u.ID, in, wait > 0)
		if msg, known := queueFailure(p.Label, err); known {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msg})
			return
		}
		if err != nil {
			apiError(w, err)
			return
		}
		if wait > 0 {
			if job, err = s.waitJob(r.Context(), job.PublicID, time.Duration(wait)*time.Second); err != nil {
				if r.Context().Err() != nil {
					return
				}
				apiError(w, err)
				return
			}
		}
		switch job.State {
		case models.JobSucceeded:
			var res importResult
			_ = json.Unmarshal([]byte(job.Result), &res) // a result that does not parse never got here: applying it failed the job
			writeJSON(w, http.StatusOK, struct {
				Provider string `json:"provider"`
				importResult
			}{p.Name, res})
		case models.JobFailed, models.JobExpired:
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the import failed", "job": jobForAPI(job)})
		case models.JobCanceled:
			writeJSON(w, http.StatusConflict, map[string]any{"error": "the import was canceled", "job": jobForAPI(job)})
		default:
			w.Header().Set("Location", "/api/v1/users/"+strconv.FormatInt(u.ID, 10)+"/jobs/"+job.PublicID)
			writeJSON(w, http.StatusAccepted, jobForAPI(job))
		}
	}))

	// GET /api/v1/users/{id}/jobs lists the user's latest jobs; GET and DELETE /api/v1/users/{id}/jobs/{job}
	// show and cancel one.
	mux.HandleFunc("GET /api/v1/users/{id}/jobs", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		jobs, err := s.store.ListJobs(r.Context(), models.JobFilter{UserID: u.ID, Limit: 50})
		if err != nil {
			apiError(w, err)
			return
		}
		out := make([]apiJob, 0, len(jobs))
		for _, j := range jobs {
			out = append(out, jobForAPI(j))
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("GET /api/v1/users/{id}/jobs/{job}", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		j, err := s.store.GetJob(r.Context(), r.PathValue("job"))
		if err != nil || j.UserID != u.ID { // another user's job looks like no job
			apiError(w, store.ErrNotFound)
			return
		}
		writeJSON(w, http.StatusOK, jobForAPI(j))
	}))
	mux.HandleFunc("DELETE /api/v1/users/{id}/jobs/{job}", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		j, err := s.store.CancelJob(r.Context(), r.PathValue("job"), u.ID, time.Now())
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, jobForAPI(j))
	}))
}
