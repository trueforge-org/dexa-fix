// Package estimate builds one estimated result per day from DEXA scans and smart-scale readings.
//
// Before anything else the averaged daily scale series is rate-limited per field (see
// Field.MaxDailyChange): scales are noisy, and e.g. muscle mass cannot realistically move more than
// 0.05 kg per day. The limited series is what DEXA calibration and the output are based on.
//
// For every field a DEXA value wins. Otherwise the scale value is used, corrected by the offset
// (dexa − scale) observed at nearby DEXA scans. Between two calibrations the offset is blended
// with a smoothstep curve, so the first half of the gap stays close to the earlier DEXA scan and
// the second half moves towards the later one.
package estimate

import (
	"sort"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// PairWindowDays is how far (in days) a scale day may be from a DEXA scan to calibrate against it.
const PairWindowDays = 3

// Origin tells where an estimated value came from.
type Origin string

const (
	OriginDexa           Origin = "dexa"
	OriginScaleCorrected Origin = "scale-corrected"
	OriginScale          Origin = "scale"
)

// Field describes one estimated measurement.
type Field struct {
	Key   string
	Label string
	Unit  string
	// Comparable fields are measured by both DEXA and the scale and so can be corrected.
	Comparable bool
	// MaxDailyChange is the largest plausible change of the daily scale value per day, in Unit.
	// The allowance grows with the number of days between two measured days. 0 means unlimited.
	MaxDailyChange float64
}

// Fields lists the estimated measurements in display order.
var Fields = []Field{
	{"weight_kg", "Weight", "kg", true, 1.0},
	{"fat_ratio_pct", "Body fat", "%", true, 0.3},
	{"fat_mass_kg", "Fat mass", "kg", true, 0.2},
	{"fat_free_mass_kg", "Fat-free mass", "kg", true, 0.5},
	{"bone_mass_kg", "Bone mass", "kg", true, 0.005},
	{"muscle_mass_kg", "Muscle mass", "kg", false, 0.05},
	{"hydration_kg", "Hydration", "kg", false, 1.0},
	{"visceral_fat_index", "Visceral fat index", "", false, 0.2},
}

// Value is an estimated value with its origin.
type Value struct {
	V      float64 `json:"value"`
	Origin Origin  `json:"origin"`
	// Limited is set when the underlying scale value was changed by the daily-change limit.
	Limited bool `json:"limited,omitempty"`
}

// DayEstimate is the estimate for a single day.
type DayEstimate struct {
	Date   time.Time        `json:"date"`
	Values map[string]Value `json:"values"`
}

// Calibration is the scale-to-DEXA offset (dexa − scale) observed at a DEXA scan.
type Calibration struct {
	Date    time.Time          `json:"date"`
	Offsets map[string]float64 `json:"offsets"`
}

// Result is the output of Compute.
type Result struct {
	Days         []DayEstimate `json:"days"`         // newest first
	Calibrations []Calibration `json:"calibrations"` // oldest first
}

func day(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

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

type scaleVal struct {
	v       float64
	limited bool
}

// dailyScale averages each field over all readings of the same day, ignoring missing values, and
// then limits the change between consecutive measured days to Field.MaxDailyChange per day.
func dailyScale(readings []models.ScaleReading) map[time.Time]map[string]scaleVal {
	type acc struct {
		sum float64
		n   int
	}
	sums := map[time.Time]map[string]*acc{}
	for i := range readings {
		r := &readings[i]
		d := day(r.MeasuredAt)
		if sums[d] == nil {
			sums[d] = map[string]*acc{}
		}
		for _, f := range Fields {
			if v := scaleGet(r, f.Key); v != nil {
				a := sums[d][f.Key]
				if a == nil {
					a = &acc{}
					sums[d][f.Key] = a
				}
				a.sum += *v
				a.n++
			}
		}
	}
	out := make(map[time.Time]map[string]scaleVal, len(sums))
	for d, m := range sums {
		out[d] = map[string]scaleVal{}
		for k, a := range m {
			out[d][k] = scaleVal{v: a.sum / float64(a.n)}
		}
	}
	for _, f := range Fields {
		if f.MaxDailyChange <= 0 {
			continue
		}
		var ds []time.Time
		for d, m := range out {
			if _, ok := m[f.Key]; ok {
				ds = append(ds, d)
			}
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i].Before(ds[j]) })
		for i := 1; i < len(ds); i++ {
			prev := out[ds[i-1]][f.Key].v
			step := f.MaxDailyChange * ds[i].Sub(ds[i-1]).Hours() / 24
			cur := out[ds[i]][f.Key]
			switch {
			case cur.v > prev+step:
				cur = scaleVal{v: prev + step, limited: true}
			case cur.v < prev-step:
				cur = scaleVal{v: prev - step, limited: true}
			}
			out[ds[i]][f.Key] = cur
		}
	}
	return out
}

// nearestScaleDay returns the scale day closest to d within the pairing window.
func nearestScaleDay(d time.Time, scale map[time.Time]map[string]scaleVal) (time.Time, bool) {
	var best time.Time
	bestDist := time.Duration(PairWindowDays)*24*time.Hour + 1
	found := false
	for sd := range scale {
		dist := sd.Sub(d)
		if dist < 0 {
			dist = -dist
		}
		if dist < bestDist || (dist == bestDist && sd.Before(best)) {
			best, bestDist, found = sd, dist, true
		}
	}
	return best, found
}

type point struct {
	date   time.Time
	offset float64
}

// Compute returns the per-day estimates and the calibrations used.
func Compute(scans []models.DexaScan, readings []models.ScaleReading) Result {
	scale := dailyScale(readings)

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
	var cals []Calibration
	perField := map[string][]point{}
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
		sd, ok := nearestScaleDay(d, scale)
		if !ok {
			continue
		}
		cal := Calibration{Date: d, Offsets: map[string]float64{}}
		for _, f := range Fields {
			dv := dexaGet(s, f.Key)
			sv, hasScale := scale[sd][f.Key]
			if f.Comparable && dv != nil && hasScale {
				off := *dv - sv.v
				cal.Offsets[f.Key] = off
				perField[f.Key] = append(perField[f.Key], point{d, off})
			}
		}
		if len(cal.Offsets) > 0 {
			cals = append(cals, cal)
		}
	}

	// corrected returns the scale value for a field and day with the blended offset applied.
	corrected := func(f Field, d time.Time, raw float64) Value {
		if !f.Comparable {
			return Value{V: raw, Origin: OriginScale}
		}
		pts := perField[f.Key]
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
			return Value{V: raw, Origin: OriginScale}
		case c2 == nil:
			return Value{V: raw + c1.offset, Origin: OriginScaleCorrected}
		}
		t := float64(d.Sub(c1.date)) / float64(c2.date.Sub(c1.date))
		w := weight(t)
		return Value{V: raw + (1-w)*c1.offset + w*c2.offset, Origin: OriginScaleCorrected}
	}

	days := map[time.Time]bool{}
	for d := range scale {
		days[d] = true
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
			} else if sv, ok := scale[d][f.Key]; ok {
				v := corrected(f, d, sv.v)
				v.Limited = sv.limited
				est.Values[f.Key] = v
			}
		}
		if len(est.Values) > 0 {
			res.Days = append(res.Days, est)
		}
	}
	sort.Slice(res.Days, func(i, j int) bool { return res.Days[i].Date.After(res.Days[j].Date) })
	return res
}
