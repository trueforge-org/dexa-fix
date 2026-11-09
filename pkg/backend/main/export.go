package mainbackend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// exportFormatVersion is bumped when the shape of an export changes incompatibly.
const exportFormatVersion = 1

type profileExport struct {
	FormatVersion int       `json:"format_version"`
	ExportedAt    time.Time `json:"exported_at"`
	Account       struct {
		ID        int64     `json:"id"`
		Name      string    `json:"name"`
		Email     string    `json:"email"`
		Role      string    `json:"role"`
		HeightCm  *float64  `json:"height_cm"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"account"`
	Settings struct {
		AthleteDefault bool     `json:"athlete_default"`
		MethodRanking  []string `json:"method_ranking"`
		HiddenColumns  []string `json:"hidden_columns"`
	} `json:"settings"`
	// Permissions are granted by an administrator.
	Permissions struct {
		StrengthLevelSync     bool `json:"strengthlevel_sync"`
		StrengthLevelBulkSync bool `json:"strengthlevel_bulk_sync"`
	} `json:"permissions"`
	// TwoFactor says whether two-step verification is on; the secret and codes are never exported.
	TwoFactor struct {
		Enabled bool `json:"enabled"`
	} `json:"two_factor"`
	// Passkeys lists the names and dates of the passkeys; the public keys are not exported.
	Passkeys      []exportPasskey     `json:"passkeys"`
	SignInMethods []exportIdentity    `json:"sign_in_methods"`
	AccessTokens  []exportAccessToken `json:"access_tokens"`
	// AuditLog lists the security events that were about this account or done by it.
	AuditLog     []models.AuditEvent `json:"audit_log"`
	Integrations struct {
		Withings struct {
			Connected bool `json:"connected"`
		} `json:"withings"`
		StrengthLevel struct {
			Connected bool   `json:"connected"`
			Email     string `json:"email,omitempty"`
		} `json:"strengthlevel"`
	} `json:"integrations"`
}

type exportIdentity struct {
	Provider string `json:"provider"`
	// Subject is the account id at an external provider. For a password it is the user id.
	Subject  string    `json:"subject"`
	LinkedAt time.Time `json:"linked_at"`
}

// exportAccessToken describes a personal access token. The token itself is not stored, only its
// hash, so it cannot be exported.
type exportPasskey struct {
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type exportAccessToken struct {
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

type dataExport struct {
	FormatVersion int                   `json:"format_version"`
	ExportedAt    time.Time             `json:"exported_at"`
	UserID        int64                 `json:"user_id"`
	DexaScans     []models.DexaScan     `json:"dexa_scans"`
	ScaleReadings []models.ScaleReading `json:"scale_readings"`
}

func (s *Backend) buildProfileExport(r *http.Request, u models.User) (profileExport, error) {
	ctx := r.Context()
	var e profileExport
	e.FormatVersion, e.ExportedAt = exportFormatVersion, time.Now().UTC()
	e.Account.ID, e.Account.Name, e.Account.Email, e.Account.Role = u.ID, u.Name, u.Email, u.Role
	e.Account.HeightCm, e.Account.CreatedAt = u.HeightCm, u.CreatedAt
	e.Settings.AthleteDefault, e.Settings.MethodRanking = u.AthleteDefault, orEmpty(u.MethodRanking)
	e.Settings.HiddenColumns = orEmpty(u.HiddenColumns)
	e.Permissions.StrengthLevelSync, e.Permissions.StrengthLevelBulkSync = u.StrengthLevelAuto, u.StrengthLevelBulk

	ids, err := s.store.ListUserIdentities(ctx, u.ID)
	if err != nil {
		return e, err
	}
	e.SignInMethods = make([]exportIdentity, 0, len(ids))
	for _, id := range ids {
		e.SignInMethods = append(e.SignInMethods, exportIdentity{Provider: id.Provider, Subject: id.Subject, LinkedAt: id.CreatedAt})
	}
	if e.TwoFactor.Enabled, err = s.hasTwoFactor(ctx, u.ID); err != nil {
		return e, err
	}
	keys, err := s.store.ListPasskeys(ctx, u.ID)
	if err != nil {
		return e, err
	}
	e.Passkeys = make([]exportPasskey, 0, len(keys))
	for _, k := range keys {
		e.Passkeys = append(e.Passkeys, exportPasskey{Name: k.Name, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt})
	}
	tokens, err := s.store.ListAccessTokens(ctx, u.ID)
	if err != nil {
		return e, err
	}
	e.AccessTokens = make([]exportAccessToken, 0, len(tokens))
	for _, t := range tokens {
		e.AccessTokens = append(e.AccessTokens, exportAccessToken{Name: t.Name, Prefix: t.Prefix, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt, ExpiresAt: t.ExpiresAt})
	}
	if e.AuditLog, err = s.store.ListAuditEvents(ctx, store.AuditFilter{UserID: u.ID, Limit: auditExportMax}); err != nil {
		return e, err
	}
	if v := s.withingsView(r, u); v != nil {
		e.Integrations.Withings.Connected = v.Connected
	}
	if login, err := s.strengthLevelLogin(r, u); err == nil {
		e.Integrations.StrengthLevel.Connected, e.Integrations.StrengthLevel.Email = true, login.Email
	} else if !errors.Is(err, store.ErrNotFound) {
		return e, err
	}
	return e, nil
}

func (s *Backend) buildDataExport(ctx context.Context, u models.User) (dataExport, error) {
	e := dataExport{FormatVersion: exportFormatVersion, ExportedAt: time.Now().UTC(), UserID: u.ID}
	body, err := s.body.Export(ctx, u)
	if err != nil {
		return e, err
	}
	e.DexaScans, e.ScaleReadings = orEmpty(body.DexaScans), orEmpty(body.ScaleReadings)
	return e, nil
}

// sendExport writes v as a downloadable, indented JSON file. The name has no user input in it.
func sendExport(w http.ResponseWriter, kind string, userID int64, v any) {
	// The encoder marshals once and writes that buffer, so the file is not held twice (as
	// MarshalIndent plus an appended newline would). Nothing is written if marshalling fails, so
	// the error response below still goes out cleanly.
	name := "dexa-fix-" + kind + "-user" + strconv.FormatInt(userID, 10) + "-" + time.Now().UTC().Format("2006-01-02") + ".json"
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Error().Err(err).Str("export", kind).Msg("export failed")
		h.Del("Content-Disposition")
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *Backend) exportProfile(w http.ResponseWriter, r *http.Request, u models.User) {
	e, err := s.buildProfileExport(r, u)
	if err != nil {
		apiError(w, err)
		return
	}
	s.audit(r, u.ID, auditExportProfile, "")
	sendExport(w, "profile", u.ID, e)
}

func (s *Backend) exportData(w http.ResponseWriter, r *http.Request, u models.User) {
	e, err := s.buildDataExport(r.Context(), u)
	if err != nil {
		apiError(w, err)
		return
	}
	s.audit(r, u.ID, auditExportData, "")
	sendExport(w, "data", u.ID, e)
}

// exportAPIRoutes registers the public API exports; the backend serves them.
func (s *Backend) exportAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/users/{id}/export/profile", s.apiUser(s.exportProfile))
	mux.HandleFunc("GET /api/v1/users/{id}/export/data", s.apiUser(s.exportData))
}
