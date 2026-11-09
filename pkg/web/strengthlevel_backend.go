package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/trueforge-org/dexa-fix/pkg/models"
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
	if u.StrengthLevelBulk {
		v.Bulk, v.Job = true, s.sl.view(u.ID)
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

func (s *Backend) runStrengthLevelSync(ctx context.Context, job *slJob, login slLogin, entries []strengthlevel.Entry, since string, update bool) {
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

// strengthLevelReady reports whether the single-day button is shown: the user may sync and has
// saved a login.
func (s *Backend) strengthLevelReady(r *http.Request, u models.User) bool {
	if !u.StrengthLevelAuto {
		return false
	}
	_, err := s.strengthLevelLogin(r, u)
	return err == nil
}
