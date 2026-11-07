package withings

import (
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// attribUnsure is Withings' "measurement may belong to someone else" attribution (e.g. a guest on the scale).
const attribUnsure = 1

func dayOf(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

func dayKey(t time.Time) string { return dayOf(t).Format("2006-01-02") }

// mapGroups converts Withings groups to scale readings. seen holds the id of every group the API
// returned, including the ones that were dropped (guest measurements, groups without mapped
// types), so a day re-import never deletes a stored reading just because it is now filtered out.
func mapGroups(groups []measureGroup) (rs []models.ScaleReading, seen map[string]bool) {
	seen = make(map[string]bool, len(groups))
	for _, g := range groups {
		id := strconv.FormatInt(g.GrpID, 10)
		seen[id] = true
		if g.Category != 1 || g.Attrib == attribUnsure || g.Date <= 0 {
			continue
		}
		r := models.ScaleReading{Source: Name, ExternalID: &id, MeasuredAt: time.Unix(g.Date, 0).UTC()}
		mapped := 0
		for _, m := range g.Measures {
			v := math.Round(float64(m.Value)*math.Pow10(m.Unit)*1e6) / 1e6
			var dst **float64
			switch m.Type {
			case 1:
				dst = &r.WeightKg
			case 5:
				dst = &r.FatFreeMassKg
			case 6:
				dst = &r.FatRatioPct
			case 8:
				dst = &r.FatMassKg
			case 76:
				dst = &r.MuscleMassKg
			case 77:
				dst = &r.HydrationKg
			case 88:
				dst = &r.BoneMassKg
			case 170:
				dst = &r.VisceralFatIndex
			default:
				continue
			}
			*dst = &v
			mapped++
		}
		if mapped > 0 {
			rs = append(rs, r)
		}
	}
	return rs, seen
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
	// Delete are ids of stored provider readings to remove (day re-import only).
	Delete []int64
	importer.Result
}

type dayState struct {
	n       int  // readings of any source
	foreign bool // has readings from another source (manual entries)
	own     []models.ScaleReading
}

// plan decides what to write. Rules, per UTC day:
//   - a day without any readings is always filled (and written first);
//   - a stored provider reading is updated when its values changed and left alone otherwise;
//   - new provider readings on a day that holds manual readings are skipped in a bulk import
//     (averaging both would distort the day), but imported in a day re-import, which is an
//     explicit request;
//   - a day re-import also removes stored provider readings the provider no longer returns.
//
// existing is the user's stored readings, fetched the mapped provider readings and seen the ids of
// all provider groups returned. dayMode must be true when day is the single day that was fetched.
func plan(existing, fetched []models.ScaleReading, seen map[string]bool, dayMode bool, day time.Time) changes {
	states := map[string]*dayState{}
	byExt := map[string]models.ScaleReading{} // stored provider readings, any day
	for _, r := range existing {
		k := dayKey(r.MeasuredAt)
		st := states[k]
		if st == nil {
			st = &dayState{}
			states[k] = st
		}
		st.n++
		if r.Source == Name && r.ExternalID != nil {
			st.own = append(st.own, r)
			byExt[*r.ExternalID] = r
		} else {
			st.foreign = true
		}
	}

	var c changes
	skipped := map[string]bool{}
	for _, f := range fetched {
		st := states[dayKey(f.MeasuredAt)]
		if old, ok := byExt[*f.ExternalID]; ok {
			// already stored (possibly on another day if Withings moved it): refresh when changed
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
			skipped[dayKey(f.MeasuredAt)] = true
		default:
			c.Other = append(c.Other, f)
			c.Imported++
		}
	}

	if dayMode {
		if st := states[dayKey(day)]; st != nil {
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
