package frontend

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// field describes one numeric form input / table column.
type field struct {
	Key   string
	Label string
	Unit  string
}

// row is one table row of a measurement overview.
type row struct {
	ID      int64
	When    string
	Cells   []string
	Source  string
	Method  string // label of the measurement method (scale readings)
	Athlete bool
	Notes   string
}

// methodLabel returns the display label for a method key.
func methodLabel(key string) string {
	for _, m := range models.Methods {
		if m.Key == key {
			return m.Label
		}
	}
	return key
}

var scaleFields = []field{
	{"weight_kg", "Weight", "kg"},
	{"fat_ratio_pct", "Body fat", "%"},
	{"fat_mass_kg", "Fat mass", "kg"},
	{"fat_free_mass_kg", "Fat-free mass", "kg"},
	{"muscle_mass_kg", "Muscle mass", "kg"},
	{"hydration_kg", "Hydration", "kg"},
	{"bone_mass_kg", "Bone mass", "kg"},
	{"visceral_fat_index", "Visceral fat", "index"},
}

var dexaFields = []field{
	{"weight_kg", "Weight", "kg"},
	{"fat_ratio_pct", "Body fat", "%"},
	{"fat_mass_kg", "Fat mass", "kg"},
	{"fat_free_mass_kg", "Fat-free mass", "kg"},
	{"lean_mass_kg", "Lean mass", "kg"},
	{"bone_mass_kg", "Bone mass (BMC)", "kg"},
	{"visceral_fat_mass_kg", "Visceral fat (VAT)", "kg"},
	{"bmd_g_cm2", "BMD", "g/cm²"},
	{"t_score", "T-score", ""},
	{"z_score", "Z-score", ""},
	{"android_fat_pct", "Android fat", "%"},
	{"gynoid_fat_pct", "Gynoid fat", "%"},
}

func scaleTargets(r *models.ScaleReading) []**float64 {
	return []**float64{&r.WeightKg, &r.FatRatioPct, &r.FatMassKg, &r.FatFreeMassKg, &r.MuscleMassKg, &r.HydrationKg, &r.BoneMassKg, &r.VisceralFatIndex}
}

func dexaTargets(d *models.DexaScan) []**float64 {
	return []**float64{&d.WeightKg, &d.FatRatioPct, &d.FatMassKg, &d.FatFreeMassKg, &d.LeanMassKg, &d.BoneMassKg, &d.VisceralFatMassKg, &d.BMDGCm2, &d.TScore, &d.ZScore, &d.AndroidFatPct, &d.GynoidFatPct}
}

func cells(targets []**float64) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = "–"
		if *t != nil {
			out[i] = fmt.Sprintf("%.2f", **t)
		}
	}
	return out
}

var errNoValues = errors.New("Enter at least one measurement.")

// parseMeasures fills targets from the form. Empty inputs stay nil; a decimal comma is accepted.
func parseMeasures(r *http.Request, fields []field, targets []**float64) error {
	any := false
	for i, f := range fields {
		raw := strings.TrimSpace(strings.ReplaceAll(r.PostFormValue(f.Key), ",", "."))
		if raw == "" {
			*targets[i] = nil
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < -1000 || v > 10000 {
			return fmt.Errorf("%s is not a valid number.", f.Label)
		}
		*targets[i] = &v
		any = true
	}
	if !any {
		return errNoValues
	}
	return nil
}

// parseMethod reads and validates the measurement method; empty means a smart scale.
func parseMethod(r *http.Request) (string, error) {
	m := strings.TrimSpace(r.PostFormValue("method"))
	if m == "" {
		return models.MethodScale, nil
	}
	if !models.ValidMethod(m) {
		return "", errors.New("Choose a valid measurement method.")
	}
	return m, nil
}

func parseNotes(r *http.Request) (string, error) {
	n := strings.TrimSpace(r.PostFormValue("notes"))
	if len(n) > 500 {
		return "", errors.New("Notes are limited to 500 characters.")
	}
	return n, nil
}

const dateLayout = "2006-01-02"
const dateTimeLayout = "2006-01-02T15:04"

// parseWhen reads a date or date-time typed in loc. The result is the instant it denotes (in UTC);
// a plain date is a calendar date and is passed with time.UTC, so it is stored as that date.
func parseWhen(raw, layout string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation(layout, strings.TrimSpace(raw), loc)
	if err != nil {
		return time.Time{}, errors.New("Enter a valid date.")
	}
	return t.UTC(), nil
}
