// Package models holds the domain types shared by the store, estimator and web layers.
package models

import (
	"strings"
	"time"
)

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

// User roles.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// ProviderLocal is the identity provider of username and password sign-in.
const ProviderLocal = "local"

// Kinds of external sign-in provider.
const (
	ProviderKindOIDC   = "oidc"   // OpenID Connect with discovery, e.g. Google
	ProviderKindApple  = "apple"  // Sign in with Apple: OIDC with a generated client secret
	ProviderKindOAuth2 = "oauth2" // plain OAuth 2.0 with a userinfo endpoint
)

// Identity is one way a user can sign in, independent of the user record itself.
type Identity struct {
	UserID   int64
	Provider string
	// Subject identifies the account at the provider. For ProviderLocal it is the user id.
	Subject string
	// Secret holds the password hash for ProviderLocal and is empty otherwise.
	Secret    string
	CreatedAt time.Time
}

// AccessToken is a personal access token. Only a hash of the token is stored.
type AccessToken struct {
	ID         int64
	UserID     int64
	Name       string
	Prefix     string // the first characters of the token, to recognise it in lists
	Hash       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
}

// AuthProvider is an external sign-in provider added by an administrator.
type AuthProvider struct {
	Slug         string
	Label        string
	Kind         string
	Issuer       string // OIDC issuer URL
	AuthURL      string // plain OAuth 2.0 endpoints
	TokenURL     string
	UserinfoURL  string
	ClientID     string
	ClientSecret string // for Apple: the PEM private key the client secret is signed with
	AppleTeamID  string
	AppleKeyID   string
	Scopes       string // space separated
	SubjectClaim string // plain OAuth 2.0: the userinfo field that identifies the account
	NameClaim    string // preferred claim for naming new users
	AllowSignup  bool   // create an account on the first sign-in
	CreatedAt    time.Time
}

// User is a person whose measurements are tracked.
type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	// Email signs the user in and identifies them, unique per user. It is empty for accounts that
	// only sign in through a provider that did not vouch for an address. External providers never
	// match accounts by it.
	Email    string   `json:"email"`
	HeightCm *float64 `json:"height_cm,omitempty"`
	// Timezone is an IANA time zone name such as "Europe/Amsterdam". It decides which calendar
	// day a reading belongs to and how manual date-times are read and shown. Empty means UTC.
	Timezone string `json:"timezone"`
	// AthleteDefault presets athlete mode for new readings and imports.
	AthleteDefault bool `json:"athlete_default"`
	// MethodRanking lists methods most accurate first. Always contains every method.
	MethodRanking []string `json:"method_ranking"`
	// HiddenColumns lists the daily table columns the user has hidden.
	HiddenColumns []string `json:"-"`
	// StrengthLevelAuto is set by an administrator. It lets the user store their Strength Level
	// login and sync body readings to it automatically.
	StrengthLevelAuto bool `json:"-"`
	// StrengthLevelBulk additionally allows the multi-day sync on the settings page. It needs
	// StrengthLevelAuto.
	StrengthLevelBulk bool `json:"-"`
	// DataVersion changes whenever the scans, readings or settings that estimates are computed
	// from change, in any way and from any process. Cached results are valid for one version.
	DataVersion int64 `json:"-"`
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

// AuditEvent records something security-relevant that happened: a sign-in, a change to an account,
// an export or an administrator's action. Detail never holds credentials or other personal data.
type AuditEvent struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	ActorID  *int64    `json:"actor_id"`  // who did it; nil for anonymous requests and deleted users
	TargetID *int64    `json:"target_id"` // whose account it concerns; nil when none or deleted
	Action   string    `json:"action"`
	Detail   string    `json:"detail,omitempty"`
	IP       string    `json:"ip,omitempty"` // cleared when the user is deleted, dropped after the retention period
}

// NormalizeEmail returns the canonical form of an e-mail address, used to compare addresses.
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// IsAdmin reports whether the user has the administrator role.
func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

// Location returns the user's time zone, or UTC when none is set or it cannot be loaded.
func (u User) Location() *time.Location {
	if u.Timezone == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(u.Timezone); err == nil {
		return loc
	}
	return time.UTC
}
