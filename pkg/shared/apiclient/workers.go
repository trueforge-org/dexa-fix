package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// The Admin page's view of the workers: the settings of each kind of job, the workers that are
// running, and the queue. Like the rest of the Admin calls they need an administrator who passed
// the step-up check. Nothing here names a provider: the backend lists whatever it has.

// WorkerOptionView is a whole-number option of a kind that is sent to its workers.
type WorkerOptionView struct {
	Name     string
	Label    string
	Value    int
	Min, Max int
	Millis   bool // shown in milliseconds
}

// WorkerKindView is one kind of job: its settings and what is going on.
type WorkerKindView struct {
	Kind  string
	Label string
	// Enabled is false when an administrator (or the circuit breaker, see PausedReason) paused it.
	Enabled      bool
	PausedReason string

	MaxRunning       int // at once in all workers together; 0 is no limit
	MaxAttempts      int
	RetryBaseSeconds int
	TimeoutSeconds   int
	ScheduleHours    int // 0: no automatic runs
	PauseAfter       int
	Breaker          int
	Schedulable      bool
	Options          []WorkerOptionView

	Queued          int
	Running         int
	Failed24h       int
	WorkersOnline   int
	PausedSchedules int // users whose automatic runs are paused
}

// WorkerProviderView groups the kinds of one provider.
type WorkerProviderView struct {
	Name  string
	Label string
	Kinds []WorkerKindView
}

// WorkerInstanceView is a worker process that asked for work recently.
type WorkerInstanceView struct {
	ID       string
	Version  string
	Kinds    []string
	LastSeen string
}

// WorkerJobView is a job in the list on the Admin page.
type WorkerJobView struct {
	ID          string
	Kind        string
	User        string
	UserID      int64
	Priority    int
	UserIsAdmin bool
	State       string
	Origin      string
	Attempts    int
	MaxAttempts int
	Error       string
	Created     string
	Updated     string
	Active      bool // can still be canceled
}

// WorkersPage is everything the workers section of the Admin page shows.
type WorkersPage struct {
	Providers       []WorkerProviderView
	Workers         []WorkerInstanceView
	Jobs            []WorkerJobView
	DefaultPriority int
	KeepHistoryDays int
}

// WorkerKindForm is what an administrator saves for a kind.
type WorkerKindForm struct {
	Enabled          bool
	MaxRunning       int
	MaxAttempts      int
	RetryBaseSeconds int
	TimeoutSeconds   int
	ScheduleHours    int
	PauseAfter       int
	Breaker          int
	Options          map[string]int
}

// WorkersGlobalForm is what an administrator saves for all workers.
type WorkersGlobalForm struct {
	DefaultPriority int
	KeepHistoryDays int
}

func (c *Client) AdminWorkers(ctx context.Context) (*WorkersPage, error) {
	var p WorkersPage
	if err := c.Get(ctx, "/admin/workers", &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Client) AdminSaveWorkerKind(ctx context.Context, kind string, f WorkerKindForm) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/workers/kinds/"+url.PathEscape(kind), f)
}

// AdminResumeWorkerKind turns a paused kind back on, and starts counting failures afresh.
func (c *Client) AdminResumeWorkerKind(ctx context.Context, kind string) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/workers/kinds/"+url.PathEscape(kind)+"/resume", nil)
}

func (c *Client) AdminSaveWorkersGlobal(ctx context.Context, f WorkersGlobalForm) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/workers/global", f)
}

func (c *Client) AdminCancelJob(ctx context.Context, id string) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/workers/jobs/"+url.PathEscape(id)+"/cancel", nil)
}

// AdminResumeSchedule lets the scheduler run a kind for a user again after it paused their runs.
func (c *Client) AdminResumeSchedule(ctx context.Context, userID int64, kind string) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/workers/schedules/"+strconv.FormatInt(userID, 10)+"/"+url.PathEscape(kind)+"/resume", nil)
}

// AdminSetJobPriority sets a user's job priority, 1 to 100.
func (c *Client) AdminSetJobPriority(ctx context.Context, userID int64, priority int) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, adminUserPath(userID, "/priority"), map[string]int{"priority": priority})
}
