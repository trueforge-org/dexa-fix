package mainbackend

import (
	"context"
	"net/http"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// computeEstimates loads a user's data and runs the estimator.
// The result comes from the cache when the user's data has not changed since it was computed (see
// estcache.go) and is shared, so callers must not modify it.
func (s *Backend) computeEstimates(ctx context.Context, u models.User) (estimate.Result, error) {
	now, loc := time.Now(), u.Location()
	key := estimateKey{version: u.DataVersion, day: now.In(loc).Format(dateLayout)}
	return s.estCache.get(u.ID, key, func() (estimate.Result, error) {
		scans, err := s.store.ListDexaScans(ctx, u.ID)
		if err != nil {
			return estimate.Result{}, err
		}
		readings, err := s.store.ListScaleReadings(ctx, u.ID)
		if err != nil {
			return estimate.Result{}, err
		}
		return estimate.Compute(scans, readings, estimate.Options{
			Today:          now,
			Location:       loc,
			Ranking:        u.MethodRanking,
			AthleteDefault: u.AthleteDefault,
			HeightCm:       u.HeightCm,
		}), nil
	})
}

// withingsView reports the Withings state for a user. Unlike oauthProvider it also returns a view
// when the server lacks credentials, so the pages can say so instead of showing nothing.
func (s *Backend) withingsView(r *http.Request, u models.User) *integrationView {
	p, ok := s.providerNamed(providerWithings)
	if !ok || p.OAuth == nil {
		return nil
	}
	configured, _ := p.OAuth.Configured(r.Context(), s)
	v := &integrationView{Configured: configured}
	v.Connected = configured && s.connected(r.Context(), p, u.ID)
	if v.Connected {
		v.AutoPaused = s.pausedReason(r.Context(), u.ID, kindWithingsImport)
	}
	return v
}
