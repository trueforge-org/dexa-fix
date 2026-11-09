package sqlstore

import (
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// The row types below map the schema for bun. They are kept separate from pkg/models so the domain
// types stay free of persistence concerns.

type userRow struct {
	bun.BaseModel `bun:"table:users"`

	ID                int64     `bun:"id,pk,autoincrement"`
	Name              string    `bun:"name"`
	Role              string    `bun:"role"`
	Email             string    `bun:"email"`
	EmailLookup       *string   `bun:"email_lookup"`
	HeightCm          *float64  `bun:"height_cm"`
	Timezone          string    `bun:"timezone"`
	CreatedAt         time.Time `bun:"created_at"`
	AthleteDefault    bool      `bun:"athlete_default"`
	MethodRanking     string    `bun:"method_ranking"`
	HiddenColumns     string    `bun:"hidden_columns"`
	StrengthLevelAuto bool      `bun:"strengthlevel_auto"`
	StrengthLevelBulk bool      `bun:"strengthlevel_bulk"`
	DataVersion       int64     `bun:"data_version"`
	PasskeyHandle     *string   `bun:"passkey_handle"`
}

func (r *userRow) toModel() models.User {
	var keys, hidden []string
	if r.MethodRanking != "" {
		keys = strings.Split(r.MethodRanking, ",")
	}
	if r.HiddenColumns != "" {
		hidden = strings.Split(r.HiddenColumns, ",")
	}
	tz := r.Timezone
	if tz == "" {
		tz = "UTC"
	}
	return models.User{
		ID:                r.ID,
		Name:              r.Name,
		Role:              r.Role,
		Email:             r.Email,
		HeightCm:          r.HeightCm,
		Timezone:          tz,
		CreatedAt:         r.CreatedAt,
		AthleteDefault:    r.AthleteDefault,
		MethodRanking:     models.NormalizeRanking(keys),
		HiddenColumns:     hidden,
		StrengthLevelAuto: r.StrengthLevelAuto,
		StrengthLevelBulk: r.StrengthLevelBulk,
		DataVersion:       r.DataVersion,
	}
}

type scaleRow struct {
	bun.BaseModel `bun:"table:scale_readings"`

	ID               int64     `bun:"id,pk,autoincrement"`
	UserID           int64     `bun:"user_id"`
	MeasuredAt       time.Time `bun:"measured_at"`
	WeightKg         *float64  `bun:"weight_kg"`
	FatRatioPct      *float64  `bun:"fat_ratio_pct"`
	FatMassKg        *float64  `bun:"fat_mass_kg"`
	FatFreeMassKg    *float64  `bun:"fat_free_mass_kg"`
	MuscleMassKg     *float64  `bun:"muscle_mass_kg"`
	HydrationKg      *float64  `bun:"hydration_kg"`
	BoneMassKg       *float64  `bun:"bone_mass_kg"`
	VisceralFatIndex *float64  `bun:"visceral_fat_index"`
	Source           string    `bun:"source"`
	Method           string    `bun:"method"`
	AthleteMode      bool      `bun:"athlete_mode"`
	ExternalID       *string   `bun:"external_id"`
	Notes            string    `bun:"notes"`
	CreatedAt        time.Time `bun:"created_at"`
}

// scaleValueCols are the columns refreshed when an import re-delivers an existing reading.
// method and athlete_mode are deliberately absent: they are user choices that must survive re-imports.
var scaleValueCols = []string{
	"measured_at", "notes",
	"weight_kg", "fat_ratio_pct", "fat_mass_kg", "fat_free_mass_kg",
	"muscle_mass_kg", "hydration_kg", "bone_mass_kg", "visceral_fat_index",
}

// scaleValueColsKeepNotes is scaleValueCols without notes, for re-delivered readings that carry none.
var scaleValueColsKeepNotes = func() []string {
	var out []string
	for _, c := range scaleValueCols {
		if c != "notes" {
			out = append(out, c)
		}
	}
	return out
}()

func (r *scaleRow) toModel() models.ScaleReading {
	return models.ScaleReading{
		ID: r.ID, UserID: r.UserID, MeasuredAt: r.MeasuredAt.UTC(),
		WeightKg: r.WeightKg, FatRatioPct: r.FatRatioPct, FatMassKg: r.FatMassKg, FatFreeMassKg: r.FatFreeMassKg,
		MuscleMassKg: r.MuscleMassKg, HydrationKg: r.HydrationKg, BoneMassKg: r.BoneMassKg, VisceralFatIndex: r.VisceralFatIndex,
		Source: r.Source, Method: r.Method, AthleteMode: r.AthleteMode, ExternalID: r.ExternalID,
		Notes: r.Notes, CreatedAt: r.CreatedAt,
	}
}

type dexaRow struct {
	bun.BaseModel `bun:"table:dexa_scans"`

	ID                int64     `bun:"id,pk,autoincrement"`
	UserID            int64     `bun:"user_id"`
	MeasuredAt        time.Time `bun:"measured_at"`
	WeightKg          *float64  `bun:"weight_kg"`
	FatRatioPct       *float64  `bun:"fat_ratio_pct"`
	FatMassKg         *float64  `bun:"fat_mass_kg"`
	FatFreeMassKg     *float64  `bun:"fat_free_mass_kg"`
	LeanMassKg        *float64  `bun:"lean_mass_kg"`
	BoneMassKg        *float64  `bun:"bone_mass_kg"`
	VisceralFatMassKg *float64  `bun:"visceral_fat_mass_kg"`
	BMDGCm2           *float64  `bun:"bmd_g_cm2"`
	TScore            *float64  `bun:"t_score"`
	ZScore            *float64  `bun:"z_score"`
	AndroidFatPct     *float64  `bun:"android_fat_pct"`
	GynoidFatPct      *float64  `bun:"gynoid_fat_pct"`
	Notes             string    `bun:"notes"`
	CreatedAt         time.Time `bun:"created_at"`
}

func (r *dexaRow) toModel() models.DexaScan {
	return models.DexaScan{
		ID: r.ID, UserID: r.UserID, MeasuredAt: r.MeasuredAt.UTC(),
		WeightKg: r.WeightKg, FatRatioPct: r.FatRatioPct, FatMassKg: r.FatMassKg, FatFreeMassKg: r.FatFreeMassKg,
		LeanMassKg: r.LeanMassKg, BoneMassKg: r.BoneMassKg, VisceralFatMassKg: r.VisceralFatMassKg,
		BMDGCm2: r.BMDGCm2, TScore: r.TScore, ZScore: r.ZScore, AndroidFatPct: r.AndroidFatPct, GynoidFatPct: r.GynoidFatPct,
		Notes: r.Notes, CreatedAt: r.CreatedAt,
	}
}

type integrationRow struct {
	bun.BaseModel `bun:"table:user_integrations"`

	UserID    int64     `bun:"user_id,pk"`
	Provider  string    `bun:"provider,pk"`
	Data      string    `bun:"data"`
	UpdatedAt time.Time `bun:"updated_at"`
}

type strengthLevelJobRow struct {
	bun.BaseModel `bun:"table:strengthlevel_jobs"`

	UserID    int64     `bun:"user_id,pk"`
	State     string    `bun:"state"`
	UpdatedAt time.Time `bun:"updated_at"`
}
