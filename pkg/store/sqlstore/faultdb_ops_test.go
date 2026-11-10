package sqlstore

import (
	"context"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// faultData is what every store operation in TestEveryStoreMethodReportsADatabaseFailure finds in the
// database: an administrator, an ordinary user with something of everything, and a running and a queued job.
type faultData struct {
	admin, user int64
	runningJob  string
	queuedJob   string
	at          time.Time
}

func seedFaultData(t *testing.T, s *Store) faultData {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	admin, err := s.CreateUserWithIdentity(ctx, "admin", "admin@example.com", models.Identity{Provider: models.ProviderLocal, Subject: "admin", Secret: "hash"})
	must(err)
	user, err := s.CreateUserWithIdentity(ctx, "user", "user@example.com", models.Identity{Provider: models.ProviderLocal, Subject: "user", Secret: "hash"})
	must(err)
	must(s.PutIdentity(ctx, models.Identity{UserID: user.ID, Provider: "github", Subject: "42"}))
	must(s.CreateSession(ctx, "session-hash", user.ID, models.ProviderLocal, at.Add(time.Hour)))
	must(s.CreateAccessToken(ctx, &models.AccessToken{UserID: user.ID, Name: "t", Prefix: "dx_", Hash: "token-hash"}))
	must(s.PutSetting(ctx, "k", "v"))
	must(s.PutAuthProvider(ctx, models.AuthProvider{Slug: "github", Label: "GitHub", Kind: "oauth2", ClientID: "id", ClientSecret: "secret"}))
	must(s.PutIntegration(ctx, user.ID, "withings", []byte(`{"a":1}`)))
	must(s.PutPendingTOTP(ctx, user.ID, "SECRET"))
	must(s.ReplaceRecoveryCodes(ctx, user.ID, []string{"c1", "c2"}))
	must(s.AddPasskey(ctx, models.Passkey{CredentialID: "cred", UserID: user.ID, Name: "key", Credential: "{}"}))
	must(s.CreatePasswordReset(ctx, "reset-hash", user.ID, &admin.ID, at.Add(time.Hour)))
	must(s.AddAuditEvent(ctx, &models.AuditEvent{ActorID: &user.ID, TargetID: &user.ID, Action: "x", At: at}))
	must(s.CreateDexaScan(ctx, &models.DexaScan{UserID: user.ID, MeasuredAt: at}))
	_, err = s.UpsertScaleReadings(ctx, user.ID, []models.ScaleReading{{UserID: user.ID, MeasuredAt: at, ExternalID: sp("e1"), Source: "csv"}})
	must(err)
	must(s.PutJobSchedule(ctx, models.JobSchedule{UserID: user.ID, Kind: testKind}))
	must(s.TouchWorker(ctx, "w", []string{testKind}, "1", at))
	must(s.RecordFailure(ctx, "k", at))
	_, err = s.UseNonce(ctx, "n", at.Add(time.Hour))
	must(err)

	fx := faultData{admin: admin.ID, user: user.ID, at: at}
	running := enqueue(t, s, ctx, user.ID, testKind, false, "running")
	j, ok := lease(t, s, ctx, time.Now().Add(time.Second), "w", testKind)
	if !ok || j.PublicID != running.PublicID {
		t.Fatalf("seed: leased %v %v", j.PublicID, ok)
	}
	fx.runningJob = running.PublicID
	fx.queuedJob = enqueue(t, s, ctx, user.ID, testKind, false, "queued").PublicID
	return fx
}

type faultOp struct {
	name string
	do   func(ctx context.Context, s *Store, fx faultData) error
}

// faultAbsorbed names, per operation, the statements whose failure it answers differently on purpose:
// goose reads a failed "does the version table exist" as "no", so the schema version is 0, and CancelJob
// reports a job it could not load as not found.
var faultAbsorbed = map[string][]int{
	"Stats": {1}, "SchemaVersion": {1}, "HasPending": {1, 3}, "MigrationStatus": {1},
	"CancelJob": {1}, "CancelJob running": {1},
}

// e1 returns the error, the last of the values a store method answered with.
func e1(results ...any) error {
	err, _ := results[len(results)-1].(error)
	return err
}

func faultOps() []faultOp {
	return []faultOp{
		{"CreateUser", func(c context.Context, s *Store, f faultData) error { return e1(s.CreateUser(c, "new")) }},
		{"CreateUserWithIdentity", func(c context.Context, s *Store, f faultData) error {
			return e1(s.CreateUserWithIdentity(c, "new", "new@example.com", models.Identity{Provider: models.ProviderLocal, Subject: "new", Secret: "h"}))
		}},
		{"ListUsers", func(c context.Context, s *Store, f faultData) error { return e1(s.ListUsers(c)) }},
		{"HasUsers", func(c context.Context, s *Store, f faultData) error { return e1(s.HasUsers(c)) }},
		{"GetUser", func(c context.Context, s *Store, f faultData) error { return e1(s.GetUser(c, f.user)) }},
		{"GetUserByEmail", func(c context.Context, s *Store, f faultData) error {
			return e1(s.GetUserByEmail(c, "user@example.com"))
		}},
		{"UpdateUserProfile", func(c context.Context, s *Store, f faultData) error {
			return s.UpdateUserProfile(c, f.user, "n", "n@example.com", nil)
		}},
		{"UpdateUserTimezone", func(c context.Context, s *Store, f faultData) error {
			return s.UpdateUserTimezone(c, f.user, "Europe/Berlin")
		}},
		{"SetUserRole", func(c context.Context, s *Store, f faultData) error {
			return s.SetUserRole(c, f.user, models.RoleAdmin)
		}},
		{"DeleteUser", func(c context.Context, s *Store, f faultData) error { return s.DeleteUser(c, f.user) }},
		{"UpdateUserSettings", func(c context.Context, s *Store, f faultData) error {
			return s.UpdateUserSettings(c, f.user, true, []string{"a"})
		}},
		{"SetUserStrengthLevelAuto", func(c context.Context, s *Store, f faultData) error {
			return s.SetUserStrengthLevelAuto(c, f.user, true)
		}},
		{"SetUserStrengthLevelBulk", func(c context.Context, s *Store, f faultData) error {
			return s.SetUserStrengthLevelBulk(c, f.user, true)
		}},
		{"UpdateUserHiddenColumns", func(c context.Context, s *Store, f faultData) error {
			return s.UpdateUserHiddenColumns(c, f.user, []string{"a"})
		}},
		{"GetIntegration", func(c context.Context, s *Store, f faultData) error {
			return e1(s.GetIntegration(c, f.user, "withings"))
		}},
		{"PutIntegration", func(c context.Context, s *Store, f faultData) error {
			return s.PutIntegration(c, f.user, "withings", []byte("{}"))
		}},
		{"DeleteIntegration", func(c context.Context, s *Store, f faultData) error {
			return s.DeleteIntegration(c, f.user, "withings")
		}},
		{"ListUsersWithIntegration", func(c context.Context, s *Store, f faultData) error {
			return e1(s.ListUsersWithIntegration(c, "withings"))
		}},
		{"GetIdentity", func(c context.Context, s *Store, f faultData) error { return e1(s.GetIdentity(c, "github", "42")) }},
		{"PutIdentity", func(c context.Context, s *Store, f faultData) error {
			return s.PutIdentity(c, models.Identity{UserID: f.user, Provider: "other", Subject: "1"})
		}},
		{"DeleteIdentity", func(c context.Context, s *Store, f faultData) error { return s.DeleteIdentity(c, f.user, "github") }},
		{"ListUserIdentities", func(c context.Context, s *Store, f faultData) error { return e1(s.ListUserIdentities(c, f.user)) }},
		{"UserProviders", func(c context.Context, s *Store, f faultData) error { return e1(s.UserProviders(c, f.user)) }},
		{"IdentityProviders", func(c context.Context, s *Store, f faultData) error { return e1(s.IdentityProviders(c)) }},
		{"CreateSession", func(c context.Context, s *Store, f faultData) error {
			return s.CreateSession(c, "other", f.user, models.ProviderLocal, f.at.Add(time.Hour))
		}},
		{"UserBySession", func(c context.Context, s *Store, f faultData) error { return e1(s.UserBySession(c, "session-hash")) }},
		{"GetSession", func(c context.Context, s *Store, f faultData) error { return e1(s.GetSession(c, "session-hash")) }},
		{"DeleteSession", func(c context.Context, s *Store, f faultData) error { return s.DeleteSession(c, "session-hash") }},
		{"DeleteUserSessions", func(c context.Context, s *Store, f faultData) error { return s.DeleteUserSessions(c, f.user) }},
		{"CreateAccessToken", func(c context.Context, s *Store, f faultData) error {
			return s.CreateAccessToken(c, &models.AccessToken{UserID: f.user, Name: "n", Prefix: "p", Hash: "h2"})
		}},
		{"ListAccessTokens", func(c context.Context, s *Store, f faultData) error { return e1(s.ListAccessTokens(c, f.user)) }},
		{"DeleteAccessToken", func(c context.Context, s *Store, f faultData) error { return s.DeleteAccessToken(c, f.user, 1) }},
		{"DeleteUserAccessTokens", func(c context.Context, s *Store, f faultData) error { return s.DeleteUserAccessTokens(c, f.user) }},
		{"UserByTokenHash", func(c context.Context, s *Store, f faultData) error { return e1(s.UserByTokenHash(c, "token-hash")) }},
		{"GetSetting", func(c context.Context, s *Store, f faultData) error { return e1(s.GetSetting(c, "k")) }},
		{"PutSetting", func(c context.Context, s *Store, f faultData) error { return s.PutSetting(c, "k", "v2") }},
		{"DeleteSetting", func(c context.Context, s *Store, f faultData) error { return s.DeleteSetting(c, "k") }},
		{"SettingsGeneration", func(c context.Context, s *Store, f faultData) error { return e1(s.SettingsGeneration(c)) }},
		{"ListAuthProviders", func(c context.Context, s *Store, f faultData) error { return e1(s.ListAuthProviders(c)) }},
		{"GetAuthProvider", func(c context.Context, s *Store, f faultData) error { return e1(s.GetAuthProvider(c, "github")) }},
		{"PutAuthProvider", func(c context.Context, s *Store, f faultData) error {
			return s.PutAuthProvider(c, models.AuthProvider{Slug: "github", Label: "G", Kind: "oauth2", ClientID: "id", ClientSecret: "s2"})
		}},
		{"DeleteAuthProvider", func(c context.Context, s *Store, f faultData) error { return s.DeleteAuthProvider(c, "github") }},
		{"Stats", func(c context.Context, s *Store, f faultData) error { return e1(s.Stats(c)) }},
		{"GetTOTP", func(c context.Context, s *Store, f faultData) error { return e1(s.GetTOTP(c, f.user)) }},
		{"PutPendingTOTP", func(c context.Context, s *Store, f faultData) error { return s.PutPendingTOTP(c, f.user, "S2") }},
		{"ConfirmTOTP", func(c context.Context, s *Store, f faultData) error { return s.ConfirmTOTP(c, f.user, 5) }},
		{"UseTOTPStep", func(c context.Context, s *Store, f faultData) error { return e1(s.UseTOTPStep(c, f.user, 5)) }},
		{"DeleteTOTP", func(c context.Context, s *Store, f faultData) error { return s.DeleteTOTP(c, f.user) }},
		{"ReplaceRecoveryCodes", func(c context.Context, s *Store, f faultData) error {
			return s.ReplaceRecoveryCodes(c, f.user, []string{"a", "b"})
		}},
		{"UseRecoveryCode", func(c context.Context, s *Store, f faultData) error { return e1(s.UseRecoveryCode(c, f.user, "c1")) }},
		{"CountRecoveryCodes", func(c context.Context, s *Store, f faultData) error { return e1(s.CountRecoveryCodes(c, f.user)) }},
		{"TwoFactorUsers", func(c context.Context, s *Store, f faultData) error { return e1(s.TwoFactorUsers(c)) }},
		{"EnsurePasskeyHandle", func(c context.Context, s *Store, f faultData) error { return e1(s.EnsurePasskeyHandle(c, f.user)) }},
		{"UserByPasskeyHandle", func(c context.Context, s *Store, f faultData) error { return e1(s.UserByPasskeyHandle(c, "nope")) }},
		{"ListPasskeys", func(c context.Context, s *Store, f faultData) error { return e1(s.ListPasskeys(c, f.user)) }},
		{"AddPasskey", func(c context.Context, s *Store, f faultData) error {
			return s.AddPasskey(c, models.Passkey{CredentialID: "c2", UserID: f.user, Name: "k", Credential: "{}"})
		}},
		{"UpdatePasskeyUse", func(c context.Context, s *Store, f faultData) error { return s.UpdatePasskeyUse(c, "cred", "{}", 3) }},
		{"RenamePasskey", func(c context.Context, s *Store, f faultData) error { return s.RenamePasskey(c, f.user, "cred", "new") }},
		{"DeletePasskey", func(c context.Context, s *Store, f faultData) error { return s.DeletePasskey(c, f.user, "cred") }},
		{"CreatePasswordReset", func(c context.Context, s *Store, f faultData) error {
			return s.CreatePasswordReset(c, "r2", f.user, nil, f.at.Add(time.Hour))
		}},
		{"PasswordResetUser", func(c context.Context, s *Store, f faultData) error { return e1(s.PasswordResetUser(c, "reset-hash")) }},
		{"CompletePasswordReset", func(c context.Context, s *Store, f faultData) error {
			return e1(s.CompletePasswordReset(c, "reset-hash", "newhash"))
		}},
		{"DeletePasswordResets", func(c context.Context, s *Store, f faultData) error { return s.DeletePasswordResets(c, f.user) }},
		{"AddAuditEvent", func(c context.Context, s *Store, f faultData) error {
			return s.AddAuditEvent(c, &models.AuditEvent{ActorID: &f.user, Action: "y", At: f.at})
		}},
		{"ListAuditEvents", func(c context.Context, s *Store, f faultData) error {
			return e1(s.ListAuditEvents(c, store.AuditFilter{UserID: f.user, BeforeID: 99, Limit: 5}))
		}},
		{"PurgeAuditEvents", func(c context.Context, s *Store, f faultData) error {
			return e1(s.PurgeAuditEvents(c, f.at.Add(time.Hour)))
		}},
		{"UseNonce", func(c context.Context, s *Store, f faultData) error {
			return e1(s.UseNonce(c, "n2", f.at.Add(time.Hour)))
		}},
		{"NonceSeen", func(c context.Context, s *Store, f faultData) error { return e1(s.NonceSeen(c, "n")) }},
		{"PurgeNonces", func(c context.Context, s *Store, f faultData) error {
			return e1(s.PurgeNonces(c, f.at.Add(2*time.Hour)))
		}},
		{"RecordFailure", func(c context.Context, s *Store, f faultData) error { return s.RecordFailure(c, "k", f.at) }},
		{"CountFailures", func(c context.Context, s *Store, f faultData) error {
			return e1(s.CountFailures(c, "k", f.at.Add(-time.Hour)))
		}},
		{"ClearFailures", func(c context.Context, s *Store, f faultData) error { return s.ClearFailures(c, "k") }},
		{"PurgeFailures", func(c context.Context, s *Store, f faultData) error {
			return e1(s.PurgeFailures(c, f.at.Add(time.Hour)))
		}},
		{"EnqueueJob", func(c context.Context, s *Store, f faultData) error {
			return e1(s.EnqueueJob(c, models.NewJob{UserID: f.user, Kind: testKind, Origin: models.JobOriginUser, DedupeKey: "fresh", Payload: "p", MaxAttempts: 3, Timeout: time.Minute}))
		}},
		{"EnqueueJob of a key that is already queued", func(c context.Context, s *Store, f faultData) error {
			return e1(s.EnqueueJob(c, models.NewJob{UserID: f.user, Kind: testKind, Origin: models.JobOriginUser, DedupeKey: "queued", MaxAttempts: 3, Timeout: time.Minute}))
		}},
		{"GetJob", func(c context.Context, s *Store, f faultData) error { return e1(s.GetJob(c, f.runningJob)) }},
		{"ListJobs", func(c context.Context, s *Store, f faultData) error {
			return e1(s.ListJobs(c, models.JobFilter{UserID: f.user, Kinds: []string{testKind}, States: []string{models.JobQueued}, Limit: 5}))
		}},
		{"LastFinishedStates", func(c context.Context, s *Store, f faultData) error {
			return e1(s.LastFinishedStates(c, testKind, 3, f.at.Add(-time.Hour)))
		}},
		{"LeaseJob", func(c context.Context, s *Store, f faultData) error {
			return e1(s.LeaseJob(c, models.LeaseRequest{Kinds: []string{testKind}, Owner: "w2", TokenHash: "tok-w2", LeaseFor: time.Minute, MaxRunning: map[string]int{testKind: 5}}, f.at))
		}},
		{"HeartbeatJob", func(c context.Context, s *Store, f faultData) error {
			p := "50%"
			return e1(s.HeartbeatJob(c, f.runningJob, "tok-w", time.Minute, &p, f.at))
		}},
		{"CompleteJob ok", func(c context.Context, s *Store, f faultData) error {
			return e1(s.CompleteJob(c, f.runningJob, "tok-w", models.Completion{Outcome: models.OutcomeOK, Result: "{}"}, f.at))
		}},
		{"CompleteJob retry", func(c context.Context, s *Store, f faultData) error {
			return e1(s.CompleteJob(c, f.runningJob, "tok-w", models.Completion{Outcome: models.OutcomeRetry, Error: "x", RetryAt: f.at.Add(time.Minute)}, f.at))
		}},
		{"CompleteJob fail", func(c context.Context, s *Store, f faultData) error {
			return e1(s.CompleteJob(c, f.runningJob, "tok-w", models.Completion{Outcome: models.OutcomeFail, Error: "x"}, f.at))
		}},
		{"ReleaseJob", func(c context.Context, s *Store, f faultData) error {
			return s.ReleaseJob(c, f.runningJob, "tok-w", f.at)
		}},
		{"CancelJob", func(c context.Context, s *Store, f faultData) error {
			return e1(s.CancelJob(c, f.queuedJob, f.user, f.at))
		}},
		{"CancelJob running", func(c context.Context, s *Store, f faultData) error {
			return e1(s.CancelJob(c, f.runningJob, f.user, f.at))
		}},
		{"CancelJobs", func(c context.Context, s *Store, f faultData) error {
			return s.CancelJobs(c, f.user, []string{testKind}, f.at)
		}},
		{"ReapExpiredLeases", func(c context.Context, s *Store, f faultData) error {
			return e1(s.ReapExpiredLeases(c, f.at.Add(time.Hour), func(models.Job) time.Duration { return time.Minute }))
		}},
		{"RunningJob", func(c context.Context, s *Store, f faultData) error {
			return e1(s.RunningJob(c, f.runningJob, "tok-w"))
		}},
		{"JobStats", func(c context.Context, s *Store, f faultData) error { return e1(s.JobStats(c, f.at.Add(-time.Hour))) }},
		{"ExpireQueuedJobs", func(c context.Context, s *Store, f faultData) error { return e1(s.ExpireQueuedJobs(c, f.at)) }},
		{"PurgeJobs", func(c context.Context, s *Store, f faultData) error { return e1(s.PurgeJobs(c, f.at.Add(time.Hour))) }},
		{"TouchWorker", func(c context.Context, s *Store, f faultData) error {
			return s.TouchWorker(c, "w", []string{testKind}, "2", f.at)
		}},
		{"ListWorkers", func(c context.Context, s *Store, f faultData) error {
			return e1(s.ListWorkers(c, f.at.Add(-time.Hour)))
		}},
		{"PurgeWorkers", func(c context.Context, s *Store, f faultData) error {
			return e1(s.PurgeWorkers(c, f.at.Add(time.Hour)))
		}},
		{"GetJobSchedule", func(c context.Context, s *Store, f faultData) error { return e1(s.GetJobSchedule(c, f.user, testKind)) }},
		{"ListJobSchedules", func(c context.Context, s *Store, f faultData) error { return e1(s.ListJobSchedules(c, testKind)) }},
		{"PutJobSchedule", func(c context.Context, s *Store, f faultData) error {
			return s.PutJobSchedule(c, models.JobSchedule{UserID: f.user, Kind: testKind, ConsecutiveFailures: 1})
		}},
		{"DeleteJobSchedule", func(c context.Context, s *Store, f faultData) error { return s.DeleteJobSchedule(c, f.user, testKind) }},
		{"SetUserJobPriority", func(c context.Context, s *Store, f faultData) error { return s.SetUserJobPriority(c, f.user, 5) }},
		{"CreateDexaScan", func(c context.Context, s *Store, f faultData) error {
			return s.CreateDexaScan(c, &models.DexaScan{UserID: f.user, MeasuredAt: f.at.Add(time.Hour)})
		}},
		{"ListDexaScans", func(c context.Context, s *Store, f faultData) error { return e1(s.ListDexaScans(c, f.user)) }},
		{"ListDexaScansPage", func(c context.Context, s *Store, f faultData) error {
			return e1(s.ListDexaScansPage(c, f.user, store.ListFilter{Since: f.at.Add(-time.Hour), Until: f.at.Add(time.Hour), Limit: 1}))
		}},
		{"DeleteDexaScan", func(c context.Context, s *Store, f faultData) error { return s.DeleteDexaScan(c, f.user, 1) }},
		{"UpsertScaleReadings", func(c context.Context, s *Store, f faultData) error {
			return e1(s.UpsertScaleReadings(c, f.user, []models.ScaleReading{
				{UserID: f.user, MeasuredAt: f.at, ExternalID: sp("e1"), Source: "csv"},
				{UserID: f.user, MeasuredAt: f.at.Add(time.Hour), ExternalID: sp("e2"), Source: "csv"}}))
		}},
		{"ListScaleReadings", func(c context.Context, s *Store, f faultData) error { return e1(s.ListScaleReadings(c, f.user)) }},
		{"ListScaleReadingsPage", func(c context.Context, s *Store, f faultData) error {
			return e1(s.ListScaleReadingsPage(c, f.user, store.ListFilter{Since: f.at.Add(-time.Hour), Limit: 1}))
		}},
		{"DeleteScaleReading", func(c context.Context, s *Store, f faultData) error { return s.DeleteScaleReading(c, f.user, 1) }},
		{"BodyVersion", func(c context.Context, s *Store, f faultData) error { return e1(s.BodyVersion(c, f.user)) }},
		{"DeleteUserBodyData", func(c context.Context, s *Store, f faultData) error { return s.DeleteUserBodyData(c, f.user) }},
		{"BodyUserIDs", func(c context.Context, s *Store, f faultData) error { return e1(s.BodyUserIDs(c)) }},
		{"BodyStats", func(c context.Context, s *Store, f faultData) error { return e1(s.BodyStats(c)) }},
		{"Ping", func(c context.Context, s *Store, f faultData) error { return s.Ping(c) }},
		{"SchemaVersion", func(c context.Context, s *Store, f faultData) error { return e1(s.SchemaVersion(c)) }},
		{"HasPending", func(c context.Context, s *Store, f faultData) error { return e1(s.HasPending(c)) }},
		{"MigrationStatus", func(c context.Context, s *Store, f faultData) error { return e1(s.MigrationStatus(c)) }},
		{"HasEncryptedValues", func(c context.Context, s *Store, f faultData) error { return e1(s.HasEncryptedValues(c)) }},
	}
}

// Whatever the database refuses, a store method has to say so: swallowing the error would show an
// empty list, or report a change as made. Each method runs once per statement it sends, and each time
// that one statement fails.
func TestEveryStoreMethodReportsADatabaseFailure(t *testing.T) {
	for _, op := range faultOps() {
		t.Run(op.name, func(t *testing.T) {
			n := failEveryStatement(t, nil, seedFaultData, op.do, faultAbsorbed[op.name]...)
			if n == 0 {
				t.Fatal("the operation sent no statement")
			}
		})
	}
}

func sp(s string) *string { return &s }
