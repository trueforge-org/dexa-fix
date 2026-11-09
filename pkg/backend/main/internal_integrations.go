package mainbackend

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// Internal endpoints for provider imports, the OAuth flow and Strength Level. The answers carry the
// message the page shows; the frontend redirects with it.

func (s *Backend) internalIntegrationRoutes(mux *http.ServeMux) {
	p := apiclient.Prefix
	u := p + "/users/{id}"
	mux.HandleFunc("POST "+u+"/import/{provider}", s.internalUser(s.internalImport))
	mux.HandleFunc("POST "+u+"/integrations/{provider}/connect", s.internalUser(s.internalConnect))
	mux.HandleFunc("POST "+u+"/integrations/{provider}/disconnect", s.internalUser(s.internalDisconnect))
	mux.HandleFunc("POST "+p+"/integrations/{provider}/callback", s.internalCallback)

	mux.HandleFunc("POST "+u+"/strengthlevel/login", s.internalUser(s.internalSLSaveLogin))
	mux.HandleFunc("DELETE "+u+"/strengthlevel/login", s.internalUser(s.internalSLDeleteLogin))
	mux.HandleFunc("POST "+u+"/strengthlevel/sync", s.internalUser(s.internalSLSync))
	mux.HandleFunc("GET "+u+"/strengthlevel/status", s.internalUser(s.internalSLStatus))
	mux.HandleFunc("POST "+u+"/strengthlevel/day", s.internalUser(s.internalSLDay))
}

func flashed(w http.ResponseWriter, msg string) {
	internalJSON(w, http.StatusOK, apiclient.Flash{Message: msg})
}

// ---- provider imports and OAuth ----

func (s *Backend) internalImport(w http.ResponseWriter, r *http.Request, u models.User) {
	var in struct{ Day string }
	if !internalDecode(w, r, &in) {
		return
	}
	p, ok := s.importProvider(r.PathValue("provider"))
	if !ok {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return
	}
	opts, err := parseOptions("", in.Day)
	if err != nil {
		flashed(w, err.Error())
		return
	}
	if _, msg := s.importBlocker(r, p, u.ID); msg != "" {
		flashed(w, msg)
		return
	}
	job, err := s.queueImport(r, p, u.ID, opts, true)
	if msg, known := queueFailure(p.Label, err); known {
		flashed(w, msg)
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	job, err = s.waitJob(r.Context(), job.PublicID, importWait)
	if err != nil {
		if r.Context().Err() == nil {
			internalError(w, err)
		}
		return
	}
	flashed(w, s.importMessage(p, in.Day, job))
}

// internalConnect sets the anti-CSRF state cookie, which carries the user id because the provider
// returns to a fixed callback URL, and answers with the provider's authorize URL.
func (s *Backend) internalConnect(w http.ResponseWriter, r *http.Request, u models.User) {
	name := r.PathValue("provider")
	p, ok := s.oauthProvider(r.Context(), name)
	if !ok {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		internalError(w, err)
		return
	}
	state := hex.EncodeToString(b)
	authURL, err := p.OAuth.AuthURL(r.Context(), s, state)
	if err != nil {
		internalError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookiePrefix + name, Value: strconv.FormatInt(u.ID, 10) + "." + state, Path: "/",
		MaxAge: int(oauthStateTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r),
	})
	internalJSON(w, http.StatusOK, map[string]string{"redirect": authURL})
}

func (s *Backend) internalCallback(w http.ResponseWriter, r *http.Request) {
	var in struct{ State, Code, Error string }
	if !internalDecode(w, r, &in) {
		return
	}
	name := r.PathValue("provider")
	p, ok := s.oauthProvider(r.Context(), name)
	if !ok || p.kind(workerapi.ActionConnect) == nil {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return
	}
	cookieName := oauthCookiePrefix + name
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true})
	c, err := r.Cookie(cookieName)
	idStr, state, found := "", "", false
	if err == nil {
		idStr, state, found = strings.Cut(c.Value, ".")
	}
	cu := currentUser(r)
	if !found || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(in.State)) != 1 {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid or expired state, start the connection again")
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid state")
		return
	}
	if cu == nil || cu.ID != id { // the account must be linked by the user who started the flow
		internalFail(w, http.StatusForbidden, apiclient.CodeForbidden, "sign in as the user who started the connection")
		return
	}
	f := apiclient.Flash{UserID: cu.ID}
	if in.Error != "" {
		f.Message = p.Label + " connection was not authorised."
		internalJSON(w, http.StatusOK, f)
		return
	}
	if in.Code == "" {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "missing code")
		return
	}
	// The code works once and only for a moment: a worker exchanges it, and the page waits for that.
	job, _, err := s.enqueueJob(r.Context(), enqueueSpec{
		UserID: cu.ID, Kind: p.kind(workerapi.ActionConnect).Kind, Origin: models.JobOriginUser, Interactive: true,
		Payload: connectPayload{Code: in.Code}, Expires: connectExpiry,
	})
	if msg, known := queueFailure(p.Label, err); known {
		f.Message = "Could not connect " + p.Label + ". " + msg
		internalJSON(w, http.StatusOK, f)
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if job, err = s.waitJob(r.Context(), job.PublicID, importWait); err != nil {
		if r.Context().Err() == nil {
			internalError(w, err)
		}
		return
	}
	switch job.State {
	case models.JobSucceeded:
		f.Message = p.Label + " connected. Use “Import” to fetch your measurements."
	case models.JobQueued, models.JobRunning:
		f.Message = p.Label + " is being connected in the background. Reload in a moment."
	default:
		log.Error().Str("provider", name).Str("job", job.PublicID).Str("error", job.Error).Msg("connect failed")
		f.Message = "Could not connect " + p.Label + ". The details are in the server log."
	}
	internalJSON(w, http.StatusOK, f)
}

func (s *Backend) internalDisconnect(w http.ResponseWriter, r *http.Request, u models.User) {
	name := r.PathValue("provider")
	p, ok := s.providerNamed(name)
	if !ok || p.Integration == "" {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return
	}
	ctx := r.Context()
	if err := s.store.DeleteIntegration(ctx, u.ID, p.Integration); err != nil {
		internalError(w, err)
		return
	}
	// Nothing of this provider may go on for a user who disconnected: stop what is queued or running
	// and forget the schedule.
	if err := s.store.CancelJobs(ctx, u.ID, p.kindNames(), time.Now()); err != nil {
		internalError(w, err)
		return
	}
	s.resumeSchedule(ctx, u.ID, p.kindNames()...)
	s.audit(r, u.ID, auditDisconnect, name)
	flashed(w, p.Label+" disconnected. Imported readings were kept.")
}

// ---- Strength Level ----

// strengthLevelSaveLogin stores the user's Strength Level login once a worker saw Strength Level
// accept it. It is refused with 403 unless an administrator enabled automatic sync for the user.
func (s *Backend) internalSLSaveLogin(w http.ResponseWriter, r *http.Request, u models.User) {
	if !u.StrengthLevelAuto {
		internalFail(w, http.StatusForbidden, apiclient.CodeForbidden, "Strength Level sync is not enabled for this account")
		return
	}
	var in struct{ Email, Password string }
	if !internalDecode(w, r, &in) {
		return
	}
	email := strings.TrimSpace(in.Email)
	password := in.Password
	if _, err := mail.ParseAddress(email); err != nil || password == "" || len(password) > 200 {
		flashed(w, "Enter the e-mail address and password of your Strength Level account.")
		return
	}
	// Signing in goes to another service on the user's behalf, so wrong logins are limited like
	// sign-ins here: a wrong password is reported now and nothing wrong is stored.
	limitKey := "sl:" + strconv.FormatInt(u.ID, 10)
	if s.limiter.Blocked(r.Context(), limitKey, maxStrengthLevelLoginFail) {
		flashed(w, "Too many failed Strength Level sign-ins. Try again in a few minutes.")
		return
	}
	job, created, err := s.enqueueJob(r.Context(), enqueueSpec{
		UserID: u.ID, Kind: kindStrengthLevelCheckLogin, Origin: models.JobOriginUser, Interactive: true,
		Payload: slLogin{Email: email, Password: password},
	})
	if msg, known := queueFailure("Strength Level", err); known {
		flashed(w, msg)
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if !created { // the earlier check carries the earlier login: do not mix them up
		flashed(w, "A Strength Level login check is already running. Try again in a moment.")
		return
	}
	if job, err = s.waitJob(r.Context(), job.PublicID, importWait); err != nil {
		if r.Context().Err() == nil {
			internalError(w, err)
		}
		return
	}
	switch job.State {
	case models.JobSucceeded:
		var res workerapi.CheckLoginResult
		if err := json.Unmarshal([]byte(job.Result), &res); err == nil && res.LoginOK {
			flashed(w, "Strength Level login saved.")
			return
		} else if err == nil {
			flashed(w, "Could not sign in to Strength Level: "+res.Message)
			return
		}
		flashed(w, "Could not sign in to Strength Level.")
	case models.JobQueued, models.JobRunning:
		flashed(w, "Your Strength Level login is being checked in the background. Reload in a moment.")
	case models.JobCanceled:
		flashed(w, "The Strength Level login check was canceled.")
	default:
		flashed(w, "Could not sign in to Strength Level: "+job.Error)
	}
}

func (s *Backend) internalSLDeleteLogin(w http.ResponseWriter, r *http.Request, u models.User) {
	s.stopStrengthLevel(r.Context(), u.ID)
	if err := s.store.DeleteIntegration(r.Context(), u.ID, strengthLevelProvider); err != nil && !errors.Is(err, store.ErrNotFound) {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditSLLoginRemoved, "")
	flashed(w, "Strength Level login removed.")
}

// internalSLSync queues the multi-day sync and returns at once; the settings page shows its
// progress. It is allowed only for users an administrator permitted to run it.
func (s *Backend) internalSLSync(w http.ResponseWriter, r *http.Request, u models.User) {
	if !u.StrengthLevelAuto || !u.StrengthLevelBulk {
		internalFail(w, http.StatusForbidden, apiclient.CodeForbidden, "the Strength Level multi-day sync is not enabled for this account")
		return
	}
	var in struct {
		Days   string
		Update bool
	}
	if !internalDecode(w, r, &in) {
		return
	}
	if _, err := s.strengthLevelLogin(r, u); err != nil {
		flashed(w, "Save your Strength Level login first.")
		return
	}
	days := strengthLevelDefaultDays
	if raw := strings.TrimSpace(in.Days); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > strengthLevelMaxDays {
			flashed(w, fmt.Sprintf("Enter a number of days between 1 and %d.", strengthLevelMaxDays))
			return
		}
		days = n
	}
	// Every written day is one request, so allow time for as many as were asked for.
	_, created, err := s.enqueueJob(r.Context(), enqueueSpec{
		UserID: u.ID, Kind: kindStrengthLevelSync, Origin: models.JobOriginUser, Interactive: true,
		Payload: slSyncPayload{Days: days, Update: in.Update}, Timeout: strengthLevelTimeout + time.Duration(days)*2*time.Second,
	})
	if msg, known := queueFailure("Strength Level", err); known {
		flashed(w, msg)
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if !created {
		flashed(w, "A sync is already running.")
		return
	}
	s.audit(r, u.ID, auditSLSync, strconv.Itoa(days)+" days")
	flashed(w, "Sync started in the background.")
}

// internalSLStatus answers with the progress of the latest sync. The settings page polls it, which
// is far cheaper than rendering the whole page every two seconds.
func (s *Backend) internalSLStatus(w http.ResponseWriter, r *http.Request, u models.User) {
	if !u.StrengthLevelAuto || !u.StrengthLevelBulk {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return
	}
	v, err := s.slJobViewOf(r.Context(), u.ID)
	if err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusOK, v)
}

// internalSLDay sends one day's estimate to Strength Level now, adding the day or overwriting the
// entry that exists. It is what the button on the estimates page does.
func (s *Backend) internalSLDay(w http.ResponseWriter, r *http.Request, u models.User) {
	if !u.StrengthLevelAuto {
		internalFail(w, http.StatusForbidden, apiclient.CodeForbidden, "Strength Level sync is not enabled for this account")
		return
	}
	var in struct{ Day string }
	if !internalDecode(w, r, &in) {
		return
	}
	day := in.Day
	if t, err := time.Parse(dateLayout, day); err != nil || t.Format(dateLayout) != day {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid day")
		return
	}
	if _, err := s.strengthLevelLogin(r, u); err != nil {
		flashed(w, "Save your Strength Level login in Settings first.")
		return
	}
	job, _, err := s.enqueueJob(r.Context(), enqueueSpec{
		UserID: u.ID, Kind: kindStrengthLevelDay, Origin: models.JobOriginUser, Interactive: true,
		Dedupe:  fmt.Sprintf("%s:%d:%s", kindStrengthLevelDay, u.ID, day),
		Payload: slDayPayload{Day: day},
	})
	if msg, known := queueFailure("Strength Level", err); known {
		flashed(w, msg)
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if job, err = s.waitJob(r.Context(), job.PublicID, importWait); err != nil {
		if r.Context().Err() == nil {
			internalError(w, err)
		}
		return
	}
	switch job.State {
	case models.JobSucceeded:
		var res workerapi.ExportResult
		if err := json.Unmarshal([]byte(job.Result), &res); err != nil {
			flashed(w, "Could not send "+day+".")
			return
		}
		what := "already up to date"
		if res.Added > 0 {
			what = "added"
		} else if res.Updated > 0 {
			what = "overwritten"
		}
		flashed(w, fmt.Sprintf("Strength Level: %s %s.", day, what))
	case models.JobQueued, models.JobRunning:
		flashed(w, "Sending "+day+" to Strength Level in the background. Reload in a moment.")
	case models.JobCanceled:
		flashed(w, "Sending "+day+" was canceled.")
	default:
		flashed(w, job.Error)
	}
}
