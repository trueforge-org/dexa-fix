package web

import (
	"net/http"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// computeEstimates loads a user's data and runs the estimator.
// The result comes from the cache when the user's data has not changed since it was computed (see
// estcache.go) and is shared, so callers must not modify it.
func (s *Backend) computeEstimates(r *http.Request, u models.User) (estimate.Result, error) {
	now, loc := time.Now(), u.Location()
	key := estimateKey{version: u.DataVersion, day: now.In(loc).Format(dateLayout)}
	return s.estCache.get(u.ID, key, func() (estimate.Result, error) {
		scans, err := s.store.ListDexaScans(r.Context(), u.ID)
		if err != nil {
			return estimate.Result{}, err
		}
		readings, err := s.store.ListScaleReadings(r.Context(), u.ID)
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

// withingsView reports the Withings state for a user, or nil if the importer is not registered.
// Unlike oauthProvider it also returns a view when the server lacks credentials, so the pages can
// say so instead of showing nothing.
func (s *Backend) withingsView(r *http.Request, u models.User) *integrationView {
	imp, ok := s.importers.Get(providerWithings)
	if !ok {
		return nil
	}
	p, ok := imp.(importer.OAuthProvider)
	if !ok {
		return nil
	}
	v := &integrationView{Configured: p.Configured()}
	v.Connected = v.Configured && p.Connected(r.Context(), s.store, u.ID)
	return v
}
