package web

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// rankRow is one method on the settings page with its 1-based rank.
type rankRow struct {
	Key   string
	Label string
	Rank  int
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, u models.User, errMsg, saved string, status int) {
	s.renderSettings(w, r, u, errMsg, saved, "", status)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, u models.User, errMsg, saved, newToken string, status int) {
	s.renderSettingsWith(w, r, u, errMsg, saved, newToken, status, nil)
}

// renderSettingsWith is renderSettings with a chance to add to the sign-in section, for values
// that are shown once (a QR code, recovery codes).
func (s *Server) renderSettingsWith(w http.ResponseWriter, r *http.Request, u models.User, errMsg, saved, newToken string, status int, extra func(*securityView)) {
	pg, err := s.api.SettingsPage(r.Context(), u.ID, flashParam(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderSettingsPage(w, u, pg, errMsg, saved, newToken, status, extra)
}

// renderSettingsPage shows the settings page from data the caller already has.
func (s *Server) renderSettingsPage(w http.ResponseWriter, u models.User, pg apiclient.SettingsPage, errMsg, saved, newToken string, status int, extra func(*securityView)) {
	rows := make([]rankRow, len(u.MethodRanking))
	for i, k := range u.MethodRanking {
		rows[i] = rankRow{Key: k, Label: methodLabel(k), Rank: i + 1}
	}
	sec := pg.Security
	sec.NewToken = newToken
	if extra != nil {
		extra(sec)
	}
	s.render(w, "settings.html", status, pageData{
		Title: u.Name + " · settings", Tab: "settings", User: &u, Error: errMsg,
		AthleteDefault: u.AthleteDefault, Ranking: rows, Saved: saved,
		Withings:      pg.Withings,
		StrengthLevel: pg.StrengthLevel,
		Security:      sec,
	})
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request, u models.User) {
	type ranked struct {
		key  string
		rank int
		pos  int
	}
	rs := make([]ranked, 0, len(models.Methods))
	for i, m := range models.Methods {
		rank := i + 1
		if raw := strings.TrimSpace(r.PostFormValue("rank_" + m.Key)); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 100 {
				u.AthleteDefault = r.PostFormValue("athlete_default") != ""
				s.settingsPage(w, r, u, "Ranks must be whole numbers from 1 to 100.", "", http.StatusUnprocessableEntity)
				return
			}
			rank = n
		}
		rs = append(rs, ranked{m.Key, rank, i})
	}
	// Lowest rank first; equal ranks keep the default order.
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].rank < rs[j].rank })
	keys := make([]string, len(rs))
	for i, x := range rs {
		keys[i] = x.key
	}
	athlete := r.PostFormValue("athlete_default") != ""
	if err := s.api.SaveSettings(r.Context(), u.ID, apiclient.Settings{AthleteDefault: athlete, MethodRanking: keys}); err != nil {
		s.fail(w, err)
		return
	}
	u.AthleteDefault, u.MethodRanking = athlete, models.NormalizeRanking(keys)
	s.settingsPage(w, r, u, "", "Settings saved.", http.StatusOK)
}
