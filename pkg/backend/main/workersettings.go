package mainbackend

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
)

// The settings of the workers are Admin settings like all others: stored in the database, applied
// without a restart, and served to the workers (the options) through workerapi.Config. One group
// per kind, named workers.<kind>.<field>, and two global ones.
const (
	settingWorkersDefaultPriority = "workers.default_priority"
	settingWorkersKeepDays        = "workers.keep_history_days"

	defaultKeepJobDays   = 30
	maxAttemptsLimit     = 10
	maxRetryDelay        = time.Hour
	maxJobTimeoutSeconds = 24 * 60 * 60
)

// Fields of a kind. Their stored names are workers.<kind>.<field>; options are workers.<kind>.opt.<name>.
const (
	fieldEnabled       = "enabled"
	fieldPausedReason  = "paused_reason"
	fieldMaxRunning    = "max_running"
	fieldMaxAttempts   = "max_attempts"
	fieldRetryBase     = "retry_base_seconds"
	fieldTimeout       = "timeout_seconds"
	fieldScheduleHours = "schedule_hours"
	fieldPauseAfter    = "pause_after"
	fieldBreaker       = "breaker"
	fieldBreakerSince  = "breaker_since" // when an administrator last resumed the kind: older failures do not count
)

func kindSettingName(kind, field string) string { return "workers." + kind + "." + field }
func optionSettingName(kind, opt string) string { return "workers." + kind + ".opt." + opt }

// kindSettings is the configuration of a kind in force.
type kindSettings struct {
	Enabled       bool
	PausedReason  string // why it is off, when the circuit breaker turned it off
	MaxRunning    int
	MaxAttempts   int
	RetryBase     time.Duration
	Timeout       time.Duration
	ScheduleEvery time.Duration // 0: not scheduled
	PauseAfter    int
	Breaker       int
	BreakerSince  time.Time
	Options       map[string]int
}

// optionStrings is what a worker is given.
func (k kindSettings) optionStrings() map[string]string {
	out := make(map[string]string, len(k.Options))
	for name, v := range k.Options {
		out[name] = strconv.Itoa(v)
	}
	return out
}

// backoff is the wait before attempt number attempts+1: the base, doubled for every attempt so far,
// capped, with a little jitter so many failed jobs do not all come back at the same moment.
func (k kindSettings) backoff(attempts int) time.Duration {
	d := k.RetryBase
	if d < time.Second {
		d = time.Second
	}
	for i := 1; i < attempts && d < maxRetryDelay; i++ {
		d *= 2
	}
	if d > maxRetryDelay {
		d = maxRetryDelay
	}
	spread := int64(d / 5)
	return d + time.Duration(rand.Int64N(2*spread+1)-spread)
}

// settingInt reads a whole number; an unset, invalid or out of range value gives def.
func (s *Backend) settingInt(ctx context.Context, name string, def, lo, hi int) (int, error) {
	v, err := s.settingValue(ctx, name)
	if err != nil || v == "" {
		return def, err
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		log.Warn().Str("setting", name).Str("value", v).Msg("ignoring an invalid setting")
		return def, nil
	}
	return n, nil
}

// loadKindSettings reads the settings of one kind.
func (s *Backend) loadKindSettings(ctx context.Context, k *kindSpec) (kindSettings, error) {
	var ks kindSettings
	var err error
	num := func(field string, def, lo, hi int) int {
		if err != nil {
			return def
		}
		var n int
		n, err = s.settingInt(ctx, kindSettingName(k.Kind, field), def, lo, hi)
		return n
	}
	d := k.Defaults
	ks.MaxRunning = num(fieldMaxRunning, d.MaxRunning, 0, 100)
	ks.MaxAttempts = num(fieldMaxAttempts, d.MaxAttempts, 1, maxAttemptsLimit)
	ks.RetryBase = time.Duration(num(fieldRetryBase, int(d.RetryBase/time.Second), 1, int(maxRetryDelay/time.Second))) * time.Second
	ks.Timeout = time.Duration(num(fieldTimeout, int(d.Timeout/time.Second), 10, maxJobTimeoutSeconds)) * time.Second
	ks.ScheduleEvery = time.Duration(num(fieldScheduleHours, d.ScheduleHours, 0, 24*30)) * time.Hour
	ks.PauseAfter = num(fieldPauseAfter, d.PauseAfter, 0, 100)
	ks.Breaker = num(fieldBreaker, d.Breaker, 0, 1000)
	if err != nil {
		return ks, err
	}
	enabled, err := s.settingValue(ctx, kindSettingName(k.Kind, fieldEnabled))
	if err != nil {
		return ks, err
	}
	ks.Enabled = enabled != "false"
	if ks.PausedReason, err = s.settingValue(ctx, kindSettingName(k.Kind, fieldPausedReason)); err != nil {
		return ks, err
	}
	if v, err := s.settingValue(ctx, kindSettingName(k.Kind, fieldBreakerSince)); err != nil {
		return ks, err
	} else if t, perr := time.Parse(time.RFC3339, v); perr == nil {
		ks.BreakerSince = t
	}
	ks.Options = map[string]int{}
	for _, o := range k.Options {
		if ks.Options[o.Name], err = s.settingInt(ctx, optionSettingName(k.Kind, o.Name), o.Default, o.Min, o.Max); err != nil {
			return ks, err
		}
	}
	return ks, nil
}

// workerSettings is every kind's configuration plus the global values, part of liveSettings.
type workerSettings struct {
	Kinds           map[string]kindSettings
	DefaultPriority int
	KeepHistory     time.Duration
}

func (s *Backend) loadWorkerSettings(ctx context.Context) (workerSettings, error) {
	ws := workerSettings{Kinds: map[string]kindSettings{}}
	for _, kind := range s.kindNames() {
		ks, err := s.loadKindSettings(ctx, s.kinds[kind])
		if err != nil {
			return ws, fmt.Errorf("settings of %s: %w", kind, err)
		}
		ws.Kinds[kind] = ks
	}
	var err error
	if ws.DefaultPriority, err = s.settingInt(ctx, settingWorkersDefaultPriority, 50, 1, 100); err != nil {
		return ws, err
	}
	days, err := s.settingInt(ctx, settingWorkersKeepDays, defaultKeepJobDays, 1, maxSettingDays)
	ws.KeepHistory = time.Duration(days) * 24 * time.Hour
	return ws, err
}
