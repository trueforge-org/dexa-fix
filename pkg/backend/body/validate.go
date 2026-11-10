package bodybackend

import (
	"errors"
	"fmt"
	"math"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// The body service checks every value again: it does not trust the main backend to have done it.
// The messages are the ones the main backend gives for the same mistakes.

// valueRange is one allowed range for a numeric API field.
type valueRange struct {
	name   string
	v      *float64
	lo, hi float64
}

// checkRanges rejects values outside what a body measurement can be. Absent (nil) values pass.
func checkRanges(notes string, rs ...valueRange) error {
	for _, r := range rs {
		if r.v == nil {
			continue
		}
		if math.IsNaN(*r.v) || math.IsInf(*r.v, 0) || *r.v < r.lo || *r.v > r.hi {
			return fmt.Errorf("%s must be between %g and %g", r.name, r.lo, r.hi)
		}
	}
	if len(notes) > 500 {
		return errors.New("notes are limited to 500 characters")
	}
	return nil
}

func validScaleReading(r *models.ScaleReading) error {
	return checkRanges(r.Notes,
		valueRange{"weight_kg", r.WeightKg, 0.1, 1000},
		valueRange{"fat_ratio_pct", r.FatRatioPct, 0, 100},
		valueRange{"fat_mass_kg", r.FatMassKg, 0, 1000},
		valueRange{"fat_free_mass_kg", r.FatFreeMassKg, 0, 1000},
		valueRange{"muscle_mass_kg", r.MuscleMassKg, 0, 1000},
		valueRange{"hydration_kg", r.HydrationKg, 0, 1000},
		valueRange{"bone_mass_kg", r.BoneMassKg, 0, 100},
		valueRange{"visceral_fat_index", r.VisceralFatIndex, 0, 1000},
	)
}

func validDexaScan(s *models.DexaScan) error {
	return checkRanges(s.Notes,
		valueRange{"weight_kg", s.WeightKg, 0.1, 1000},
		valueRange{"fat_ratio_pct", s.FatRatioPct, 0, 100},
		valueRange{"fat_mass_kg", s.FatMassKg, 0, 1000},
		valueRange{"fat_free_mass_kg", s.FatFreeMassKg, 0, 1000},
		valueRange{"lean_mass_kg", s.LeanMassKg, 0, 1000},
		valueRange{"bone_mass_kg", s.BoneMassKg, 0, 100},
		valueRange{"visceral_fat_mass_kg", s.VisceralFatMassKg, 0, 100},
		valueRange{"bmd_g_cm2", s.BMDGCm2, 0, 10},
		valueRange{"t_score", s.TScore, -20, 20},
		valueRange{"z_score", s.ZScore, -20, 20},
		valueRange{"android_fat_pct", s.AndroidFatPct, 0, 100},
		valueRange{"gynoid_fat_pct", s.GynoidFatPct, 0, 100},
	)
}
