package web

import (
	"context"
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
	"github.com/trueforge-org/dexa-fix/pkg/store"
	"github.com/trueforge-org/dexa-fix/pkg/strengthlevel"
	"github.com/trueforge-org/dexa-fix/pkg/strengthlevel/client"
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
	imp, ok := s.importers.Get(r.PathValue("provider"))
	if !ok {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return
	}
	o, err := parseOptions("", in.Day)
	if err != nil {
		flashed(w, err.Error())
		return
	}
	res, err := imp.Import(r.Context(), s.store, u.ID, o)
	if err != nil {
		flashed(w, importError(label(imp.Name()), err))
		return
	}
	s.audit(r, u.ID, auditImport, imp.Name())
	flashed(w, summary(label(imp.Name()), res, in.Day))
}

// internalConnect sets the anti-CSRF state cookie, which carries the user id because the provider
// returns to a fixed callback URL, and answers with the provider's authorize URL.
func (s *Backend) internalConnect(w http.ResponseWriter, r *http.Request, u models.User) {
	name := r.PathValue("provider")
	p, ok := s.oauthProvider(name)
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
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookiePrefix + name, Value: strconv.FormatInt(u.ID, 10) + "." + state, Path: "/",
		MaxAge: int(oauthStateTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r),
	})
	internalJSON(w, http.StatusOK, map[string]string{"redirect": p.AuthURL(state)})
}

func (s *Backend) internalCallback(w http.ResponseWriter, r *http.Request) {
	var in struct{ State, Code, Error string }
	if !internalDecode(w, r, &in) {
		return
	}
	name := r.PathValue("provider")
	p, ok := s.oauthProvider(name)
	if !ok {
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
		f.Message = label(name) + " connection was not authorised."
		internalJSON(w, http.StatusOK, f)
		return
	}
	if in.Code == "" {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "missing code")
		return
	}
	if err := p.Connect(r.Context(), s.store, cu.ID, in.Code); err != nil {
		log.Error().Err(err).Str("provider", name).Msg("connect failed")
		f.Message = "Could not connect " + label(name) + ". The details are in the server log."
		internalJSON(w, http.StatusOK, f)
		return
	}
	s.audit(r, cu.ID, auditConnect, name)
	f.Message = label(name) + " connected. Use “Import” to fetch your measurements."
	internalJSON(w, http.StatusOK, f)
}

func (s *Backend) internalDisconnect(w http.ResponseWriter, r *http.Request, u models.User) {
	name := r.PathValue("provider")
	p, ok := s.oauthProvider(name)
	if !ok {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return
	}
	if err := p.Disconnect(r.Context(), s.store, u.ID); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditDisconnect, name)
	flashed(w, label(name)+" disconnected. Imported readings were kept.")
}

// ---- Strength Level ----

// strengthLevelSaveLogin stores the user's Strength Level login. It is refused with 403 unless an
// administrator enabled automatic sync for the user.
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
	// Sign in first, so a wrong password is reported now and nothing wrong is stored. The check
	// signs in to another service from this server, so the failures are limited like sign-ins.
	limitKey := "sl:" + strconv.FormatInt(u.ID, 10)
	if s.limiter.Blocked(limitKey, maxStrengthLevelLoginFail) {
		flashed(w, "Too many failed Strength Level sign-ins. Try again in a few minutes.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), strengthLevelTimeout)
	defer cancel()
	if err := client.New(strengthLevelBase, nil).Login(ctx, email, password); err != nil {
		if errors.Is(err, client.ErrLogin) {
			s.limiter.Fail(limitKey)
		}
		flashed(w, "Could not sign in to Strength Level: "+strengthLevelError(err))
		return
	}
	s.limiter.Reset(limitKey)
	data, err := json.Marshal(slLogin{Email: email, Password: password})
	if err == nil {
		err = s.store.PutIntegration(r.Context(), u.ID, strengthLevelProvider, data)
	}
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditSLLoginSaved, "")
	flashed(w, "Strength Level login saved.")
}

func (s *Backend) internalSLDeleteLogin(w http.ResponseWriter, r *http.Request, u models.User) {
	s.sl.stop(u.ID)
	if err := s.store.DeleteIntegration(r.Context(), u.ID, strengthLevelProvider); err != nil && !errors.Is(err, store.ErrNotFound) {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditSLLoginRemoved, "")
	flashed(w, "Strength Level login removed.")
}

// internalSLSync starts the multi-day sync in the background and returns at once; the settings
// page shows its progress. It is allowed only for users an administrator permitted to run it.
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
	login, err := s.strengthLevelLogin(r, u)
	if err != nil {
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
	update := in.Update
	est, err := s.computeEstimates(r, u)
	if err != nil {
		internalError(w, err)
		return
	}
	entries := strengthlevel.FromEstimates(est.Days)
	since := time.Now().UTC().AddDate(0, 0, -days).Format(strengthlevel.DayLayout)

	// Every written day is one request, so allow time for as many as were asked for. The job does
	// not belong to this request: it carries on after the response.
	ctx, cancel := context.WithTimeout(s.bg.Ctx, strengthLevelTimeout+time.Duration(days)*2*time.Second)
	job, ok := s.sl.begin(u.ID, days, update, cancel)
	if !ok {
		cancel()
		flashed(w, "A sync is already running.")
		return
	}
	started := s.bg.Start(func() {
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
		flashed(w, "The server is shutting down.")
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
	internalJSON(w, http.StatusOK, s.sl.view(u.ID))
}

// internalSLDay sends one day's estimate to Strength Level now, adding the day or overwriting the
// entry that exists there. It is what the button on the estimates page does.
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
	if t, err := time.Parse(strengthlevel.DayLayout, day); err != nil || t.Format(strengthlevel.DayLayout) != day {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid day")
		return
	}
	login, err := s.strengthLevelLogin(r, u)
	if err != nil {
		flashed(w, "Save your Strength Level login in Settings first.")
		return
	}
	est, err := s.computeEstimates(r, u)
	if err != nil {
		internalError(w, err)
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
		flashed(w, "There is no weight estimate for "+day+" to send.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), strengthLevelTimeout)
	defer cancel()
	c := client.New(strengthLevelBase, nil)
	if err := c.Login(ctx, login.Email, login.Password); err != nil {
		flashed(w, "Could not sign in to Strength Level: "+strengthLevelError(err))
		return
	}
	out, err := c.Upsert(ctx, *entry)
	if err != nil {
		flashed(w, "Could not send "+day+": "+strengthLevelError(err))
		return
	}
	flashed(w, fmt.Sprintf("Strength Level: %s %s.", day, map[client.Outcome]string{
		client.Added: "added", client.Updated: "overwritten", client.Unchanged: "already up to date",
	}[out]))
}
