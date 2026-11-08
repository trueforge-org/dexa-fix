package web

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// rankRow is one method on the settings page with its 1-based rank.
type rankRow struct {
	Key   string
	Label string
	Rank  int
}

func (s *Server) settingsPage(w http.ResponseWriter, u models.User, errMsg, saved string, status int) {
	rows := make([]rankRow, len(u.MethodRanking))
	for i, k := range u.MethodRanking {
		rows[i] = rankRow{Key: k, Label: methodLabel(k), Rank: i + 1}
	}
	s.render(w, "settings.html", status, pageData{
		Title: u.Name + " · settings", Tab: "settings", User: &u, Error: errMsg,
		AthleteDefault: u.AthleteDefault, Ranking: rows, Saved: saved,
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
				s.settingsPage(w, u, "Ranks must be whole numbers from 1 to 100.", "", http.StatusUnprocessableEntity)
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
	if err := s.store.UpdateUserSettings(r.Context(), u.ID, athlete, keys); err != nil {
		s.fail(w, err)
		return
	}
	u.AthleteDefault, u.MethodRanking = athlete, models.NormalizeRanking(keys)
	s.settingsPage(w, u, "", "Settings saved.", http.StatusOK)
}
