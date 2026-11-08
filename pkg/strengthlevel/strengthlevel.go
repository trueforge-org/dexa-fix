// Package strengthlevel prepares body readings for the Strength Level training log
// (my.strengthlevel.com). It has no dependencies on the store or the web layer.
//
// Strength Level publishes no API, and its terms forbid automated access without written
// permission. This package only reduces readings to one entry per day; the opt-in transport that
// sends them lives in its own subpackage.
package strengthlevel

import (
	"fmt"
	"sort"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// DefaultURL is the Strength Level training log, where a body weight is entered.
const DefaultURL = "https://my.strengthlevel.com/"

// DayLayout is the layout of Entry.Day keys.
const DayLayout = "2006-01-02"

// Entry is the body weight and body fat of one calendar day.
type Entry struct {
	Day      string // YYYY-MM-DD
	WeightKg float64
	FatPct   *float64 // nil when no reading of the day has a body fat value
}

// DailyEntries reduces readings to one entry per UTC day, newest day first. A day takes the
// weight of its latest reading that has one. Its body fat comes from that reading, or else from
// the latest reading of the day that has one. Days without any weight are left out.
func DailyEntries(readings []models.ScaleReading) []Entry {
	type best struct {
		weightAt, fatAt time.Time
		entry           Entry
		hasFat          bool
	}
	days := map[string]*best{}
	for i := range readings {
		r := &readings[i]
		at := r.MeasuredAt.UTC()
		key := at.Format(DayLayout)
		b := days[key]
		if b == nil {
			b = &best{entry: Entry{Day: key}}
			days[key] = b
		}
		if r.WeightKg != nil && (b.weightAt.IsZero() || at.After(b.weightAt)) {
			b.weightAt, b.entry.WeightKg = at, *r.WeightKg
		}
		if r.FatRatioPct != nil && (!b.hasFat || at.After(b.fatAt)) {
			v := *r.FatRatioPct
			b.fatAt, b.entry.FatPct, b.hasFat = at, &v, true
		}
	}
	out := make([]Entry, 0, len(days))
	for _, b := range days {
		if !b.weightAt.IsZero() {
			out = append(out, b.entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day > out[j].Day })
	return out
}

// Weight formats the weight in kilograms with one decimal.
func (e Entry) Weight() string { return fmt.Sprintf("%.1f", e.WeightKg) }

// Fat formats the body fat percentage with one decimal, or "" when there is none.
func (e Entry) Fat() string {
	if e.FatPct == nil {
		return ""
	}
	return fmt.Sprintf("%.1f", *e.FatPct)
}

// ValidDay reports whether s is a YYYY-MM-DD date.
func ValidDay(s string) bool {
	t, err := time.Parse(DayLayout, s)
	return err == nil && t.Format(DayLayout) == s
}
