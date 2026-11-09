package mainbackend

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// The scheduler enqueues recurring jobs (import from a scale, send estimates to an exporter) for
// every eligible user, at the interval an administrator set for the kind. It runs in the
// maintenance backend only (--maintenance), so one process does it; the queue's dedupe key means a
// second one could not double a job anyway.
//
// Two fail-safes stop a recurring job from hammering a service that does not work:
//   - after PauseAfter failed runs in a row, the user's schedule for that kind is paused, with a
//     reason shown to the user and the administrator, until the user fixes the connection (the
//     handlers call resumeSchedule) or an administrator resumes it;
//   - after Breaker failed jobs in a row, whoever they belong to, the whole kind is paused (the
//     circuit breaker), until an administrator resumes it.

var (
	maintainTick   = 30 * time.Second
	housekeepEvery = time.Hour
)

// startWorkerMaintenance runs the sweeps and the scheduler until Close.
func (s *Backend) startWorkerMaintenance() {
	s.bg.Start(func() {
		t := time.NewTicker(maintainTick)
		defer t.Stop()
		var lastHousekeeping time.Time
		for {
			s.lastSweep.Store(0) // always sweep here: the leasing requests only do it when they are busy
			s.maintainQueue(s.bg.Ctx)
			s.runScheduler(s.bg.Ctx)
			if time.Since(lastHousekeeping) >= housekeepEvery {
				lastHousekeeping = time.Now()
				s.purgeJobs(s.bg.Ctx)
			}
			select {
			case <-s.bg.Ctx.Done():
				return
			case <-t.C:
			}
		}
	})
}

// purgeJobs forgets jobs that ended long ago and workers that have not been seen for a day.
func (s *Backend) purgeJobs(ctx context.Context) {
	if keep := s.cfg().workers.KeepHistory; keep > 0 {
		if n, err := s.store.PurgeJobs(ctx, time.Now().Add(-keep)); err != nil {
			if ctx.Err() == nil {
				log.Error().Err(err).Msg("purge old jobs")
			}
		} else if n > 0 {
			log.Info().Int64("deleted", n).Msg("purged old jobs")
		}
	}
	if _, err := s.store.PurgeWorkers(ctx, time.Now().Add(-24*time.Hour)); err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("purge old workers")
	}
}

// runScheduler enqueues the jobs that are due.
func (s *Backend) runScheduler(ctx context.Context) {
	for _, kind := range s.kindNames() {
		spec := s.kinds[kind]
		ks := s.cfg().workers.Kinds[kind]
		if spec.ScheduleFor == nil || !ks.Enabled || ks.ScheduleEvery <= 0 {
			continue
		}
		if err := s.scheduleKind(ctx, spec, ks); err != nil && ctx.Err() == nil {
			log.Error().Err(err).Str("kind", kind).Msg("schedule jobs")
		}
	}
}

func (s *Backend) scheduleKind(ctx context.Context, spec *kindSpec, ks kindSettings) error {
	targets, err := spec.ScheduleFor(ctx, s)
	if err != nil {
		return err
	}
	known, err := s.store.ListJobSchedules(ctx, spec.Kind)
	if err != nil {
		return err
	}
	byUser := make(map[int64]models.JobSchedule, len(known))
	for _, k := range known {
		byUser[k.UserID] = k
	}
	now := time.Now()
	for _, t := range targets {
		sc := byUser[t.UserID]
		if sc.PausedReason != "" || (sc.LastEnqueuedAt != nil && now.Sub(*sc.LastEnqueuedAt) < ks.ScheduleEvery) {
			continue
		}
		_, _, err := s.enqueueJob(ctx, enqueueSpec{UserID: t.UserID, Kind: spec.Kind, Origin: models.JobOriginSchedule, Payload: t.Payload, Expires: ks.ScheduleEvery})
		if errors.Is(err, errKindPaused) {
			return nil
		}
		if errors.Is(err, store.ErrNotFound) { // the user was deleted meanwhile
			continue
		}
		if err != nil {
			return err
		}
		sc.UserID, sc.Kind, sc.LastEnqueuedAt = t.UserID, spec.Kind, &now
		if err := s.store.PutJobSchedule(ctx, sc); err != nil {
			return err
		}
	}
	return nil
}

// afterJob is called when a job ended (or was ended by the reaper). It keeps the schedule's
// failure count and watches for a kind that keeps failing.
func (s *Backend) afterJob(ctx context.Context, j models.Job) {
	if models.JobActive(j.State) {
		return
	}
	ks := s.cfg().workers.Kinds[j.Kind]
	if j.Origin == models.JobOriginSchedule && (j.State == models.JobSucceeded || j.State == models.JobFailed) {
		s.recordScheduledResult(ctx, j, ks)
	}
	if j.State == models.JobFailed && ks.Breaker > 0 {
		s.checkBreaker(ctx, j.Kind, ks)
	}
}

func (s *Backend) recordScheduledResult(ctx context.Context, j models.Job, ks kindSettings) {
	sc, err := s.store.GetJobSchedule(ctx, j.UserID, j.Kind)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Error().Err(err).Msg("read the schedule")
		return
	}
	sc.UserID, sc.Kind = j.UserID, j.Kind
	if j.State == models.JobSucceeded {
		if sc.ConsecutiveFailures == 0 {
			return
		}
		sc.ConsecutiveFailures = 0
	} else {
		sc.ConsecutiveFailures++
		if ks.PauseAfter > 0 && sc.ConsecutiveFailures >= ks.PauseAfter && sc.PausedReason == "" {
			sc.PausedReason = fmt.Sprintf("Automatic runs were paused after %d failures in a row. Last error: %s", sc.ConsecutiveFailures, clip(j.Error, 200))
			log.Warn().Int64("user", j.UserID).Str("kind", j.Kind).Msg("a user's schedule was paused after repeated failures")
		}
	}
	if err := s.store.PutJobSchedule(ctx, sc); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Error().Err(err).Msg("write the schedule")
	}
}

// checkBreaker pauses a kind whose latest ks.Breaker jobs all failed.
func (s *Backend) checkBreaker(ctx context.Context, kind string, ks kindSettings) {
	states, err := s.store.LastFinishedStates(ctx, kind, ks.Breaker, ks.BreakerSince)
	if err != nil {
		log.Error().Err(err).Msg("check the circuit breaker")
		return
	}
	if len(states) < ks.Breaker {
		return
	}
	for _, st := range states {
		if st != models.JobFailed {
			return
		}
	}
	reason := fmt.Sprintf("Paused automatically after %d jobs failed in a row (%s). Check the workers and the service, then resume it.",
		ks.Breaker, time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	if err := s.store.PutSetting(ctx, kindSettingName(kind, fieldEnabled), "false"); err != nil {
		log.Error().Err(err).Msg("pause the kind")
		return
	}
	if err := s.store.PutSetting(ctx, kindSettingName(kind, fieldPausedReason), reason); err != nil {
		log.Error().Err(err).Msg("write the reason")
	}
	log.Error().Str("kind", kind).Int("failures", ks.Breaker).Msg("circuit breaker: the kind of job was paused")
	if err := s.reloadSettings(ctx); err != nil {
		log.Error().Err(err).Msg("reload the settings")
	}
}

// resumeSchedule lets the scheduler enqueue for a user again, for the given kinds (a user fixed their
// connection) or for every kind when none is named.
func (s *Backend) resumeSchedule(ctx context.Context, userID int64, kinds ...string) {
	if len(kinds) == 0 {
		kinds = s.kindNames()
	}
	for _, k := range kinds {
		if err := s.store.DeleteJobSchedule(ctx, userID, k); err != nil {
			log.Warn().Err(err).Msg("resume the schedule")
		}
	}
}

// pausedReason says why automatic runs of a kind were paused for the user, or "" when they are not.
func (s *Backend) pausedReason(ctx context.Context, userID int64, kind string) string {
	sc, err := s.store.GetJobSchedule(ctx, userID, kind)
	if err != nil {
		return ""
	}
	return sc.PausedReason
}
