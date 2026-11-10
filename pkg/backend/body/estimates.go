package bodybackend

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/trueforge-org/dexafix/pkg/estimate"
	"github.com/trueforge-org/dexafix/pkg/shared/bodyapi"
)

const dateLayout = "2006-01-02"

// estimateKeyFor names the inputs of a user's estimates: the version of their data, the settings
// the main backend sent, and the day in their time zone.
func (s *Service) estimateKeyFor(r *http.Request, userID int64, p bodyapi.EstimateParams) (estimateKey, error) {
	v, err := s.store.BodyVersion(r.Context(), userID)
	if err != nil {
		return estimateKey{}, err
	}
	return estimateKey{version: v, params: paramsHash(p), day: s.now().In(p.Location()).Format(dateLayout)}, nil
}

// paramsHash digests the settings the estimates take besides the data. The data has its own
// version, so these need their own part of the cache key.
func paramsHash(p bodyapi.EstimateParams) string {
	height := "-"
	if p.HeightCm != nil {
		height = fmt.Sprint(*p.HeightCm)
	}
	return digest(fmt.Sprintf("%s|%s|%t|%s", p.Timezone, strings.Join(p.Ranking, ","), p.AthleteDefault, height), 12)
}

// estimateETag identifies the estimates answer for a user: it changes with their data and settings,
// the day and the running version of the program, and so lets callers keep their own copy.
func (s *Service) estimateETag(userID int64, k estimateKey) string {
	return fmt.Sprintf(`W/"est-%d-%d-%s-%s-%s"`, userID, k.version, k.params, k.day, digest(s.version, 8))
}

// estimates returns the user's estimates from the cache, or computes them from the whole history.
// The result is shared, so callers must not modify it.
func (s *Service) estimates(r *http.Request, userID int64, p bodyapi.EstimateParams, key estimateKey) (estimate.Result, error) {
	return s.est.get(userID, key, func() (estimate.Result, error) {
		release, ok := func() (func(), bool) {
			select {
			case s.estimateSlots <- struct{}{}:
				return func() { <-s.estimateSlots }, true
			case <-r.Context().Done():
				return nil, false
			}
		}()
		if !ok {
			return estimate.Result{}, r.Context().Err()
		}
		defer release()
		scans, err := s.store.ListDexaScans(r.Context(), userID)
		if err != nil {
			return estimate.Result{}, err
		}
		readings, err := s.store.ListScaleReadings(r.Context(), userID)
		if err != nil {
			return estimate.Result{}, err
		}
		now := s.now()
		return estimate.Compute(scans, readings, estimate.Options{
			Today:          now,
			Location:       p.Location(),
			Ranking:        p.Ranking,
			AthleteDefault: p.AthleteDefault,
			HeightCm:       p.HeightCm,
		}), nil
	})
}

// etagMatches reports whether an If-None-Match header lists the tag (or is "*"). The comparison is
// weak, as the spec requires for this header.
func etagMatches(header, etag string) bool {
	weak := func(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "W/") }
	for _, candidate := range strings.Split(header, ",") {
		if c := strings.TrimSpace(candidate); c == "*" || weak(c) == weak(etag) {
			return true
		}
	}
	return false
}
