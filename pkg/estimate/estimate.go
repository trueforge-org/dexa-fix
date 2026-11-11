// Package estimate builds one estimated result per day from DEXA scans and scale/body-composition
// readings. Stored readings are only ever read, never changed.
//
// Per field, the estimate is built in these steps:
//
//  1. Readings are averaged per day and per group (measurement method + athlete mode).
//  2. For every day the most accurate group wins, using the user's method ranking.
//  3. A day with at least MinWindowReadings real readings in its centred 7-day window gets the
//     average of those readings; interpolated days are never part of an average. Any other day
//     with a reading keeps that reading. Peaks are kept, not capped.
//  4. All remaining days are a straight line between the days from step 3, and the last of those
//     is held up to today. Together this is our per-day series.
//  5. DEXA scans calibrate that series: the offset (dexa − the day's reading) is stored per group,
//     because e.g. an athlete-mode scale reading is biased differently from a normal-mode one. A
//     group needs its own reading on the scan day to get an offset; the 7-day average is not used.
//  6. Between two calibrations the offset is blended with a smoothstep curve, so the first half of
//     the gap stays close to the earlier DEXA scan and the second half moves towards the later one.
//     Days before a group's first calibration use its offset, days after the last one use that one.
//  7. Only body fat % and bone mass have offsets. Fat mass is calculated from body fat % and the
//     day's weight, and fat-free mass is the weight minus that fat mass, so the two add up to the
//     weight. The scale's own values of these two are only used when they can't be calculated.
//     Lean mass is the fat-free mass minus the bone mass.
//
// For every field a DEXA value on its own day wins over the calibrated series.
package estimate

import (
	"sort"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// WindowDays is the number of days on each side of a day that the rolling average covers
// (3 + 1 + 3 = a 7-day window).
const WindowDays = 3

// MinWindowReadings is how many days with a real reading a window needs before it is averaged.
const MinWindowReadings = 3

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
	// Comparable fields are corrected with a DEXA offset: only body fat % and bone mass. Fat mass
	// and fat-free mass are calculated from the corrected body fat % instead, and weight is
	// deliberately left uncorrected.
	Comparable bool
}

// Fields lists the estimated measurements in display order.
var Fields = []Field{
	{"weight_kg", "Weight", "kg", false},
	{"fat_ratio_pct", "Body fat", "%", true},
	{"fat_mass_kg", "Fat mass", "kg", false},
	{"fat_free_mass_kg", "Fat-free mass", "kg", false},
	{"bone_mass_kg", "Bone mass", "kg", true},
	{"lean_mass_kg", "Lean mass", "kg", false},
	{"hydration_kg", "Hydration", "kg", false},
	{"visceral_fat_index", "Visceral fat index", "", false},
	{"bmi", "BMI", "kg/m²", false},
	{"fmi", "FMI", "kg/m²", false},
	{"lmi", "LMI", "kg/m²", false},
	{"ag_ratio", "A/G ratio", "", false},
}

// Options configure Compute.
type Options struct {
	// Today is the last day of the series (the day it falls on in Location); the last known value
	// is held up to it. The zero value ends each series at its last reading.
	Today time.Time
	// Location is the user's time zone: it decides which calendar day a scale reading belongs to.
	// Nil means UTC. DEXA scans are plain dates and are not moved by it.
	Location *time.Location
	// Ranking lists methods, most accurate first. Empty means models.DefaultRanking.
	Ranking []string
	// AthleteDefault breaks ties between the normal and athlete group of the same method.
	AthleteDefault bool
	// HeightCm is needed for BMI, FMI and LMI. Without it they are left out.
	HeightCm *float64
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

// Calibration is the reading-to-DEXA offset (dexa − daily reading) observed at a DEXA scan, for one
// reading group.
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

// WithinDays returns the part of days (newest first) within n days of the newest day, the newest
// day counting as the first. An n of 0 or less returns all of them. The result shares the slice.
func WithinDays(days []DayEstimate, n int) []DayEstimate {
	if n <= 0 || len(days) == 0 {
		return days
	}
	cutoff := days[0].Date.AddDate(0, 0, -(n - 1))
	k := 0
	for k < len(days) && !days[k].Date.Before(cutoff) {
		k++
	}
	return days[:k]
}

const dayDur = 24 * time.Hour

// day is the calendar date of t in UTC, as a UTC midnight. It is used for DEXA scans, which are
// stored as plain dates.
func day(t time.Time) time.Time { return t.UTC().Truncate(dayDur) }

// localDay is the calendar date of t in loc, as a UTC midnight, so days from different time zones
// can be compared and stepped through in whole 24-hour units.
func localDay(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		return day(t)
	}
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

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
	case "lean_mass_kg":
		return d.LeanMassKg
	case "ag_ratio":
		if d.AndroidFatPct != nil && d.GynoidFatPct != nil && *d.GynoidFatPct > 0 {
			ratio := *d.AndroidFatPct / *d.GynoidFatPct
			return &ratio
		}
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
func daily(readings []models.ScaleReading, loc *time.Location) map[string]map[time.Time]map[group]float64 {
	type acc struct {
		sum float64
		n   int
	}
	sums := map[string]map[time.Time]map[group]*acc{}
	for i := range readings {
		r := &readings[i]
		d := localDay(r.MeasuredAt, loc)
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
	smooth float64 // average of the real readings in the window, or interpolated between such days
	grp    group   // group the value is based on (window group for non-measured days)
	// mix counts the groups of the real readings in the window, when more than one group is in
	// it. The offset then is the average of those groups' own offsets, not that of grp alone.
	mix map[group]int
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
		mi = append(mi, i)
	}

	// Days without a reading are interpolated between readings, or held after the last one.
	for k := 0; k+1 < len(mi); k++ {
		for i := mi[k] + 1; i < mi[k+1]; i++ {
			s.days[i].origin = OriginInterpolated
		}
	}
	for i := mi[len(mi)-1] + 1; i < n; i++ {
		s.days[i].origin = OriginHeld
	}

	// Step 3: anchor days. A day with enough real readings in its window gets their average; any
	// other day with a reading keeps that reading. Filled days never enter an average.
	var anchors []int
	for i := range s.days {
		sum, count := 0.0, 0
		for j := max(0, i-WindowDays); j <= min(n-1, i+WindowDays); j++ {
			if s.days[j].origin == OriginMeasured {
				sum += s.days[j].raw
				count++
			}
		}
		switch {
		case count >= MinWindowReadings:
			s.days[i].smooth = sum / float64(count)
		case s.days[i].origin == OriginMeasured:
			s.days[i].smooth = s.days[i].raw
		default:
			continue
		}
		anchors = append(anchors, i)
	}

	// Step 4: the remaining days are a straight line between anchors, then the last anchor is held.
	for k := 0; k+1 < len(anchors); k++ {
		a, b := anchors[k], anchors[k+1]
		for i := a + 1; i < b; i++ {
			t := float64(i-a) / float64(b-a)
			s.days[i].smooth = s.days[a].smooth + t*(s.days[b].smooth-s.days[a].smooth)
		}
	}
	lastAnchor := anchors[len(anchors)-1]
	for i := lastAnchor + 1; i < n; i++ {
		s.days[i].smooth = s.days[lastAnchor].smooth
	}

	// Window group for filled days: most common measured group within the window, ties to the more
	// accurate one; with no measured day in the window, the group of the nearest measured day.
	for i := range s.days {
		counts := map[group]int{}
		total := 0
		for j := max(0, i-WindowDays); j <= min(n-1, i+WindowDays); j++ {
			if s.days[j].origin == OriginMeasured {
				counts[s.days[j].grp]++
				total++
			}
		}
		if s.days[i].origin == OriginMeasured {
			// A measured day below the window minimum keeps its own reading, so no mix.
			if total >= MinWindowReadings && len(counts) > 1 {
				s.days[i].mix = counts
			}
			continue
		}
		if len(counts) > 1 {
			s.days[i].mix = counts
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

// deriveFromBodyFat makes a day's fat mass and fat-free mass add up to its weight. Fat mass is
// calculated from body fat % and weight when both are known, otherwise the existing fat mass is
// kept. Fat-free mass is the weight minus that fat mass. The scale's own values of these two are
// replaced; a DEXA value of a mass itself is kept. Without a weight nothing changes.
func deriveFromBodyFat(vals map[string]Value) {
	w, haveW := vals["weight_kg"]
	if !haveW {
		return
	}
	fm, haveFM := vals["fat_mass_kg"]
	if fat, haveFat := vals["fat_ratio_pct"]; haveFat && fm.Origin != OriginDexa {
		fm = calculatedValue(fat, w.V*fat.V/100)
		vals["fat_mass_kg"] = fm
		haveFM = true
	}
	ffm := vals["fat_free_mass_kg"]
	if haveFM && ffm.Origin != OriginDexa {
		vals["fat_free_mass_kg"] = calculatedValue(fm, w.V-fm.V)
	}
}

// deriveLeanMass sets a day's lean mass to its fat-free mass minus its bone mass. A DEXA value of
// the lean mass itself is kept. Without both inputs nothing is calculated.
func deriveLeanMass(vals map[string]Value) {
	ffm, haveFFM := vals["fat_free_mass_kg"]
	bone, haveBone := vals["bone_mass_kg"]
	if !haveFFM || !haveBone || vals["lean_mass_kg"].Origin == OriginDexa {
		return
	}
	lean := calculatedValue(ffm, ffm.V-bone.V)
	lean.Corrected = lean.Corrected || bone.Corrected || bone.Origin == OriginDexa
	vals["lean_mass_kg"] = lean
}

// deriveBodyIndices sets BMI, FMI and LMI: weight, fat mass and lean mass divided by height squared.
func deriveBodyIndices(vals map[string]Value, heightCm *float64) {
	if heightCm == nil || *heightCm <= 0 {
		return
	}
	metres := *heightCm / 100
	for _, index := range []struct{ key, from string }{
		{"bmi", "weight_kg"}, {"fmi", "fat_mass_kg"}, {"lmi", "lean_mass_kg"},
	} {
		if source, ok := vals[index.from]; ok {
			vals[index.key] = calculatedValue(source, source.V/(metres*metres))
		}
	}
}

// calculatedValue returns value as a Value calculated from source: it takes over the origin and
// group of source, and counts as corrected when source is corrected or comes from DEXA.
func calculatedValue(source Value, value float64) Value {
	// The number is calculated from source, so its origin, group and raw reading are source's too:
	// an own reading of the same field (a scale's fat mass) would otherwise be shown next to a
	// value it did not produce.
	v := Value{Origin: source.Origin, Method: source.Method, AthleteMode: source.AthleteMode}
	if v.Origin == OriginDexa {
		v.Origin = OriginMeasured // calculated, not a value of the scan itself
	}
	v.V, v.Corrected = value, source.Corrected || source.Origin == OriginDexa
	return v
}

// Compute returns the per-day estimates and the calibrations used. The inputs are not modified.
func Compute(scans []models.DexaScan, readings []models.ScaleReading, o Options) Result {
	rk := newRanker(o)
	byField := daily(readings, o.Location)
	var today time.Time
	if !o.Today.IsZero() {
		today = localDay(o.Today, o.Location)
	}
	all := map[string]*series{}
	for _, f := range Fields {
		all[f.Key] = buildSeries(byField[f.Key], rk, today)
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
	// A scan without a body fat % still calibrates it, through its fat mass and weight.
	for _, vals := range dexaDay {
		w, haveW := vals["weight_kg"]
		fm, haveFM := vals["fat_mass_kg"]
		if _, havePct := vals["fat_ratio_pct"]; !havePct && haveFM {
			// Fat mass over weight; without the scan's weight, over fat plus fat-free mass.
			total := w
			if !haveW {
				ffm, haveFFM := vals["fat_free_mass_kg"]
				if !haveFFM {
					continue
				}
				total = fm + ffm
			}
			if total > 0 {
				vals["fat_ratio_pct"] = fm / total * 100
			}
		}
	}

	// Calibrations: offset between each DEXA value and the group's own reading on that day. A group
	// without a reading on the scan day gets no calibration from that scan.
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
			for g, measured := range byField[f.Key][d] {
				off := dv - measured
				perKey[calKey{f.Key, g}] = append(perKey[calKey{f.Key, g}], point{d, off})
				id := calID{d, g}
				if calOffsets[id] == nil {
					calOffsets[id] = map[string]float64{}
				}
				calOffsets[id][f.Key] = off
			}
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
		case c1 == nil && c2 == nil:
			return 0, false
		case c1 == nil: // before the group's first calibration
			return c2.offset, true
		case c2 == nil:
			return c1.offset, true
		}
		t := float64(d.Sub(c1.date)) / float64(c2.date.Sub(c1.date))
		w := weight(t)
		return (1-w)*c1.offset + w*c2.offset, true
	}

	// offsetMix averages the offsets of the groups behind a day's value, so a window that mixes
	// methods applies each reading's own method offset. Groups without a calibration count as 0.
	offsetMix := func(f Field, sd *seriesDay, d time.Time) (float64, bool) {
		if len(sd.mix) < 2 {
			return offsetAt(f, sd.grp, d)
		}
		sum, n, any := 0.0, 0, false
		for g, c := range sd.mix {
			if off, ok := offsetAt(f, g, d); ok {
				sum += float64(c) * off
				any = true
			}
			n += c
		}
		return sum / float64(n), any
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
			if off, ok := offsetMix(f, sd, d); ok {
				v.V += off
				v.Corrected = true
				// An offset must not push a value outside what is physically possible.
				v.V = max(v.V, 0)
				if f.Unit == "%" {
					v.V = min(v.V, 100)
				}
			}
			est.Values[f.Key] = v
		}
		deriveFromBodyFat(est.Values)
		deriveLeanMass(est.Values)
		deriveBodyIndices(est.Values, o.HeightCm)
		if _, ok := est.Values["ag_ratio"]; !ok {
			// A/G is only measured by DEXA: keep the latest earlier scan's ratio.
			for i := len(dexaDays) - 1; i >= 0; i-- {
				if v, has := dexaDay[dexaDays[i]]["ag_ratio"]; has && dexaDays[i].Before(d) {
					est.Values["ag_ratio"] = Value{V: v, Origin: OriginHeld}
					break
				}
			}
		}
		if len(est.Values) > 0 {
			res.Days = append(res.Days, est)
		}
	}
	sort.Slice(res.Days, func(i, j int) bool { return res.Days[i].Date.After(res.Days[j].Date) })
	return res
}
