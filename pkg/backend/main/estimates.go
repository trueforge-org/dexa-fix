package mainbackend

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// computeEstimates loads a user's data and runs the estimator.
// The result comes from the cache when the user's data has not changed since it was computed (see
// estcache.go) and is shared, so callers must not modify it.
func (s *Backend) computeEstimates(ctx context.Context, u models.User) (estimate.Result, error) {
	now, loc := time.Now(), u.Location()
	key, err := s.estimateKeyFor(ctx, u, now)
	if err != nil {
		return estimate.Result{}, err
	}
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

// estimateKeyFor names the inputs of a user's estimates: the version of their body data, the
// settings of the user row the estimates depend on, and the day in their time zone.
func (s *Backend) estimateKeyFor(ctx context.Context, u models.User, now time.Time) (estimateKey, error) {
	v, err := s.store.BodyVersion(ctx, u.ID)
	if err != nil {
		return estimateKey{}, err
	}
	return estimateKey{version: v, params: estimateParamsHash(u), day: now.In(u.Location()).Format(dateLayout)}, nil
}

// estimateParamsHash digests what the estimates take from the user row besides the body data.
// The body data has its own version, so these settings need their own part of the cache key.
func estimateParamsHash(u models.User) string {
	height := "-"
	if u.HeightCm != nil {
		height = fmt.Sprint(*u.HeightCm)
	}
	return hashToken(fmt.Sprintf("%s|%s|%t|%s", u.Timezone, strings.Join(u.MethodRanking, ","), u.AthleteDefault, height))[:12]
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
