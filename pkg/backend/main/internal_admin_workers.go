package mainbackend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// The workers section of the Admin page: the configuration of each kind of job, the running workers
// and the queue, plus the per-user priority. Behind internalAdmin like every Admin endpoint.

const workerJobsShown = 100

func (s *Backend) internalAdminWorkerRoutes(mux *http.ServeMux) {
	p := apiclient.Prefix + "/admin"
	mux.HandleFunc("GET "+p+"/workers", s.internalAdmin(s.internalAdminWorkers))
	mux.HandleFunc("POST "+p+"/workers/kinds/{kind}", s.internalAdmin(s.internalAdminSaveKind))
	mux.HandleFunc("POST "+p+"/workers/kinds/{kind}/resume", s.internalAdmin(s.internalAdminResumeKind))
	mux.HandleFunc("POST "+p+"/workers/global", s.internalAdmin(s.internalAdminSaveWorkersGlobal))
	mux.HandleFunc("POST "+p+"/workers/jobs/{id}/cancel", s.internalAdmin(s.internalAdminCancelJob))
	mux.HandleFunc("POST "+p+"/workers/schedules/{user}/{kind}/resume", s.internalAdmin(s.internalAdminResumeSchedule))
	mux.HandleFunc("POST "+p+"/users/{id}/priority", s.internalAdmin(s.internalAdminSetPriority))
}

func (s *Backend) internalAdminWorkers(w http.ResponseWriter, r *http.Request, _ models.User) {
	ctx := r.Context()
	now := time.Now()
	stats, err := s.store.JobStats(ctx, now.Add(-24*time.Hour))
	if err != nil {
		internalError(w, err)
		return
	}
	byKind := map[string]models.JobKindStats{}
	for _, st := range stats {
		byKind[st.Kind] = st
	}
	instances, err := s.store.ListWorkers(ctx, now.Add(-workerSeen.Get()))
	if err != nil {
		internalError(w, err)
		return
	}
	live := s.cfg().workers
	page := apiclient.WorkersPage{DefaultPriority: live.DefaultPriority, KeepHistoryDays: int(live.KeepHistory / (24 * time.Hour))}
	for _, in := range instances {
		page.Workers = append(page.Workers, apiclient.WorkerInstanceView{ID: in.ID, Version: in.Version, Kinds: in.Kinds, LastSeen: in.LastSeen.UTC().Format("2006-01-02 15:04:05 UTC")})
	}
	for _, p := range s.providers {
		pv := apiclient.WorkerProviderView{Name: p.Name, Label: p.Label}
		for _, k := range p.Kinds {
			ks := live.Kinds[k.Kind]
			kv := apiclient.WorkerKindView{
				Kind: k.Kind, Label: k.Label, Enabled: ks.Enabled, PausedReason: ks.PausedReason,
				MaxRunning: ks.MaxRunning, MaxAttempts: ks.MaxAttempts, RetryBaseSeconds: int(ks.RetryBase / time.Second),
				TimeoutSeconds: int(ks.Timeout / time.Second), ScheduleHours: int(ks.ScheduleEvery / time.Hour),
				PauseAfter: ks.PauseAfter, Breaker: ks.Breaker, Schedulable: k.ScheduleFor != nil,
				Queued: byKind[k.Kind].Queued, Running: byKind[k.Kind].Running, Failed24h: byKind[k.Kind].Failed,
			}
			for _, o := range k.Options {
				kv.Options = append(kv.Options, apiclient.WorkerOptionView{Name: o.Name, Label: o.Label, Value: ks.Options[o.Name], Min: o.Min, Max: o.Max, Millis: o.Millis})
			}
			for _, in := range instances {
				for _, ik := range in.Kinds {
					if ik == k.Kind {
						kv.WorkersOnline++
					}
				}
			}
			if k.ScheduleFor != nil {
				schedules, err := s.store.ListJobSchedules(ctx, k.Kind)
				if err != nil {
					internalError(w, err)
					return
				}
				for _, sc := range schedules {
					if sc.PausedReason != "" {
						kv.PausedSchedules++
					}
				}
			}
			pv.Kinds = append(pv.Kinds, kv)
		}
		page.Providers = append(page.Providers, pv)
	}

	jobs, err := s.store.ListJobs(ctx, models.JobFilter{Limit: workerJobsShown})
	if err != nil {
		internalError(w, err)
		return
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	byID := make(map[int64]models.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	const layout = "2006-01-02 15:04:05"
	for _, j := range jobs {
		u := byID[j.UserID]
		page.Jobs = append(page.Jobs, apiclient.WorkerJobView{
			ID: j.PublicID, Kind: j.Kind, User: u.Name, UserID: j.UserID, Priority: u.JobPriority, UserIsAdmin: u.IsAdmin(),
			State: j.State, Origin: j.Origin, Attempts: j.Attempts, MaxAttempts: j.MaxAttempts, Error: j.Error,
			Created: j.CreatedAt.UTC().Format(layout), Updated: j.UpdatedAt.UTC().Format(layout), Active: models.JobActive(j.State),
		})
	}
	internalJSON(w, http.StatusOK, page)
}

// kindFromPath finds the kind the path names.
func (s *Backend) kindFromPath(w http.ResponseWriter, r *http.Request) (*kindSpec, bool) {
	kind, err := url.PathUnescape(r.PathValue("kind"))
	spec, ok := s.kinds[kind]
	if err != nil || !ok {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return nil, false
	}
	return spec, true
}

// within checks a whole number against its range and names the field in the message.
func within(label string, n, lo, hi int) string {
	if n < lo || n > hi {
		return fmt.Sprintf("%s must be a number from %d to %d.", label, lo, hi)
	}
	return ""
}

func (s *Backend) internalAdminSaveKind(w http.ResponseWriter, r *http.Request, _ models.User) {
	spec, ok := s.kindFromPath(w, r)
	if !ok {
		return
	}
	var in apiclient.WorkerKindForm
	if !internalDecode(w, r, &in) {
		return
	}
	for _, c := range []string{
		within("Jobs at once", in.MaxRunning, 0, 100),
		within("Attempts", in.MaxAttempts, 1, maxAttemptsLimit),
		within("Retry delay (seconds)", in.RetryBaseSeconds, 1, int(maxRetryDelay/time.Second)),
		within("Time limit (seconds)", in.TimeoutSeconds, 10, maxJobTimeoutSeconds),
		within("Automatic run interval (hours)", in.ScheduleHours, 0, 24*30),
		within("Pause automatic runs after (failures)", in.PauseAfter, 0, 100),
		within("Pause the kind after (failures)", in.Breaker, 0, 1000),
	} {
		if c != "" {
			adminMsg(w, c)
			return
		}
	}
	for _, o := range spec.Options {
		if v, given := in.Options[o.Name]; given {
			if c := within(o.Label, v, o.Min, o.Max); c != "" {
				adminMsg(w, c)
				return
			}
		}
	}
	ctx := r.Context()
	put := func(field string, v int) error {
		return s.store.PutSetting(ctx, kindSettingName(spec.Kind, field), strconv.Itoa(v))
	}
	var err error
	for _, f := range []struct {
		name string
		v    int
	}{
		{fieldMaxRunning, in.MaxRunning}, {fieldMaxAttempts, in.MaxAttempts}, {fieldRetryBase, in.RetryBaseSeconds},
		{fieldTimeout, in.TimeoutSeconds}, {fieldScheduleHours, in.ScheduleHours}, {fieldPauseAfter, in.PauseAfter}, {fieldBreaker, in.Breaker},
	} {
		if err == nil {
			err = put(f.name, f.v)
		}
	}
	for _, o := range spec.Options {
		if v, given := in.Options[o.Name]; given && err == nil {
			err = s.store.PutSetting(ctx, optionSettingName(spec.Kind, o.Name), strconv.Itoa(v))
		}
	}
	if err == nil {
		if in.Enabled {
			err = s.enableKind(ctx, spec.Kind)
		} else {
			err = s.store.PutSetting(ctx, kindSettingName(spec.Kind, fieldEnabled), "false")
		}
	}
	if err == nil {
		err = s.reloadSettings(ctx)
	}
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "workers "+spec.Kind)
	adminMsg(w, spec.Label+" settings saved.")
}

// enableKind turns a kind on and forgets the failures that made the circuit breaker turn it off.
func (s *Backend) enableKind(ctx context.Context, kind string) error {
	if s.cfg().workers.Kinds[kind].PausedReason == "" && s.cfg().workers.Kinds[kind].Enabled {
		return nil // nothing to undo
	}
	if err := s.store.DeleteSetting(ctx, kindSettingName(kind, fieldEnabled)); err != nil {
		return err
	}
	if err := s.store.DeleteSetting(ctx, kindSettingName(kind, fieldPausedReason)); err != nil {
		return err
	}
	return s.store.PutSetting(ctx, kindSettingName(kind, fieldBreakerSince), time.Now().UTC().Format(time.RFC3339Nano))
}

func (s *Backend) internalAdminResumeKind(w http.ResponseWriter, r *http.Request, _ models.User) {
	spec, ok := s.kindFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	err := s.store.DeleteSetting(ctx, kindSettingName(spec.Kind, fieldEnabled))
	if err == nil {
		err = s.store.DeleteSetting(ctx, kindSettingName(spec.Kind, fieldPausedReason))
	}
	if err == nil {
		err = s.store.PutSetting(ctx, kindSettingName(spec.Kind, fieldBreakerSince), time.Now().UTC().Format(time.RFC3339Nano))
	}
	if err == nil {
		err = s.reloadSettings(ctx)
	}
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "workers "+spec.Kind+" resumed")
	adminMsg(w, spec.Label+" is on again.")
}

func (s *Backend) internalAdminSaveWorkersGlobal(w http.ResponseWriter, r *http.Request, _ models.User) {
	var in apiclient.WorkersGlobalForm
	if !internalDecode(w, r, &in) {
		return
	}
	for _, c := range []string{
		within("The default priority", in.DefaultPriority, models.MinJobPriority, models.MaxJobPriority),
		within("Days to keep finished jobs", in.KeepHistoryDays, 1, maxSettingDays),
	} {
		if c != "" {
			adminMsg(w, c)
			return
		}
	}
	ctx := r.Context()
	err := s.store.PutSetting(ctx, settingWorkersDefaultPriority, strconv.Itoa(in.DefaultPriority))
	if err == nil {
		err = s.store.PutSetting(ctx, settingWorkersKeepDays, strconv.Itoa(in.KeepHistoryDays))
	}
	if err == nil {
		err = s.reloadSettings(ctx)
	}
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "workers")
	adminMsg(w, "Worker settings saved.")
}

func (s *Backend) internalAdminCancelJob(w http.ResponseWriter, r *http.Request, _ models.User) {
	j, err := s.store.CancelJob(r.Context(), r.PathValue("id"), 0, time.Now())
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, j.UserID, auditJobCanceled, j.Kind)
	if j.State == models.JobRunning {
		adminMsg(w, "The worker was asked to stop.")
		return
	}
	adminMsg(w, "The job was canceled.")
}

func (s *Backend) internalAdminResumeSchedule(w http.ResponseWriter, r *http.Request, _ models.User) {
	spec, ok := s.kindFromPath(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return
	}
	if _, err := s.store.GetUser(r.Context(), id); err != nil {
		internalError(w, err)
		return
	}
	s.resumeSchedule(r.Context(), id, spec.Kind)
	s.audit(r, id, auditSettingChanged, "workers "+spec.Kind+" schedule resumed")
	adminMsg(w, "Automatic runs will start again.")
}

func (s *Backend) internalAdminSetPriority(w http.ResponseWriter, r *http.Request, _ models.User) {
	t, ok := s.internalTarget(w, r)
	if !ok {
		return
	}
	var in struct{ Priority int }
	if !internalDecode(w, r, &in) {
		return
	}
	if c := within("The priority", in.Priority, models.MinJobPriority, models.MaxJobPriority); c != "" {
		adminMsg(w, c)
		return
	}
	if err := s.store.SetUserJobPriority(r.Context(), t.ID, in.Priority); err != nil {
		if errors.Is(err, store.ErrInvalid) {
			adminMsg(w, err.Error())
			return
		}
		internalError(w, err)
		return
	}
	s.audit(r, t.ID, auditJobPriority, strconv.Itoa(in.Priority))
	adminMsg(w, "Priority of "+t.Name+" is now "+strconv.Itoa(in.Priority)+".")
}
