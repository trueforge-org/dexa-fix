// Package estimate builds one estimated result per day from DEXA scans and scale/body-composition
// readings. Stored readings are only ever read, never changed.
//
// Per field, the estimate is built in these steps:
//
//  1. Readings are averaged per day and per group (measurement method + athlete mode).
//  2. For every day the most accurate group wins, using the user's method ranking.
//  3. Days without a reading are filled: straight-line interpolation between known days, and the
//     last known value held up to today.
//  4. The filled series is smoothed with a centred 7-day rolling average. This is our per-day
//     series; peaks are kept, not capped.
//  5. DEXA scans calibrate that series: the offset (dexa − series) is stored per group, because
//     e.g. an athlete-mode scale reading is biased differently from a normal-mode one.
//  6. Between two calibrations the offset is blended with a smoothstep curve, so the first half of
//     the gap stays close to the earlier DEXA scan and the second half moves towards the later one.
//
// For every field a DEXA value on its own day wins over the calibrated series.
package estimate

import (
	"sort"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// WindowDays is the number of days on each side of a day that the rolling average covers
// (3 + 1 + 3 = a 7-day window).
const WindowDays = 3

// Origin tells where an estimated value came from.
type Origin string

const (
	OriginDexa         Origin = "dexa"         // a DEXA scan on this day
	OriginMeasured     Origin = "measured"     // a reading exists for this day
	OriginInterpolated Origin = "interpolated" // between two days with readings
	OriginHeld         Origin = "held"         // after the last reading, repeating it
)

// Field describes one estimated measurement.
type Field struct {
	Key   string
	Label string
	Unit  string
	// Comparable fields are measured by both DEXA and the scale and so can be corrected.
	Comparable bool
}

// Fields lists the estimated measurements in display order.
var Fields = []Field{
	{"weight_kg", "Weight", "kg", true},
	{"fat_ratio_pct", "Body fat", "%", true},
	{"fat_mass_kg", "Fat mass", "kg", true},
	{"fat_free_mass_kg", "Fat-free mass", "kg", true},
	{"bone_mass_kg", "Bone mass", "kg", true},
	{"muscle_mass_kg", "Muscle mass", "kg", false},
	{"hydration_kg", "Hydration", "kg", false},
	{"visceral_fat_index", "Visceral fat index", "", false},
}

// Options configure Compute.
type Options struct {
	// Today is the last day of the series; the last known value is held up to it. The zero value
	// ends each series at its last reading.
	Today time.Time
	// Ranking lists methods, most accurate first. Empty means models.DefaultRanking.
	Ranking []string
	// AthleteDefault breaks ties between the normal and athlete group of the same method.
	AthleteDefault bool
}

// Value is an estimated value with its origin.
type Value struct {
	V      float64 `json:"value"`
	Origin Origin  `json:"origin"`
	// Raw is the selected raw daily reading, set only on days with a reading.
	Raw *float64 `json:"raw,omitempty"`
	// Method and AthleteMode name the reading group the value is based on (empty for DEXA).
	Method      string `json:"method,omitempty"`
	AthleteMode bool   `json:"athlete_mode,omitempty"`
	// Corrected is set when a DEXA offset was applied.
	Corrected bool `json:"corrected,omitempty"`
}

// DayEstimate is the estimate for a single day.
type DayEstimate struct {
	Date   time.Time        `json:"date"`
	Values map[string]Value `json:"values"`
}

// Calibration is the reading-to-DEXA offset (dexa − smoothed series) observed at a DEXA scan, for
// one reading group.
type Calibration struct {
	Date        time.Time          `json:"date"`
	Method      string             `json:"method"`
	AthleteMode bool               `json:"athlete_mode"`
	Offsets     map[string]float64 `json:"offsets"`
}

// Result is the output of Compute.
type Result struct {
	Days         []DayEstimate `json:"days"`         // newest first
	Calibrations []Calibration `json:"calibrations"` // oldest first
}

const dayDur = 24 * time.Hour

func day(t time.Time) time.Time { return t.UTC().Truncate(dayDur) }

// weight is the blend weight for the later calibration at fraction t (0..1) of the gap.
func weight(t float64) float64 { return t * t * (3 - 2*t) }

func scaleGet(r *models.ScaleReading, key string) *float64 {
	switch key {
	case "weight_kg":
		return r.WeightKg
	case "fat_ratio_pct":
		return r.FatRatioPct
	case "fat_mass_kg":
		return r.FatMassKg
	case "fat_free_mass_kg":
		return r.FatFreeMassKg
	case "bone_mass_kg":
		return r.BoneMassKg
	case "muscle_mass_kg":
		return r.MuscleMassKg
	case "hydration_kg":
		return r.HydrationKg
	case "visceral_fat_index":
		return r.VisceralFatIndex
	}
	return nil
}

func dexaGet(d *models.DexaScan, key string) *float64 {
	switch key {
	case "weight_kg":
		return d.WeightKg
	case "fat_ratio_pct":
		return d.FatRatioPct
	case "fat_mass_kg":
		return d.FatMassKg
	case "fat_free_mass_kg":
		return d.FatFreeMassKg
	case "bone_mass_kg":
		return d.BoneMassKg
	}
	return nil
}

// group is a measurement method combined with athlete mode.
type group struct {
	method  string
	athlete bool
}

// ranker orders groups by accuracy.
type ranker struct {
	idx            map[string]int
	athleteDefault bool
}

func newRanker(o Options) ranker {
	keys := models.NormalizeRanking(o.Ranking)
	r := ranker{idx: make(map[string]int, len(keys)), athleteDefault: o.AthleteDefault}
	for i, k := range keys {
		r.idx[k] = i
	}
	return r
}

func (r ranker) rank(m string) int {
	if i, ok := r.idx[m]; ok {
		return i
	}
	return len(r.idx)
}

// better reports whether a is more accurate than b. Same method: the athlete mode matching the
// user default wins. Remaining ties are broken deterministically.
func (r ranker) better(a, b group) bool {
	if ra, rb := r.rank(a.method), r.rank(b.method); ra != rb {
		return ra < rb
	}
	if a.method != b.method {
		return a.method < b.method
	}
	if a.athlete != b.athlete {
		return a.athlete == r.athleteDefault
	}
	return false
}

// daily averages each field over all readings of the same day and group, ignoring missing values.
// Result: field key → day → group → mean.
func daily(readings []models.ScaleReading) map[string]map[time.Time]map[group]float64 {
	type acc struct {
		sum float64
		n   int
	}
	sums := map[string]map[time.Time]map[group]*acc{}
	for i := range readings {
		r := &readings[i]
		d := day(r.MeasuredAt)
		g := group{r.Method, r.AthleteMode}
		if g.method == "" {
			g.method = models.MethodScale
		}
		for _, f := range Fields {
			v := scaleGet(r, f.Key)
			if v == nil {
				continue
			}
			if sums[f.Key] == nil {
				sums[f.Key] = map[time.Time]map[group]*acc{}
			}
			if sums[f.Key][d] == nil {
				sums[f.Key][d] = map[group]*acc{}
			}
			a := sums[f.Key][d][g]
			if a == nil {
				a = &acc{}
				sums[f.Key][d][g] = a
			}
			a.sum += *v
			a.n++
		}
	}
	out := make(map[string]map[time.Time]map[group]float64, len(sums))
	for k, byDay := range sums {
		out[k] = map[time.Time]map[group]float64{}
		for d, byGroup := range byDay {
			out[k][d] = map[group]float64{}
			for g, a := range byGroup {
				out[k][d][g] = a.sum / float64(a.n)
			}
		}
	}
	return out
}

// seriesDay is one day of a field's per-day series.
type seriesDay struct {
	origin Origin
	raw    float64 // selected raw daily reading, valid when origin == OriginMeasured
	smooth float64 // centred rolling average of the filled series
	grp    group   // group the value is based on (window group for non-measured days)
}

// series is a field's per-day series from first up to the end day.
type series struct {
	first time.Time
	days  []seriesDay
}

func (s *series) at(d time.Time) (*seriesDay, bool) {
	if s == nil || d.Before(s.first) {
		return nil, false
	}
	i := int(d.Sub(s.first) / dayDur)
	if i >= len(s.days) {
		return nil, false
	}
	return &s.days[i], true
}

// buildSeries selects the best reading per day, fills missing days and smooths the result.
func buildSeries(byDay map[time.Time]map[group]float64, rk ranker, today time.Time) *series {
	if len(byDay) == 0 {
		return nil
	}
	var known []time.Time
	for d := range byDay {
		known = append(known, d)
	}
	sort.Slice(known, func(i, j int) bool { return known[i].Before(known[j]) })
	first, last := known[0], known[len(known)-1]
	end := last
	if !today.IsZero() && day(today).After(end) {
		end = day(today)
	}
	n := int(end.Sub(first)/dayDur) + 1
	s := &series{first: first, days: make([]seriesDay, n)}

	// Step 2: most accurate group per measured day.
	var mi []int // indices of measured days
	filled := make([]float64, n)
	for _, d := range known {
		i := int(d.Sub(first) / dayDur)
		var best group
		haveBest := false
		for g := range byDay[d] {
			if !haveBest || rk.better(g, best) {
				best, haveBest = g, true
			}
		}
		v := byDay[d][best]
		s.days[i] = seriesDay{origin: OriginMeasured, raw: v, grp: best}
		filled[i] = v
		mi = append(mi, i)
	}

	// Step 3: interpolate gaps, hold the last value.
	for k := 0; k+1 < len(mi); k++ {
		a, b := mi[k], mi[k+1]
		for i := a + 1; i < b; i++ {
			t := float64(i-a) / float64(b-a)
			filled[i] = filled[a] + t*(filled[b]-filled[a])
			s.days[i].origin = OriginInterpolated
		}
	}
	lastIdx := mi[len(mi)-1]
	for i := lastIdx + 1; i < n; i++ {
		filled[i] = filled[lastIdx]
		s.days[i].origin = OriginHeld
	}

	// Step 4: centred rolling average, window shrinking at the ends of the series.
	for i := range s.days {
		lo, hi := max(0, i-WindowDays), min(n-1, i+WindowDays)
		sum := 0.0
		for j := lo; j <= hi; j++ {
			sum += filled[j]
		}
		s.days[i].smooth = sum / float64(hi-lo+1)
	}

	// Window group for filled days: most common measured group within the window, ties to the more
	// accurate one; with no measured day in the window, the group of the nearest measured day.
	for i := range s.days {
		if s.days[i].origin == OriginMeasured {
			continue
		}
		counts := map[group]int{}
		for j := max(0, i-WindowDays); j <= min(n-1, i+WindowDays); j++ {
			if s.days[j].origin == OriginMeasured {
				counts[s.days[j].grp]++
			}
		}
		var best group
		bestN := 0
		for g, c := range counts {
			if c > bestN || (c == bestN && rk.better(g, best)) {
				best, bestN = g, c
			}
		}
		if bestN == 0 {
			dist := n + 1
			for _, j := range mi {
				d := j - i
				if d < 0 {
					d = -d
				}
				if d < dist {
					dist, best = d, s.days[j].grp
				}
			}
		}
		s.days[i].grp = best
	}
	return s
}

type point struct {
	date   time.Time
	offset float64
}

type calKey struct {
	field string
	grp   group
}

// Compute returns the per-day estimates and the calibrations used. The inputs are not modified.
func Compute(scans []models.DexaScan, readings []models.ScaleReading, o Options) Result {
	rk := newRanker(o)
	byField := daily(readings)
	all := map[string]*series{}
	for _, f := range Fields {
		all[f.Key] = buildSeries(byField[f.Key], rk, o.Today)
	}

	sorted := append([]models.DexaScan(nil), scans...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := day(sorted[i].MeasuredAt), day(sorted[j].MeasuredAt)
		if !a.Equal(b) {
			return a.Before(b)
		}
		return sorted[i].ID < sorted[j].ID
	})

	// DEXA values per day; a later scan on the same day overrides per field.
	dexaDay := map[time.Time]map[string]float64{}
	for i := range sorted {
		s := &sorted[i]
		d := day(s.MeasuredAt)
		if dexaDay[d] == nil {
			dexaDay[d] = map[string]float64{}
		}
		for _, f := range Fields {
			if v := dexaGet(s, f.Key); v != nil {
				dexaDay[d][f.Key] = *v
			}
		}
	}

	// Calibrations: offset between each DEXA value and the smoothed series, per reading group.
	perKey := map[calKey][]point{}
	type calID struct {
		date time.Time
		grp  group
	}
	calOffsets := map[calID]map[string]float64{}
	var dexaDays []time.Time
	for d := range dexaDay {
		dexaDays = append(dexaDays, d)
	}
	sort.Slice(dexaDays, func(i, j int) bool { return dexaDays[i].Before(dexaDays[j]) })
	for _, d := range dexaDays {
		for _, f := range Fields {
			dv, ok := dexaDay[d][f.Key]
			if !ok || !f.Comparable {
				continue
			}
			sd, ok := all[f.Key].at(d)
			if !ok {
				continue
			}
			off := dv - sd.smooth
			perKey[calKey{f.Key, sd.grp}] = append(perKey[calKey{f.Key, sd.grp}], point{d, off})
			id := calID{d, sd.grp}
			if calOffsets[id] == nil {
				calOffsets[id] = map[string]float64{}
			}
			calOffsets[id][f.Key] = off
		}
	}
	var cals []Calibration
	for id, offs := range calOffsets {
		cals = append(cals, Calibration{Date: id.date, Method: id.grp.method, AthleteMode: id.grp.athlete, Offsets: offs})
	}
	sort.Slice(cals, func(i, j int) bool {
		a, b := cals[i], cals[j]
		if !a.Date.Equal(b.Date) {
			return a.Date.Before(b.Date)
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		return !a.AthleteMode && b.AthleteMode
	})

	// offsetAt returns the blended offset for a field and group on day d.
	offsetAt := func(f Field, g group, d time.Time) (float64, bool) {
		if !f.Comparable {
			return 0, false
		}
		pts := perKey[calKey{f.Key, g}]
		var c1, c2 *point
		for i := range pts {
			if !pts[i].date.After(d) {
				c1 = &pts[i]
			} else if c2 == nil {
				c2 = &pts[i]
			}
		}
		switch {
		case c1 == nil:
			return 0, false
		case c2 == nil:
			return c1.offset, true
		}
		t := float64(d.Sub(c1.date)) / float64(c2.date.Sub(c1.date))
		w := weight(t)
		return (1-w)*c1.offset + w*c2.offset, true
	}

	days := map[time.Time]bool{}
	for _, s := range all {
		if s == nil {
			continue
		}
		for i := range s.days {
			days[s.first.Add(time.Duration(i)*dayDur)] = true
		}
	}
	for d := range dexaDay {
		days[d] = true
	}
	res := Result{Calibrations: cals}
	for d := range days {
		est := DayEstimate{Date: d, Values: map[string]Value{}}
		for _, f := range Fields {
			if v, ok := dexaDay[d][f.Key]; ok {
				est.Values[f.Key] = Value{V: v, Origin: OriginDexa}
				continue
			}
			sd, ok := all[f.Key].at(d)
			if !ok {
				continue
			}
			v := Value{V: sd.smooth, Origin: sd.origin, Method: sd.grp.method, AthleteMode: sd.grp.athlete}
			if sd.origin == OriginMeasured {
				raw := sd.raw
				v.Raw = &raw
			}
			if off, ok := offsetAt(f, sd.grp, d); ok {
				v.V += off
				v.Corrected = true
			}
			est.Values[f.Key] = v
		}
		if len(est.Values) > 0 {
			res.Days = append(res.Days, est)
		}
	}
	sort.Slice(res.Days, func(i, j int) bool { return res.Days[i].Date.After(res.Days[j].Date) })
	return res
}
