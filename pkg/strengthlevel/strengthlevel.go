// Package strengthlevel prepares the daily estimates for the Strength Level training log
// (my.strengthlevel.com). It has no dependencies on the store or the web layer; the opt-in
// transport that sends them lives in the client subpackage.
package strengthlevel

import (
	"sort"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
)

// DayLayout is the layout of Entry.Day keys.
const DayLayout = "2006-01-02"

// Entry is the body weight and body fat of one calendar day.
type Entry struct {
	Day      string // YYYY-MM-DD
	WeightKg float64
	FatPct   *float64 // nil when the day has no body fat estimate
}

// FromEstimates turns daily estimates into entries, newest day first. Every estimated value counts,
// whatever its origin: measured, interpolated between readings, held after the last one, or from a
// DEXA scan. Days without a weight estimate are left out.
func FromEstimates(days []estimate.DayEstimate) []Entry {
	out := make([]Entry, 0, len(days))
	for _, d := range days {
		w, ok := d.Values["weight_kg"]
		if !ok {
			continue
		}
		e := Entry{Day: d.Date.UTC().Format(DayLayout), WeightKg: w.V}
		if f, ok := d.Values["fat_ratio_pct"]; ok {
			v := f.V
			e.FatPct = &v
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day > out[j].Day })
	return out
}
