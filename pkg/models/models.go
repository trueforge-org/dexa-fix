// Package models holds the domain types shared by the store, estimator and web layers.
package models

import "time"

// SourceManual is the source of readings entered by hand.
const SourceManual = "manual"

// User is a person whose measurements are tracked. There is no authentication.
type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
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
