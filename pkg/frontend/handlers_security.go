package frontend

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
)

const ()

// saveProfile stores the name, e-mail address and height shown on the settings page.
func (s *Frontend) saveProfile(w http.ResponseWriter, r *http.Request, u models.User) {
	err := s.api.SaveProfile(r.Context(), u.ID, apiclient.Profile{
		Name: r.PostFormValue("name"), Email: r.PostFormValue("email"), Timezone: r.PostFormValue("timezone"),
		HeightCm: r.PostFormValue("height_cm"), Current: r.PostFormValue("current"),
	})
	if err != nil {
		s.settingsError(w, r, u, err)
		return
	}
	settingsRedirect(w, r, u.ID, "Profile saved.")
}

// settingsError shows a refused change on the settings page with the status the backend gave; any
// other failure is answered as usual.
func (s *Frontend) settingsError(w http.ResponseWriter, r *http.Request, u models.User, err error) {
	var e *apiclient.Error
	if errors.As(err, &e) && (e.Code == apiclient.CodeInvalid || e.Code == apiclient.CodeThrottled ||
		e.Code == apiclient.CodeForbidden || e.Code == apiclient.CodeExists) {
		s.settingsPage(w, r, u, e.Message, "", e.Status)
		return
	}
	s.fail(w, err)
}

// reauthWindow is how recent the sign-in of a user without a password must be for a sensitive
// change, where recent sign-in is required.
const reauthWindow = 10 * time.Minute

func (s *Frontend) changePassword(w http.ResponseWriter, r *http.Request, u models.User) {
	pg, err := s.api.ChangePassword(r.Context(), u.ID, apiclient.PasswordChange{
		Current: r.PostFormValue("current"), Password: r.PostFormValue("password"), Confirm: r.PostFormValue("confirm"),
	})
	if err != nil {
		s.settingsError(w, r, u, err)
		return
	}
	// The old session is gone; the backend sent the page data along with the new session cookie.
	s.renderSettingsPage(w, u, pg, "", "Password saved. Access tokens were revoked; create new ones if you use the API.", "", http.StatusOK, nil)
}

func (s *Frontend) createToken(w http.ResponseWriter, r *http.Request, u models.User) {
	token, err := s.api.CreateToken(r.Context(), u.ID, apiclient.NewTokenRequest{
		Name: r.PostFormValue("name"), Expires: r.PostFormValue("expires"), Current: r.PostFormValue("current"),
	})
	if err != nil {
		s.settingsError(w, r, u, err)
		return
	}
	// Rendered directly, never redirected, so the token does not end up in a URL.
	s.renderSettings(w, r, u, "", "Token created.", token, http.StatusOK)
}

func (s *Frontend) deleteToken(w http.ResponseWriter, r *http.Request, u models.User) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.api.DeleteToken(r.Context(), u.ID, id); err != nil {
		s.fail(w, err)
		return
	}
	settingsRedirect(w, r, u.ID, "Token revoked.")
}

func (s *Frontend) unlinkIdentity(w http.ResponseWriter, r *http.Request, u models.User) {
	msg, err := s.api.UnlinkIdentity(r.Context(), u.ID, r.PathValue("provider"))
	if err != nil {
		s.fail(w, err)
		return
	}
	settingsRedirect(w, r, u.ID, msg)
}

func settingsRedirect(w http.ResponseWriter, r *http.Request, userID int64, msg string) {
	http.Redirect(w, r, settingsPath(userID, msg), http.StatusSeeOther)
}

// settingsPath is the address of a user's settings page with a message to show.
func settingsPath(userID int64, msg string) string {
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	return userPath(userID, "settings") + "?msg=" + url.QueryEscape(msg)
}
