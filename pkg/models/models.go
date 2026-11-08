// Package models holds the domain types shared by the store, estimator and web layers.
package models

import "time"

// SourceManual is the source of readings entered by hand.
const SourceManual = "manual"

// Measurement methods: how a scale/body-composition reading was obtained.
const (
	MethodScale       = "scale"       // bioimpedance smart scale (Withings, manual entry from a scale)
	MethodHydrostatic = "hydrostatic" // underwater weighing
	MethodBodPod      = "bodpod"      // air displacement plethysmography
	MethodCaliper     = "caliper"     // skinfold calipers
	MethodTape        = "tape"        // tape measure / Navy formula
	MethodVisualAI    = "visual-ai"   // AI estimate from photos
)

// MethodInfo describes a measurement method for forms.
type MethodInfo struct {
	Key   string
	Label string
}

// Methods lists all methods in the default accuracy order (most accurate first).
var Methods = []MethodInfo{
	{MethodHydrostatic, "Hydrostatic weighing"},
	{MethodBodPod, "BodPod"},
	{MethodCaliper, "Skinfold caliper"},
	{MethodScale, "Smart scale (Withings, manual)"},
	{MethodTape, "Tape / Navy formula"},
	{MethodVisualAI, "Visual AI"},
}

// DefaultRanking returns the method keys in the default accuracy order.
func DefaultRanking() []string {
	out := make([]string, len(Methods))
	for i, m := range Methods {
		out[i] = m.Key
	}
	return out
}

// ValidMethod reports whether key is a known method.
func ValidMethod(key string) bool {
	for _, m := range Methods {
		if m.Key == key {
			return true
		}
	}
	return false
}

// NormalizeRanking drops unknown and duplicate keys and appends missing methods in default order,
// so the result always contains every method exactly once.
func NormalizeRanking(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range in {
		if ValidMethod(k) && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, m := range Methods {
		if !seen[m.Key] {
			out = append(out, m.Key)
		}
	}
	return out
}

// User is a person whose measurements are tracked. There is no authentication.
type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// AthleteDefault presets athlete mode for new readings and imports.
	AthleteDefault bool `json:"athlete_default"`
	// MethodRanking lists methods most accurate first. Always contains every method.
	MethodRanking []string `json:"method_ranking"`
}

// ScaleReading is a single smart-scale measurement. Fields are aligned with Withings measure types.
// Source and ExternalID make imports idempotent (e.g. Source "withings", ExternalID = grpid).
type ScaleReading struct {
	ID               int64     `json:"id"`
	UserID           int64     `json:"user_id"`
	MeasuredAt       time.Time `json:"measured_at"`
	WeightKg         *float64  `json:"weight_kg,omitempty"`
	FatRatioPct      *float64  `json:"fat_ratio_pct,omitempty"`
	FatMassKg        *float64  `json:"fat_mass_kg,omitempty"`
	FatFreeMassKg    *float64  `json:"fat_free_mass_kg,omitempty"`
	MuscleMassKg     *float64  `json:"muscle_mass_kg,omitempty"`
	HydrationKg      *float64  `json:"hydration_kg,omitempty"`
	BoneMassKg       *float64  `json:"bone_mass_kg,omitempty"`
	VisceralFatIndex *float64  `json:"visceral_fat_index,omitempty"`
	Source           string    `json:"source"`
	Method           string    `json:"method"`
	AthleteMode      bool      `json:"athlete_mode"`
	ExternalID       *string   `json:"external_id,omitempty"`
	Notes            string    `json:"notes,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// DexaScan is a single DEXA scan result.
type DexaScan struct {
	ID                int64     `json:"id"`
	UserID            int64     `json:"user_id"`
	MeasuredAt        time.Time `json:"measured_at"`
	WeightKg          *float64  `json:"weight_kg,omitempty"`
	FatRatioPct       *float64  `json:"fat_ratio_pct,omitempty"`
	FatMassKg         *float64  `json:"fat_mass_kg,omitempty"`
	FatFreeMassKg     *float64  `json:"fat_free_mass_kg,omitempty"`
	LeanMassKg        *float64  `json:"lean_mass_kg,omitempty"`
	BoneMassKg        *float64  `json:"bone_mass_kg,omitempty"`
	VisceralFatMassKg *float64  `json:"visceral_fat_mass_kg,omitempty"`
	BMDGCm2           *float64  `json:"bmd_g_cm2,omitempty"`
	TScore            *float64  `json:"t_score,omitempty"`
	ZScore            *float64  `json:"z_score,omitempty"`
	AndroidFatPct     *float64  `json:"android_fat_pct,omitempty"`
	GynoidFatPct      *float64  `json:"gynoid_fat_pct,omitempty"`
	Notes             string    `json:"notes,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}
