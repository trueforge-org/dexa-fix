package mainbackend

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// A source of scale readings (Withings, and whatever comes after it) hands the backend what it
// found: readings with the source's name and its own id for each. This is the one place that
// decides what to do with them, the same for every source. A worker only fetches; it never sees
// the user's stored readings.

// importResult reports what an import did.
type importResult struct {
	Imported    int      `json:"imported"`               // new readings stored
	Updated     int      `json:"updated"`                // existing readings whose values changed
	Unchanged   int      `json:"unchanged"`              // readings already stored with identical values
	Removed     int      `json:"removed"`                // stored readings the source no longer returns (day re-import only)
	Skipped     int      `json:"skipped"`                // days left alone because they hold manual readings
	SkippedDays []string `json:"skipped_days,omitempty"` // those days as YYYY-MM-DD
}

// dayKey is the calendar date of t in loc (nil: UTC), the day a reading belongs to for the user.
func dayKey(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02")
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return math.Abs(*a-*b) < 1e-9
}

func sameReading(a, b models.ScaleReading) bool {
	return a.MeasuredAt.Equal(b.MeasuredAt) &&
		sameFloat(a.WeightKg, b.WeightKg) && sameFloat(a.FatRatioPct, b.FatRatioPct) &&
		sameFloat(a.FatMassKg, b.FatMassKg) && sameFloat(a.FatFreeMassKg, b.FatFreeMassKg) &&
		sameFloat(a.MuscleMassKg, b.MuscleMassKg) && sameFloat(a.HydrationKg, b.HydrationKg) &&
		sameFloat(a.BoneMassKg, b.BoneMassKg) && sameFloat(a.VisceralFatIndex, b.VisceralFatIndex)
}

// changes is the outcome of comparing fetched readings with what is stored.
type changes struct {
	// Gap are readings on days that had no readings at all; they are written first.
	Gap []models.ScaleReading
	// Other are new or changed readings on days that already had readings.
	Other []models.ScaleReading
	// Delete are ids of stored readings of the source to remove (day re-import only).
	Delete []int64
	importResult
}

type dayState struct {
	n       int  // readings of any source
	foreign bool // has readings from another source (manual entries)
	own     []models.ScaleReading
}

// plan decides what to write. Rules, per calendar day in the user's time zone loc (nil: UTC):
//   - a day without any readings is always filled (and written first);
//   - a stored reading of the source is updated when its values changed and left alone otherwise;
//   - new readings of the source on a day that holds manual readings are skipped in a bulk import
//     (averaging both would distort the day), but imported in a day re-import, which is an
//     explicit request;
//   - a day re-import also removes stored readings of the source that it no longer returns.
//
// source is the name the source's readings carry. existing is the user's stored readings, fetched
// the mapped readings of the source and seen the ids of all it returned. dayMode must be true when
// day is the single day that was fetched, given as a calendar date (a UTC midnight).
func plan(source string, existing, fetched []models.ScaleReading, seen map[string]bool, dayMode bool, day time.Time, loc *time.Location) changes {
	states := map[string]*dayState{}
	byExt := map[string]models.ScaleReading{} // stored readings of the source, any day
	for _, r := range existing {
		k := dayKey(r.MeasuredAt, loc)
		st := states[k]
		if st == nil {
			st = &dayState{}
			states[k] = st
		}
		st.n++
		if r.Source == source && r.ExternalID != nil {
			st.own = append(st.own, r)
			byExt[*r.ExternalID] = r
		} else {
			st.foreign = true
		}
	}

	var c changes
	skipped := map[string]bool{}
	for _, f := range fetched {
		st := states[dayKey(f.MeasuredAt, loc)]
		if old, ok := byExt[*f.ExternalID]; ok {
			// already stored (possibly on another day if the source moved it): refresh when changed
			if sameReading(old, f) {
				c.Unchanged++
			} else {
				c.Other = append(c.Other, f)
				c.Updated++
			}
			continue
		}
		switch {
		case st == nil || st.n == 0:
			c.Gap = append(c.Gap, f)
			c.Imported++
		case st.foreign && !dayMode:
			skipped[dayKey(f.MeasuredAt, loc)] = true
		default:
			c.Other = append(c.Other, f)
			c.Imported++
		}
	}

	if dayMode {
		if st := states[day.Format("2006-01-02")]; st != nil { // day is a calendar date
			for _, r := range st.own {
				if !seen[*r.ExternalID] {
					c.Delete = append(c.Delete, r.ID)
					c.Removed++
				}
			}
			sort.Slice(c.Delete, func(i, j int) bool { return c.Delete[i] < c.Delete[j] })
		}
	}
	for k := range skipped {
		c.SkippedDays = append(c.SkippedDays, k)
	}
	sort.Strings(c.SkippedDays)
	c.Skipped = len(c.SkippedDays)
	return c
}

// reconcileReadings stores what a source returned for a user, by the rules on plan. New readings
// follow the user's default athlete mode. Days without any readings go first so a failure further
// down still keeps them. Applying the same result twice changes nothing.
func (s *Backend) reconcileReadings(ctx context.Context, u models.User, source string, fetched []models.ScaleReading, seen map[string]bool, dayMode bool, day time.Time) (importResult, error) {
	for i := range fetched {
		fetched[i].AthleteMode = u.AthleteDefault
	}
	existing, err := s.store.ListScaleReadings(ctx, u.ID)
	if err != nil {
		return importResult{}, err
	}
	c := plan(source, existing, fetched, seen, dayMode, day, u.Location())
	if len(c.Gap) > 0 {
		if _, err := s.store.UpsertScaleReadings(ctx, u.ID, c.Gap); err != nil {
			return importResult{}, err
		}
	}
	if len(c.Other) > 0 {
		if _, err := s.store.UpsertScaleReadings(ctx, u.ID, c.Other); err != nil {
			return importResult{}, err
		}
	}
	for _, id := range c.Delete {
		if err := s.store.DeleteScaleReading(ctx, u.ID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return importResult{}, err
		}
	}
	return c.importResult, nil
}
