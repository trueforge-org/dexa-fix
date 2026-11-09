package web

import (
	"net/http"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/estimate"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// Internal endpoints for the estimates and settings pages and the HTML exports.

func (s *Backend) internalSettingsRoutes(mux *http.ServeMux) {
	p := apiclient.Prefix + "/users/{id}"
	mux.HandleFunc("GET "+p+"/pages/estimates", s.internalUser(s.internalEstimatesPage))
	mux.HandleFunc("POST "+p+"/estimates/columns", s.internalUser(s.internalColumnsSave))
	mux.HandleFunc("GET "+p+"/pages/settings", s.internalUser(s.internalSettingsPage))
	mux.HandleFunc("POST "+p+"/settings", s.internalUser(s.internalSettingsSave))
	mux.HandleFunc("GET "+p+"/export/profile", s.internalUser(s.internalExportProfile))
	mux.HandleFunc("GET "+p+"/export/data", s.internalUser(s.internalExportData))
}

func (s *Backend) internalEstimatesPage(w http.ResponseWriter, r *http.Request, u models.User) {
	res, err := s.computeEstimates(r, u)
	if err != nil {
		internalError(w, err)
		return
	}
	res.Days, res.Calibrations = orEmpty(res.Days), orEmpty(res.Calibrations)
	internalJSON(w, http.StatusOK, apiclient.EstimatesPage{
		Result: res, Withings: s.withingsView(r, u), SLReady: s.strengthLevelReady(r, u),
	})
}

func (s *Backend) internalColumnsSave(w http.ResponseWriter, r *http.Request, u models.User) {
	var in struct {
		Hidden []string `json:"hidden"`
	}
	if !internalDecode(w, r, &in) {
		return
	}
	known := make(map[string]bool, len(estimate.Fields))
	for _, f := range estimate.Fields {
		known[f.Key] = true
	}
	var hidden []string
	for _, k := range in.Hidden {
		if known[k] {
			hidden = append(hidden, k)
		}
	}
	if err := s.store.UpdateUserHiddenColumns(r.Context(), u.ID, hidden); err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusNoContent, nil)
}

func (s *Backend) internalSettingsPage(w http.ResponseWriter, r *http.Request, u models.User) {
	pg, err := s.settingsPageData(r, u)
	if err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusOK, pg)
}

// settingsPageData collects what the settings page shows besides the user.
func (s *Backend) settingsPageData(r *http.Request, u models.User) (apiclient.SettingsPage, error) {
	sec, err := s.securityView(r, u, "")
	if err != nil {
		return apiclient.SettingsPage{}, err
	}
	if sec.Activity, err = s.activityFor(r.Context(), u); err != nil {
		return apiclient.SettingsPage{}, err
	}
	return apiclient.SettingsPage{
		Withings:      s.withingsView(r, u),
		StrengthLevel: s.strengthLevelView(r, u),
		Security:      sec,
	}, nil
}

func (s *Backend) internalSettingsSave(w http.ResponseWriter, r *http.Request, u models.User) {
	var in apiclient.Settings
	if !internalDecode(w, r, &in) {
		return
	}
	if err := s.store.UpdateUserSettings(r.Context(), u.ID, in.AthleteDefault, models.NormalizeRanking(in.MethodRanking)); err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusNoContent, nil)
}

func (s *Backend) internalExportProfile(w http.ResponseWriter, r *http.Request, u models.User) {
	e, err := s.buildProfileExport(r, u)
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditExportProfile, "")
	sendExport(w, "profile", u.ID, e)
}

func (s *Backend) internalExportData(w http.ResponseWriter, r *http.Request, u models.User) {
	e, err := s.buildDataExport(r.Context(), u)
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditExportData, "")
	sendExport(w, "data", u.ID, e)
}
