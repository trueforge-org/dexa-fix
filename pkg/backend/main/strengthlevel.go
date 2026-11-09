package mainbackend

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// The Strength Level feature is in workerprovider_strengthlevel.go (the jobs) and
// internal_integrations.go (the pages' endpoints). This file has what the pages show.

// strengthLevelView returns the settings card, or nil when an administrator has not enabled
// automatic sync for the user.
func (s *Backend) strengthLevelView(r *http.Request, u models.User) *slView {
	if !u.StrengthLevelAuto {
		return nil
	}
	v := &slView{UserID: u.ID, Msg: flashParam(r)}
	login, err := s.strengthLevelLogin(r, u)
	if err == nil {
		v.Saved, v.Email = true, login.Email
	}
	if v.Saved {
		v.AutoPaused = s.pausedReason(r.Context(), u.ID, kindStrengthLevelSync)
	}
	if u.StrengthLevelBulk {
		v.Bulk = true
		var err error
		if v.Job, err = s.slJobViewOf(r.Context(), u.ID); err != nil {
			log.Warn().Err(err).Msg("read the Strength Level sync status")
		}
	}
	return v
}

func (s *Backend) strengthLevelLogin(r *http.Request, u models.User) (slLogin, error) {
	var l slLogin
	data, err := s.store.GetIntegration(r.Context(), u.ID, strengthLevelProvider)
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(data, &l)
}

// strengthLevelReady reports whether the single-day button is shown: the user may sync and has
// saved a login.
func (s *Backend) strengthLevelReady(r *http.Request, u models.User) bool {
	if !u.StrengthLevelAuto {
		return false
	}
	_, err := s.strengthLevelLogin(r, u)
	return err == nil
}
