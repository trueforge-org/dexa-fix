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
	JobPriority       int       `bun:"job_priority"`
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
		JobPriority:       r.JobPriority,
	}
}

// bodyStateRow counts the changes to one user's scans and readings (see bumpBodyVersion).
type bodyStateRow struct {
	bun.BaseModel `bun:"table:body_user_state"`

	UserID      int64 `bun:"user_id,pk"`
	DataVersion int64 `bun:"data_version"`
}

// scaleRow is one scale reading. The columns that identify and order it are plain; the measured
// values and the notes are in Data (see scaleValues), encrypted when the store has a key.
// bodyMetaRow is a row of body_meta: the key check of the body domain.
type bodyMetaRow struct {
	bun.BaseModel `bun:"table:body_meta"`

	Name      string    `bun:"name,pk"`
	Value     string    `bun:"value"`
	UpdatedAt time.Time `bun:"updated_at"`
}

type scaleRow struct {
	bun.BaseModel `bun:"table:scale_readings"`

	ID          int64     `bun:"id,pk,autoincrement"`
	UserID      int64     `bun:"user_id"`
	MeasuredAt  time.Time `bun:"measured_at"`
	Source      string    `bun:"source"`
	Method      string    `bun:"method"`
	AthleteMode bool      `bun:"athlete_mode"`
	ExternalID  *string   `bun:"external_id"`
	CreatedAt   time.Time `bun:"created_at"`
	RowUID      string    `bun:"row_uid"`
	Data        string    `bun:"data"`

	Vals scaleValues `bun:"-"` // what Data holds, while a row is being written
}

// scaleValues is the document in scaleRow.Data.
type scaleValues struct {
	WeightKg         *float64 `json:"weight_kg,omitempty"`
	FatRatioPct      *float64 `json:"fat_ratio_pct,omitempty"`
	FatMassKg        *float64 `json:"fat_mass_kg,omitempty"`
	FatFreeMassKg    *float64 `json:"fat_free_mass_kg,omitempty"`
	MuscleMassKg     *float64 `json:"muscle_mass_kg,omitempty"`
	HydrationKg      *float64 `json:"hydration_kg,omitempty"`
	BoneMassKg       *float64 `json:"bone_mass_kg,omitempty"`
	VisceralFatIndex *float64 `json:"visceral_fat_index,omitempty"`
	Notes            string   `json:"notes,omitempty"`
}

// dexaRow is one DEXA scan, laid out like scaleRow.
type dexaRow struct {
	bun.BaseModel `bun:"table:dexa_scans"`

	ID         int64     `bun:"id,pk,autoincrement"`
	UserID     int64     `bun:"user_id"`
	MeasuredAt time.Time `bun:"measured_at"`
	CreatedAt  time.Time `bun:"created_at"`
	RowUID     string    `bun:"row_uid"`
	Data       string    `bun:"data"`

	Vals dexaValues `bun:"-"`
}

// dexaValues is the document in dexaRow.Data.
type dexaValues struct {
	WeightKg          *float64 `json:"weight_kg,omitempty"`
	FatRatioPct       *float64 `json:"fat_ratio_pct,omitempty"`
	FatMassKg         *float64 `json:"fat_mass_kg,omitempty"`
	FatFreeMassKg     *float64 `json:"fat_free_mass_kg,omitempty"`
	LeanMassKg        *float64 `json:"lean_mass_kg,omitempty"`
	BoneMassKg        *float64 `json:"bone_mass_kg,omitempty"`
	VisceralFatMassKg *float64 `json:"visceral_fat_mass_kg,omitempty"`
	BMDGCm2           *float64 `json:"bmd_g_cm2,omitempty"`
	TScore            *float64 `json:"t_score,omitempty"`
	ZScore            *float64 `json:"z_score,omitempty"`
	AndroidFatPct     *float64 `json:"android_fat_pct,omitempty"`
	GynoidFatPct      *float64 `json:"gynoid_fat_pct,omitempty"`
	Notes             string   `json:"notes,omitempty"`
}

type integrationRow struct {
	bun.BaseModel `bun:"table:user_integrations"`

	UserID    int64     `bun:"user_id,pk"`
	Provider  string    `bun:"provider,pk"`
	Data      string    `bun:"data"`
	UpdatedAt time.Time `bun:"updated_at"`
}
