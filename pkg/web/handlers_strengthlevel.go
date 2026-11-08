package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
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

// strengthLevelWindow is how far back a sync adds missing days, so a first sync does not write
// years of history at once.
const strengthLevelWindow = 90 * 24 * time.Hour

// strengthLevelTimeout bounds one sign-in or sync.
const strengthLevelTimeout = 2 * time.Minute

// strengthLevelProvider is the integration name under which the login is stored (encrypted).
const strengthLevelProvider = "strengthlevel"

// slLogin is the Strength Level login, stored as the data of the integration.
type slLogin struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// slView is the Strength Level card of the settings page.
type slView struct {
	Saved    bool
	Username string // the saved login's Strength Level username
	Email    string // the saved login's e-mail address; the password is never sent back
	Msg      string
}

// strengthLevelView returns the settings card, or nil when an administrator has not enabled
// automatic sync for the user.
func (s *Server) strengthLevelView(r *http.Request, u models.User) *slView {
	if !u.StrengthLevelAuto {
		return nil
	}
	v := &slView{Msg: flashParam(r)}
	login, err := s.strengthLevelLogin(r, u)
	if err == nil {
		v.Saved, v.Username, v.Email = true, login.Username, login.Email
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
	username := strings.TrimSpace(r.PostFormValue("username"))
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	if _, err := mail.ParseAddress(email); err != nil || username == "" || len(username) > 64 || password == "" || len(password) > 200 {
		settingsFlash(w, r, u, "Enter the username, e-mail address and password of your Strength Level account.")
		return
	}
	// Sign in first, so a wrong password or username is reported now and nothing wrong is stored.
	ctx, cancel := context.WithTimeout(r.Context(), strengthLevelTimeout)
	defer cancel()
	if err := client.New(strengthLevelBase, nil).Login(ctx, username, email, password); err != nil {
		settingsFlash(w, r, u, "Could not sign in to Strength Level: "+strengthLevelError(err))
		return
	}
	data, err := json.Marshal(slLogin{Username: username, Email: email, Password: password})
	if err == nil {
		err = s.store.PutIntegration(r.Context(), u.ID, strengthLevelProvider, data)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	settingsFlash(w, r, u, "Strength Level login saved.")
}

// strengthLevelError turns a client error into a sentence for the user; details go to the log.
func strengthLevelError(err error) string {
	switch {
	case errors.Is(err, client.ErrLogin):
		return "the e-mail address or password was refused."
	case errors.Is(err, client.ErrPage):
		log.Warn().Err(err).Msg("strengthlevel page changed")
		return "Strength Level looks different than expected (check the username), or it has changed."
	}
	log.Warn().Err(err).Msg("strengthlevel request failed")
	return "Strength Level could not be reached."
}

// strengthLevelSync adds the user's recent days that Strength Level has no entry for.
func (s *Server) strengthLevelSync(w http.ResponseWriter, r *http.Request, u models.User) {
	if !u.StrengthLevelAuto {
		http.Error(w, "Strength Level sync is not enabled for this account", http.StatusForbidden)
		return
	}
	login, err := s.strengthLevelLogin(r, u)
	if err != nil {
		settingsFlash(w, r, u, "Save your Strength Level login first.")
		return
	}
	readings, err := s.store.ListScaleReadings(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), strengthLevelTimeout)
	defer cancel()
	c := client.New(strengthLevelBase, nil)
	if err := c.Login(ctx, login.Username, login.Email, login.Password); err != nil {
		settingsFlash(w, r, u, "Could not sign in to Strength Level: "+strengthLevelError(err))
		return
	}
	since := time.Now().UTC().Add(-strengthLevelWindow).Format(strengthlevel.DayLayout)
	res, err := c.Sync(ctx, strengthlevel.DailyEntries(readings), since)
	if err != nil {
		settingsFlash(w, r, u, "Sync stopped: "+strengthLevelError(err))
		return
	}
	msg := fmt.Sprintf("Strength Level: added %d day(s), %d already there.", res.Added, res.Skipped)
	if len(res.Failed) > 0 {
		log.Warn().Strs("failed", res.Failed).Msg("strengthlevel sync")
		msg += fmt.Sprintf(" %d could not be added.", len(res.Failed))
	}
	settingsFlash(w, r, u, msg)
}

func (s *Server) strengthLevelDeleteLogin(w http.ResponseWriter, r *http.Request, u models.User) {
	if err := s.store.DeleteIntegration(r.Context(), u.ID, strengthLevelProvider); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	settingsFlash(w, r, u, "Strength Level login removed.")
}
