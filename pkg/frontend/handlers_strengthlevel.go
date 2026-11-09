package frontend

import (
	"bytes"
	"net/http"
	"net/url"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

func settingsFlash(w http.ResponseWriter, r *http.Request, u models.User, msg string) {
	http.Redirect(w, r, userPath(u.ID, "settings")+"?msg="+url.QueryEscape(msg)+"#strengthlevel", http.StatusSeeOther)
}

func (s *Frontend) strengthLevelSaveLogin(w http.ResponseWriter, r *http.Request, u models.User) {
	f, err := s.api.SaveStrengthLevelLogin(r.Context(), u.ID, r.PostFormValue("email"), r.PostFormValue("password"))
	if err != nil {
		s.fail(w, err)
		return
	}
	settingsFlash(w, r, u, f.Message)
}

func (s *Frontend) strengthLevelSync(w http.ResponseWriter, r *http.Request, u models.User) {
	f, err := s.api.StartStrengthLevelSync(r.Context(), u.ID, r.PostFormValue("days"), r.PostFormValue("update") != "")
	if err != nil {
		s.fail(w, err)
		return
	}
	settingsFlash(w, r, u, f.Message)
}

// strengthLevelStatus renders only the progress block of a running sync. The settings page polls
// it, which is far cheaper than rendering the whole page every two seconds.
func (s *Frontend) strengthLevelStatus(w http.ResponseWriter, r *http.Request, u models.User) {
	job, err := s.api.StrengthLevelStatus(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var buf bytes.Buffer
	if err := s.tmpl["settings.html"].ExecuteTemplate(&buf, "slstatus", slView{UserID: u.ID, Job: job}); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (s *Frontend) strengthLevelDay(w http.ResponseWriter, r *http.Request, u models.User) {
	f, err := s.api.SendStrengthLevelDay(r.Context(), u.ID, r.PostFormValue("day"))
	if err != nil {
		s.fail(w, err)
		return
	}
	flashRedirect(w, r, u.ID, f.Message)
}

func (s *Frontend) strengthLevelDeleteLogin(w http.ResponseWriter, r *http.Request, u models.User) {
	f, err := s.api.DeleteStrengthLevelLogin(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	settingsFlash(w, r, u, f.Message)
}
