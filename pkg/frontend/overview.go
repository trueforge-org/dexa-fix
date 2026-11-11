package frontend

import (
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/trueforge-org/dexafix/pkg/estimate"
)

const (
	// deltaDays is how far back a tile compares; deltaSlackDays is how much older than that the
	// comparison day may be before the change is left out.
	deltaDays      = 30
	deltaSlackDays = 7

	// maxHitColumns caps the hover targets in the chart. The line itself keeps every point.
	maxHitColumns = 240

	chartWidth      = 960.0
	chartHeight     = 300.0
	chartLeft       = 56.0
	chartRight      = chartWidth - 12
	chartTop        = 14.0
	chartBottom     = chartHeight - 30
	chartYTickCount = 5
	chartXTickCount = 4

	tipWidth  = 176.0
	tipHeight = 66.0
	tipGap    = 12.0
)

// tileFields are the measurements summarised at the top of the estimates page.
var tileFields = []string{"weight_kg", "fat_ratio_pct", "fat_mass_kg", "fat_free_mass_kg"}

// chartRange is a selectable time window of the chart and the daily table.
type chartRange struct {
	Key, Label, Desc string
	Days             int
}

// chartRanges are the selectable windows. Days 0 means the whole series.
var chartRanges = []chartRange{
	{"30", "30d", "the last 30 days", 30},
	{"90", "90d", "the last 90 days", 90},
	{"365", "1y", "the last year", 365},
	{"all", "All", "all time", 0},
}

// overviewView is the summary shown above the estimates table.
type overviewView struct {
	Tiles   []tileView
	Metrics []chartOption
	Ranges  []chartOption
	Chart   *chartView
	Note    string // shown instead of the chart when there is too little data
}

type tileView struct {
	Label  string
	Value  string
	Unit   string
	Origin string
	Arrow  string // empty when there is nothing to compare with
	Delta  string
}

// chartOption is a metric or range link above the chart.
type chartOption struct {
	Label  string
	Href   string
	Active bool
}

// chartView holds a chart's geometry, ready to be written out as SVG.
type chartView struct {
	Summary string // text alternative of the chart

	Width, Height         float64
	PlotLeft, PlotRight   float64
	PlotTop, PlotBottom   float64
	PlotHeight            float64
	TipWidth, TipHeight   float64
	YLabelX, XLabelY      float64
	YTicks                []yTick
	XTicks                []xTick
	Solid, Dashed         string // SVG path data
	Markers               []chartMark
	Hits                  []hitColumn
	HasDashed, HasMarkers bool
}

type yTick struct {
	Y, LabelY float64
	Label     string
}

type xTick struct {
	X      float64
	Label  string
	Anchor string
}

type chartMark struct{ X, Y float64 }

// hitColumn is the hover target of one day, with the crosshair and tooltip it reveals.
type hitColumn struct {
	Left, Width            float64
	X, Y                   float64
	TipX, TipY             float64
	TextX                  float64
	DateY, ValueY, OriginY float64
	Date, Value, Origin    string
}

type chartPoint struct {
	date  time.Time
	value estimate.Value
}

// selectedRange returns the range with the given key, or 90 days for an unknown key.
func selectedRange(key string) chartRange {
	for _, r := range chartRanges {
		if r.Key == key {
			return r
		}
	}
	return chartRanges[1]
}

// pageDays is how many days of estimates the estimates page needs for a chart range, so that the
// rest need not cross the network: the range itself, and at least what the tiles compare with
// (deltaDays back, plus the slack, plus the newest day). 0 means every day.
func pageDays(r chartRange) int {
	if r.Days <= 0 {
		return 0
	}
	return max(r.Days, deltaDays+deltaSlackDays+1)
}

// daysInRange returns the part of days (newest first) within rangeDays of the newest day. A
// rangeDays of 0 returns all of them.
func daysInRange(days []estimate.DayEstimate, rangeDays int) []estimate.DayEstimate {
	return estimate.WithinDays(days, rangeDays)
}

// buildOverview summarises a result for the given chart selection. Unknown selections fall back
// to the defaults, and nil is returned when there is nothing to summarise.
func buildOverview(res estimate.Result, userID int64, metric, rng string) *overviewView {
	if len(res.Days) == 0 {
		return nil
	}
	field := fieldOrDefault(metric)
	window := selectedRange(rng)

	ov := &overviewView{}
	for _, key := range tileFields {
		if t, ok := buildTile(res.Days, fieldOrDefault(key)); ok {
			ov.Tiles = append(ov.Tiles, t)
		}
	}
	for _, f := range estimate.Fields {
		ov.Metrics = append(ov.Metrics, chartOption{Label: f.Label, Href: overviewHref(userID, f.Key, window.Key), Active: f.Key == field.Key})
	}
	for _, r := range chartRanges {
		ov.Ranges = append(ov.Ranges, chartOption{Label: r.Label, Href: overviewHref(userID, field.Key, r.Key), Active: r.Key == window.Key})
	}

	points := fieldSeries(res.Days, field.Key, window.Days)
	if len(points) < 2 {
		ov.Note = fmt.Sprintf("Not enough %s data in this range to draw a trend.", strings.ToLower(field.Label))
		return ov
	}
	ov.Chart = buildChart(points, field, window.Desc)
	return ov
}

// fieldOrDefault finds a field by key, falling back to the first one (weight).
func fieldOrDefault(key string) estimate.Field {
	for _, f := range estimate.Fields {
		if f.Key == key {
			return f
		}
	}
	return estimate.Fields[0]
}

func overviewHref(userID int64, metric, rng string) string {
	return userPath(userID, "estimates") + "?" + url.Values{"metric": {metric}, "range": {rng}}.Encode()
}

// buildTile summarises the newest value of a field; days are newest first.
func buildTile(days []estimate.DayEstimate, f estimate.Field) (tileView, bool) {
	for i, d := range days {
		v, ok := d.Values[f.Key]
		if !ok {
			continue
		}
		t := tileView{Label: f.Label, Value: fmt.Sprintf("%.1f", v.V), Unit: f.Unit, Origin: tileOrigin(v)}
		if prev, age, ok := valueAround(days[i+1:], d.Date, f.Key); ok {
			t.Arrow, t.Delta = describeDelta(v.V-prev, f.Unit, age)
		}
		return t, true
	}
	return tileView{}, false
}

func tileOrigin(v estimate.Value) string {
	var s string
	switch v.Origin {
	case estimate.OriginDexa:
		return "DEXA scan"
	case estimate.OriginHeld:
		if !v.Corrected {
			return "Held since last reading"
		}
		s = "Held"
	case estimate.OriginInterpolated:
		s = "Interpolated"
	default:
		s = "Measured"
	}
	if v.Corrected {
		s += ", corrected to DEXA"
	}
	return s
}

// valueAround finds the value about deltaDays before latest; older is newest first.
func valueAround(older []estimate.DayEstimate, latest time.Time, key string) (float64, int, bool) {
	target := latest.AddDate(0, 0, -deltaDays)
	for _, d := range older {
		if d.Date.After(target) {
			continue
		}
		if daysBetween(d.Date, target) > deltaSlackDays {
			return 0, 0, false
		}
		if v, ok := d.Values[key]; ok {
			return v.V, daysBetween(d.Date, latest), true
		}
	}
	return 0, 0, false
}

// describeDelta words a change without judging it: which direction is good depends on the goal.
func describeDelta(delta float64, unit string, age int) (arrow, text string) {
	size := fmt.Sprintf("%.1f", math.Abs(delta))
	if size == "0.0" {
		return "→", fmt.Sprintf("No change in %d days", age)
	}
	arrow = "↑"
	if delta < 0 {
		arrow = "↓"
	}
	if unit != "" {
		size += " " + unit
	}
	return arrow, fmt.Sprintf("%s in %d days", size, age)
}

// fieldSeries returns a field's values oldest first, limited to the last rangeDays days of that
// field (0 means all of them). days are newest first.
func fieldSeries(days []estimate.DayEstimate, key string, rangeDays int) []chartPoint {
	var pts []chartPoint
	for i := len(days) - 1; i >= 0; i-- {
		if v, ok := days[i].Values[key]; ok {
			pts = append(pts, chartPoint{days[i].Date, v})
		}
	}
	if rangeDays <= 0 || len(pts) == 0 {
		return pts
	}
	cutoff := pts[len(pts)-1].date.AddDate(0, 0, -(rangeDays - 1))
	// The newest point is never before the cutoff, so there is always one.
	return pts[slices.IndexFunc(pts, func(p chartPoint) bool { return !p.date.Before(cutoff) }):]
}

func buildChart(pts []chartPoint, f estimate.Field, rangeDesc string) *chartView {
	first, last := pts[0].date, pts[len(pts)-1].date
	span := max(1, daysBetween(first, last))

	lo, hi := pts[0].value.V, pts[0].value.V
	for _, p := range pts {
		lo, hi = math.Min(lo, p.value.V), math.Max(hi, p.value.V)
	}
	ticks, decimals := niceTicks(lo, hi, chartYTickCount)
	yMin, yMax := ticks[0], ticks[len(ticks)-1]

	xOf := func(d time.Time) float64 {
		return round1(chartLeft + float64(daysBetween(first, d))/float64(span)*(chartRight-chartLeft))
	}
	yOf := func(v float64) float64 {
		return round1(chartBottom - (v-yMin)/(yMax-yMin)*(chartBottom-chartTop))
	}

	c := &chartView{
		Summary:  chartSummary(pts, f, rangeDesc, lo, hi),
		Width:    chartWidth,
		Height:   chartHeight,
		PlotLeft: chartLeft, PlotRight: chartRight,
		PlotTop: chartTop, PlotBottom: chartBottom, PlotHeight: chartBottom - chartTop,
		TipWidth: tipWidth, TipHeight: tipHeight,
		YLabelX: chartLeft - 8, XLabelY: chartBottom + 20,
	}
	for _, t := range ticks {
		y := yOf(t)
		c.YTicks = append(c.YTicks, yTick{Y: y, LabelY: y + 4, Label: fmt.Sprintf("%.*f", decimals, t)})
	}
	c.XTicks = buildXTicks(first, span, xOf)
	c.Solid, c.Dashed = linePaths(pts, xOf, yOf)
	c.HasDashed = c.Dashed != ""
	for _, p := range pts {
		if p.value.Origin == estimate.OriginDexa {
			c.Markers = append(c.Markers, chartMark{xOf(p.date), yOf(p.value.V)})
		}
	}
	c.HasMarkers = len(c.Markers) > 0
	c.Hits = buildHits(pts, f, xOf, yOf)
	return c
}

func chartSummary(pts []chartPoint, f estimate.Field, rangeDesc string, lo, hi float64) string {
	withUnit := func(v float64) string { return strings.TrimSpace(fmt.Sprintf("%.2f %s", v, f.Unit)) }
	a, b := pts[0], pts[len(pts)-1]
	return fmt.Sprintf("%s over %s: from %s on %s to %s on %s, lowest %s, highest %s.",
		f.Label, rangeDesc, withUnit(a.value.V), a.date.Format(dateLayout),
		withUnit(b.value.V), b.date.Format(dateLayout), withUnit(lo), withUnit(hi))
}

func buildXTicks(first time.Time, span int, xOf func(time.Time) float64) []xTick {
	layout := "2 Jan"
	if span > 365 {
		layout = "Jan 2006"
	}
	n := min(span, chartXTickCount)
	ticks := make([]xTick, 0, n+1)
	for i := 0; i <= n; i++ {
		d := first.AddDate(0, 0, int(math.Round(float64(span)*float64(i)/float64(n))))
		anchor := "middle"
		switch i {
		case 0:
			anchor = "start"
		case n:
			anchor = "end"
		}
		ticks = append(ticks, xTick{X: xOf(d), Label: d.Format(layout), Anchor: anchor})
	}
	return ticks
}

// linePaths draws the series as two paths: solid between measured days and dashed where either
// end of a segment is interpolated or held. Days without a value leave a gap.
func linePaths(pts []chartPoint, xOf func(time.Time) float64, yOf func(float64) float64) (solid, dashed string) {
	var sb, db strings.Builder
	sEnd, dEnd := -1, -1
	for i := 1; i < len(pts); i++ {
		if daysBetween(pts[i-1].date, pts[i].date) != 1 {
			continue
		}
		b, end := &sb, &sEnd
		if isFilled(pts[i-1].value) || isFilled(pts[i].value) {
			b, end = &db, &dEnd
		}
		if *end != i-1 {
			fmt.Fprintf(b, "M%.1f %.1f", xOf(pts[i-1].date), yOf(pts[i-1].value.V))
		}
		fmt.Fprintf(b, "L%.1f %.1f", xOf(pts[i].date), yOf(pts[i].value.V))
		*end = i
	}
	return sb.String(), db.String()
}

func isFilled(v estimate.Value) bool {
	return v.Origin == estimate.OriginInterpolated || v.Origin == estimate.OriginHeld
}

func buildHits(pts []chartPoint, f estimate.Field, xOf func(time.Time) float64, yOf func(float64) float64) []hitColumn {
	stride := max(1, (len(pts)+maxHitColumns-1)/maxHitColumns)
	var idx []int
	for i := len(pts) - 1; i >= 0; i -= stride {
		idx = append([]int{i}, idx...)
	}
	mid := (chartLeft + chartRight) / 2
	hits := make([]hitColumn, 0, len(idx))
	for k, i := range idx {
		p := pts[i]
		x, y := xOf(p.date), yOf(p.value.V)
		left, right := chartLeft, chartRight
		if k > 0 {
			left = round1((xOf(pts[idx[k-1]].date) + x) / 2)
		}
		if k < len(idx)-1 {
			right = round1((xOf(pts[idx[k+1]].date) + x) / 2)
		}
		tipX := x + tipGap
		if x > mid {
			tipX = x - tipGap - tipWidth
		}
		tipY := round1(math.Max(chartTop, math.Min(y-tipHeight/2, chartBottom-tipHeight)))
		hits = append(hits, hitColumn{
			Left: left, Width: round1(right - left),
			X: x, Y: y,
			TipX: tipX, TipY: tipY, TextX: tipX + 12,
			DateY: tipY + 20, ValueY: tipY + 39, OriginY: tipY + 56,
			Date:   p.date.Format(dateLayout),
			Value:  strings.TrimSpace(fmt.Sprintf("%.2f %s", p.value.V, f.Unit)),
			Origin: pointOrigin(p.value),
		})
	}
	return hits
}

func pointOrigin(v estimate.Value) string {
	var s string
	switch v.Origin {
	case estimate.OriginDexa:
		s = "DEXA scan"
	case estimate.OriginHeld:
		s = "Held"
	case estimate.OriginInterpolated:
		s = "Interpolated"
	default:
		s = "Measured"
	}
	if v.Corrected {
		s += ", corrected"
	}
	return s
}

// niceTicks returns round tick values covering lo..hi and the decimals needed to print them.
func niceTicks(lo, hi float64, count int) ([]float64, int) {
	// A range that is only a few float ULPs wide (identical readings averaged in different
	// window sizes) would give a step near 1e-16 and a tick count that never finishes.
	if hi-lo < 1e-9*math.Max(1, math.Max(math.Abs(lo), math.Abs(hi))) {
		mid := (lo + hi) / 2
		lo, hi = mid-1, mid+1
	}
	pad := (hi - lo) * 0.05
	if lo >= 0 {
		lo = math.Max(0, lo-pad)
	} else {
		lo -= pad
	}
	hi += pad

	step := niceStep((hi - lo) / float64(count-1))
	start := math.Floor(lo/step + 1e-9)
	end := math.Ceil(hi/step - 1e-9)
	decimals := max(0, -int(math.Floor(math.Log10(step)+1e-9)))
	scale := math.Pow(10, float64(decimals))
	count = min(int(end-start)+1, 50)
	ticks := make([]float64, 0, count)
	for i := 0; i < count; i++ {
		ticks = append(ticks, math.Round((start+float64(i))*step*scale)/scale)
	}
	return ticks, decimals
}

func niceStep(raw float64) float64 {
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5} {
		if raw <= m*mag {
			return m * mag
		}
	}
	return 10 * mag
}

func daysBetween(a, b time.Time) int {
	return int(math.Round(b.Sub(a).Hours() / 24))
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
