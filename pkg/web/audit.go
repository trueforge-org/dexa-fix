package web

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// Audit actions. Detail strings never hold credentials or free-form personal data (no passwords,
// tokens, e-mail addresses or login names), because the log is kept for a long time and read by
// administrators.
const (
	auditLogin            = "login"
	auditLoginFailed      = "login_failed"
	auditLoginThrottled   = "login_throttled"
	auditLogout           = "logout"
	auditSignup           = "signup"
	auditProfile          = "profile_updated"
	auditPasswordChanged  = "password_changed"
	auditPasswordWrong    = "password_confirm_failed"
	auditAPIAuthFailed    = "api_auth_failed"
	auditSLLoginSaved     = "strengthlevel_login_saved"
	auditSLLoginRemoved   = "strengthlevel_login_removed"
	auditSLSync           = "strengthlevel_sync_started"
	auditTokenCreated     = "token_created"
	auditTokenRevoked     = "token_revoked"
	auditIdentityLinked   = "sign_in_linked"
	auditIdentityUnlinked = "sign_in_unlinked"
	auditExportProfile    = "export_profile"
	auditExportData       = "export_data"
	auditScanDeleted      = "dexa_scan_deleted"
	auditReadingDeleted   = "scale_reading_deleted"
	auditImport           = "import"
	auditConnect          = "integration_connected"
	auditDisconnect       = "integration_disconnected"

	auditUserAdded        = "user_added"
	auditUserDeleted      = "user_deleted"
	auditRoleChanged      = "role_changed"
	auditPasswordSet      = "password_set_by_admin"
	auditStrengthLevel    = "strengthlevel_permission_changed"
	auditStrengthLevelBlk = "strengthlevel_bulk_permission_changed"
	auditSettingChanged   = "server_setting_changed"
	auditProviderSaved    = "provider_saved"
	auditProviderRemoved  = "provider_removed"
)

var auditLabels = map[string]string{
	auditLogin: "Signed in", auditLoginFailed: "Failed sign-in", auditLoginThrottled: "Sign-in blocked (too many failures)",
	auditLogout: "Signed out", auditSignup: "Account created",
	auditPasswordWrong: "Wrong password on a sensitive change", auditAPIAuthFailed: "Failed API sign-in",
	auditSLLoginSaved: "Strength Level login saved", auditSLLoginRemoved: "Strength Level login removed",
	auditSLSync:  "Strength Level sync started",
	auditProfile: "Profile changed", auditPasswordChanged: "Password changed", auditTokenCreated: "Access token created",
	auditTokenRevoked: "Access token revoked", auditIdentityLinked: "Sign-in method linked", auditIdentityUnlinked: "Sign-in method unlinked",
	auditExportProfile: "Settings and personal details exported", auditExportData: "Measurements exported",
	auditScanDeleted: "DEXA scan deleted", auditReadingDeleted: "Scale reading deleted", auditImport: "Import ran",
	auditConnect: "Integration connected", auditDisconnect: "Integration disconnected",
	auditUserAdded: "User added", auditUserDeleted: "User deleted", auditRoleChanged: "Role changed",
	auditPasswordSet: "Password set by an administrator", auditStrengthLevel: "Strength Level permission changed",
	auditStrengthLevelBlk: "Strength Level multi-day permission changed", auditSettingChanged: "Server setting changed",
	auditProviderSaved: "Sign-in provider saved", auditProviderRemoved: "Sign-in provider removed",
}

const (
	auditPageSize     = 100 // events on the admin page
	auditActivitySize = 20  // events on the settings page
	auditExportMax    = 100000
	auditPurgeEvery   = 6 * time.Hour
)

// audit records an event for the signed-in user (the actor) about the account target (0 for none).
// Recording is best effort: a failure is logged and never fails the request that caused it.
func (s *Server) audit(r *http.Request, target int64, action, detail string) {
	var actor int64
	if u := currentUser(r); u != nil {
		actor = u.ID
	}
	s.auditAs(r, actor, target, action, detail)
}

// auditAs is audit for requests that have no signed-in user yet, such as a sign-in.
func (s *Server) auditAs(r *http.Request, actor, target int64, action, detail string) {
	if noisyAudit[action] {
		ok, skipped := s.auditNoise.allow(action+"|"+clientIP(r), time.Now())
		if !ok {
			return
		}
		if skipped > 0 {
			detail = "plus " + strconv.Itoa(skipped) + " more in the last minutes"
		}
	}
	e := models.AuditEvent{Action: action, Detail: detail, IP: clientIP(r)}
	if actor != 0 {
		e.ActorID = &actor
	}
	if target != 0 {
		e.TargetID = &target
	}
	// A client that disconnects must not lose the record of what it did.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := s.store.AddAuditEvent(ctx, &e); err != nil {
		log.Error().Err(err).Str("action", action).Msg("audit log write failed")
		return
	}
	log.Info().Str("audit", action).Int64("actor", actor).Int64("target", target).Str("detail", detail).Msg("audit")
}

// auditLine is one event as shown on a page.
type auditLine struct {
	When, Action, Detail, IP string
	Actor, Target            string
}

// auditLines turns events into display lines. name resolves a user id; events that no longer
// refer to a user (deleted) show "—".
func auditLines(events []models.AuditEvent, name func(int64) string) []auditLine {
	out := make([]auditLine, 0, len(events))
	who := func(id *int64) string {
		if id == nil {
			return "—"
		}
		return name(*id)
	}
	for _, e := range events {
		label := auditLabels[e.Action]
		if label == "" {
			label = e.Action
		}
		out = append(out, auditLine{
			When: e.At.UTC().Format("2006-01-02 15:04:05"), Action: label, Detail: e.Detail, IP: e.IP,
			Actor: who(e.ActorID), Target: who(e.TargetID),
		})
	}
	return out
}

// activityFor lists the latest events about one user for their own settings page. Other people's
// names are not shown to them: an administrator's action reads "an administrator".
func (s *Server) activityFor(ctx context.Context, u models.User) ([]auditLine, error) {
	events, err := s.store.ListAuditEvents(ctx, store.AuditFilter{UserID: u.ID, Limit: auditActivitySize})
	if err != nil {
		return nil, err
	}
	return auditLines(events, func(id int64) string {
		if id == u.ID {
			return "you"
		}
		return "someone else"
	}), nil
}

// ---- API ----

func queryInt(r *http.Request, key string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return max(n, 0)
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, userID int64) {
	events, err := s.store.ListAuditEvents(r.Context(), store.AuditFilter{
		UserID: userID, BeforeID: queryInt(r, "before_id"), Limit: int(min(queryInt(r, "limit"), 1000)),
	})
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) auditRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/audit", s.apiAdmin(func(w http.ResponseWriter, r *http.Request) {
		s.listAudit(w, r, queryInt(r, "user_id"))
	}))
	mux.HandleFunc("GET /api/v1/users/{id}/audit", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		s.listAudit(w, r, u.ID)
	}))
}

// ---- retention ----

// StartMaintenance purges audit events older than AuditRetention now and every few hours, until
// Close. Without a retention period (zero) the log is kept for good.
func (s *Server) StartMaintenance() {
	if s.AuditRetention <= 0 {
		return
	}
	s.startJob(func() {
		t := time.NewTicker(auditPurgeEvery)
		defer t.Stop()
		for {
			cutoff := time.Now().Add(-s.AuditRetention)
			if n, err := s.store.PurgeAuditEvents(s.jobCtx, cutoff); err != nil {
				if s.jobCtx.Err() == nil {
					log.Error().Err(err).Msg("purge audit log")
				}
			} else if n > 0 {
				log.Info().Int64("deleted", n).Msg("purged old audit events")
			}
			select {
			case <-s.jobCtx.Done():
				return
			case <-t.C:
			}
		}
	})
}

// noisyAudit lists events that anyone, signed in or not, can trigger at will. They are recorded at
// most once per client address and minute, with a count of the ones left out, so a flood of failed
// sign-ins cannot fill the database.
var noisyAudit = map[string]bool{auditLoginFailed: true, auditLoginThrottled: true, auditPasswordWrong: true, auditAPIAuthFailed: true}

const (
	auditNoiseWindow = time.Minute
	auditNoiseMax    = 10000 // distinct (action, address) pairs remembered
)

type noiseEntry struct {
	last    time.Time
	skipped int
}

type auditNoise struct {
	mu   sync.Mutex
	seen map[string]noiseEntry
}

func newAuditNoise() *auditNoise { return &auditNoise{seen: map[string]noiseEntry{}} }

// allow reports whether to record the event now, and how many were left out since the last one.
func (n *auditNoise) allow(key string, now time.Time) (bool, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	e, ok := n.seen[key]
	if ok && now.Sub(e.last) < auditNoiseWindow {
		e.skipped++
		n.seen[key] = e
		return false, 0
	}
	if len(n.seen) >= auditNoiseMax {
		for k, v := range n.seen {
			if now.Sub(v.last) >= auditNoiseWindow {
				delete(n.seen, k)
			}
		}
		if len(n.seen) >= auditNoiseMax {
			clear(n.seen)
		}
	}
	n.seen[key] = noiseEntry{last: now}
	return true, e.skipped
}
