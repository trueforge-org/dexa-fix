package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
	"github.com/trueforge-org/dexa-fix/pkg/strengthlevel"
	"github.com/trueforge-org/dexa-fix/pkg/strengthlevel/client"
)

// strengthLevelBase is the Strength Level web app; tests point it at a fake.
var strengthLevelBase = client.DefaultBase

// A sync adds the missing days of the last N days. N defaults to 90 and is at most 10 years, so a
// slip of the finger does not write a lifetime of data.
const (
	strengthLevelDefaultDays = 90
	strengthLevelMaxDays     = 3650

	maxStrengthLevelJobs      = 3 // syncs running at the same time, over all users
	maxStrengthLevelLoginFail = 5 // wrong Strength Level logins per user and limiter window
)

// strengthLevelDelay is the pause between two writes to Strength Level; tests shorten it.
var strengthLevelDelay = 300 * time.Millisecond

// strengthLevelTimeout bounds a sign-in.
const strengthLevelTimeout = 2 * time.Minute

// strengthLevelProvider is the integration name under which the login is stored (encrypted).
const strengthLevelProvider = "strengthlevel"

// slLogin is the Strength Level login, stored as the data of the integration.
type slLogin struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// slView is the Strength Level card of the settings page.
type slView struct {
	UserID int64
	Saved  bool
	Email  string // the saved login's e-mail address; the password is never sent back
	Msg    string
	Bulk   bool       // the multi-day sync is allowed
	Job    *slJobView // the latest multi-day sync, if any
}

// strengthLevelView returns the settings card, or nil when an administrator has not enabled
// automatic sync for the user.
func (s *Server) strengthLevelView(r *http.Request, u models.User) *slView {
	if !u.StrengthLevelAuto {
		return nil
	}
	v := &slView{UserID: u.ID, Msg: flashParam(r)}
	login, err := s.strengthLevelLogin(r, u)
	if err == nil {
		v.Saved, v.Email = true, login.Email
	}
	if u.StrengthLevelBulk {
		v.Bulk, v.Job = true, s.sl.view(u.ID)
	}
	return v
}

func (s *Server) strengthLevelLogin(r *http.Request, u models.User) (slLogin, error) {
	var l slLogin
	data, err := s.store.GetIntegration(r.Context(), u.ID, strengthLevelProvider)
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(data, &l)
}

func settingsFlash(w http.ResponseWriter, r *http.Request, u models.User, msg string) {
	http.Redirect(w, r, userPath(u.ID, "settings")+"?msg="+url.QueryEscape(msg)+"#strengthlevel", http.StatusSeeOther)
}

// strengthLevelSaveLogin stores the user's Strength Level login. It is refused with 403 unless an
// administrator enabled automatic sync for the user.
func (s *Server) strengthLevelSaveLogin(w http.ResponseWriter, r *http.Request, u models.User) {
	if !u.StrengthLevelAuto {
		http.Error(w, "Strength Level sync is not enabled for this account", http.StatusForbidden)
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	if _, err := mail.ParseAddress(email); err != nil || password == "" || len(password) > 200 {
		settingsFlash(w, r, u, "Enter the e-mail address and password of your Strength Level account.")
		return
	}
	// Sign in first, so a wrong password is reported now and nothing wrong is stored. The check
	// signs in to another service from this server, so the failures are limited like sign-ins.
	limitKey := "sl:" + strconv.FormatInt(u.ID, 10)
	if s.limiter.Blocked(limitKey, maxStrengthLevelLoginFail) {
		settingsFlash(w, r, u, "Too many failed Strength Level sign-ins. Try again in a few minutes.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), strengthLevelTimeout)
	defer cancel()
	if err := client.New(strengthLevelBase, nil).Login(ctx, email, password); err != nil {
		if errors.Is(err, client.ErrLogin) {
			s.limiter.Fail(limitKey)
		}
		settingsFlash(w, r, u, "Could not sign in to Strength Level: "+strengthLevelError(err))
		return
	}
	s.limiter.Reset(limitKey)
	data, err := json.Marshal(slLogin{Email: email, Password: password})
	if err == nil {
		err = s.store.PutIntegration(r.Context(), u.ID, strengthLevelProvider, data)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, u.ID, auditSLLoginSaved, "")
	settingsFlash(w, r, u, "Strength Level login saved.")
}

// strengthLevelError turns a client error into a sentence for the user; details go to the log.
func strengthLevelError(err error) string {
	switch {
	case errors.Is(err, client.ErrLogin):
		return "the e-mail address or password was refused."
	case errors.Is(err, client.ErrPage):
		log.Warn().Err(err).Msg("strengthlevel page changed")
		return "Strength Level looks different than expected, so it may have changed."
	}
	log.Warn().Err(err).Msg("strengthlevel request failed")
	return "Strength Level could not be reached."
}

// strengthLevelSync starts the multi-day sync in the background and returns at once; the settings
// page shows its progress. It is allowed only for users an administrator permitted to run it.
func (s *Server) strengthLevelSync(w http.ResponseWriter, r *http.Request, u models.User) {
	if !u.StrengthLevelAuto || !u.StrengthLevelBulk {
		http.Error(w, "the Strength Level multi-day sync is not enabled for this account", http.StatusForbidden)
		return
	}
	login, err := s.strengthLevelLogin(r, u)
	if err != nil {
		settingsFlash(w, r, u, "Save your Strength Level login first.")
		return
	}
	days := strengthLevelDefaultDays
	if raw := strings.TrimSpace(r.PostFormValue("days")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > strengthLevelMaxDays {
			settingsFlash(w, r, u, fmt.Sprintf("Enter a number of days between 1 and %d.", strengthLevelMaxDays))
			return
		}
		days = n
	}
	update := r.PostFormValue("update") != ""
	est, err := s.computeEstimates(r, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	entries := strengthlevel.FromEstimates(est.Days)
	since := time.Now().UTC().AddDate(0, 0, -days).Format(strengthlevel.DayLayout)

	// Every written day is one request, so allow time for as many as were asked for. The job does
	// not belong to this request: it carries on after the response.
	ctx, cancel := context.WithTimeout(s.jobCtx, strengthLevelTimeout+time.Duration(days)*2*time.Second)
	job, ok := s.sl.begin(u.ID, days, update, cancel)
	if !ok {
		cancel()
		settingsFlash(w, r, u, "A sync is already running.")
		return
	}
	started := s.startJob(func() {
		// Only a few syncs hit Strength Level at once; the others wait their turn.
		select {
		case s.slSlots <- struct{}{}:
			defer func() { <-s.slSlots }()
		case <-ctx.Done():
			return
		}
		s.runStrengthLevelSync(ctx, job, login, entries, since, update)
	})
	if !started {
		cancel()
		settingsFlash(w, r, u, "The server is shutting down.")
		return
	}
	s.audit(r, u.ID, auditSLSync, strconv.Itoa(days)+" days")
	settingsFlash(w, r, u, "Sync started in the background.")
}

// strengthLevelStatus renders only the progress block of a running sync. The settings page polls
// it, which is far cheaper than rendering the whole page every two seconds.
func (s *Server) strengthLevelStatus(w http.ResponseWriter, r *http.Request, u models.User) {
	if !u.StrengthLevelAuto || !u.StrengthLevelBulk {
		http.NotFound(w, r)
		return
	}
	var buf bytes.Buffer
	if err := s.tmpl["settings.html"].ExecuteTemplate(&buf, "slstatus", slView{UserID: u.ID, Job: s.sl.view(u.ID)}); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (s *Server) runStrengthLevelSync(ctx context.Context, job *slJob, login slLogin, entries []strengthlevel.Entry, since string, update bool) {
	c := client.New(strengthLevelBase, nil)
	c.Delay = strengthLevelDelay
	if err := c.Login(ctx, login.Email, login.Password); err != nil {
		s.sl.finish(job, func(j *slJob) { j.err = "could not sign in: " + strengthLevelError(err) })
		return
	}
	res, err := c.Sync(ctx, entries, since, client.Options{
		Update:   update,
		Progress: func(done, total int) { s.sl.progress(job, done, total) },
	})
	if len(res.Failed) > 0 {
		log.Warn().Strs("failed", res.Failed).Msg("strengthlevel sync")
	}
	s.sl.finish(job, func(j *slJob) {
		j.added, j.updated, j.unchanged, j.failed = res.Added, res.Updated, res.Skipped, len(res.Failed)
		if err != nil && !j.canceled {
			j.err = strengthLevelError(err)
		}
	})
}

// strengthLevelDay sends one day's estimate to Strength Level now, adding the day or overwriting
// the entry that exists there. It is what the button on the estimates page does.
func (s *Server) strengthLevelDay(w http.ResponseWriter, r *http.Request, u models.User) {
	if !u.StrengthLevelAuto {
		http.Error(w, "Strength Level sync is not enabled for this account", http.StatusForbidden)
		return
	}
	day := r.PostFormValue("day")
	if t, err := time.Parse(strengthlevel.DayLayout, day); err != nil || t.Format(strengthlevel.DayLayout) != day {
		http.Error(w, "invalid day", http.StatusBadRequest)
		return
	}
	login, err := s.strengthLevelLogin(r, u)
	if err != nil {
		flashRedirect(w, r, u.ID, "Save your Strength Level login in Settings first.")
		return
	}
	est, err := s.computeEstimates(r, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	var entry *strengthlevel.Entry
	for _, e := range strengthlevel.FromEstimates(est.Days) {
		if e.Day == day {
			entry = &e
			break
		}
	}
	if entry == nil {
		flashRedirect(w, r, u.ID, "There is no weight estimate for "+day+" to send.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), strengthLevelTimeout)
	defer cancel()
	c := client.New(strengthLevelBase, nil)
	if err := c.Login(ctx, login.Email, login.Password); err != nil {
		flashRedirect(w, r, u.ID, "Could not sign in to Strength Level: "+strengthLevelError(err))
		return
	}
	out, err := c.Upsert(ctx, *entry)
	if err != nil {
		flashRedirect(w, r, u.ID, "Could not send "+day+": "+strengthLevelError(err))
		return
	}
	flashRedirect(w, r, u.ID, fmt.Sprintf("Strength Level: %s %s.", day, map[client.Outcome]string{
		client.Added: "added", client.Updated: "overwritten", client.Unchanged: "already up to date",
	}[out]))
}

// strengthLevelReady reports whether the single-day button is shown: the user may sync and has
// saved a login.
func (s *Server) strengthLevelReady(r *http.Request, u models.User) bool {
	if !u.StrengthLevelAuto {
		return false
	}
	_, err := s.strengthLevelLogin(r, u)
	return err == nil
}

func (s *Server) strengthLevelDeleteLogin(w http.ResponseWriter, r *http.Request, u models.User) {
	s.sl.stop(u.ID)
	if err := s.store.DeleteIntegration(r.Context(), u.ID, strengthLevelProvider); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	s.audit(r, u.ID, auditSLLoginRemoved, "")
	settingsFlash(w, r, u, "Strength Level login removed.")
}
