package frontend

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// The workers section of the Admin page: the workers that are running, the settings of each kind
// of job, the queue, and each user's priority. The frontend parses the forms and renders; the
// backend (internal_admin_workers.go) validates and changes.

func (s *Frontend) adminWorkerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/workers", s.admin(s.adminWorkersHome))
	mux.HandleFunc("POST /admin/workers/kinds/{kind}", s.admin(s.adminSaveWorkerKind))
	mux.HandleFunc("POST /admin/workers/kinds/{kind}/resume", s.admin(s.adminResumeWorkerKind))
	mux.HandleFunc("POST /admin/workers/global", s.admin(s.adminSaveWorkersGlobal))
	mux.HandleFunc("POST /admin/workers/jobs/{id}/cancel", s.admin(s.adminCancelJob))
	mux.HandleFunc("POST /admin/workers/schedules/{user}/{kind}/resume", s.admin(s.adminResumeSchedule))
	mux.HandleFunc("POST /admin/users/{id}/priority", s.admin(s.adminSetPriority))
}

func (s *Frontend) adminWorkersHome(w http.ResponseWriter, r *http.Request, u models.User) {
	page, err := s.api.AdminWorkers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "adminworkers.html", http.StatusOK, pageData{Title: "Workers · DexaFix", Tab: "admin", User: &u, Msg: flashParam(r), Workers: page})
}

// workersDone shows the backend's message on the workers page.
func (s *Frontend) workersDone(w http.ResponseWriter, r *http.Request, res apiclient.AdminResult, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	msg := res.Message
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	http.Redirect(w, r, "/admin/workers?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// wholeNumber reads a form value that must be a whole number; an empty or invalid one is reported.
func wholeNumber(r *http.Request, name string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue(name)))
	return n, err == nil
}

func (s *Frontend) adminSaveWorkerKind(w http.ResponseWriter, r *http.Request, _ models.User) {
	f := apiclient.WorkerKindForm{Enabled: r.PostFormValue("enabled") == "1", Options: map[string]int{}}
	for name, dst := range map[string]*int{
		"max_running": &f.MaxRunning, "max_attempts": &f.MaxAttempts, "retry_base_seconds": &f.RetryBaseSeconds,
		"timeout_seconds": &f.TimeoutSeconds, "schedule_hours": &f.ScheduleHours, "pause_after": &f.PauseAfter, "breaker": &f.Breaker,
	} {
		n, ok := wholeNumber(r, name)
		if !ok {
			s.workersDone(w, r, apiclient.AdminResult{Message: "Every setting must be a whole number."}, nil)
			return
		}
		*dst = n
	}
	for key := range r.PostForm {
		if opt, ok := strings.CutPrefix(key, "opt_"); ok {
			n, good := wholeNumber(r, key)
			if !good {
				s.workersDone(w, r, apiclient.AdminResult{Message: "Every setting must be a whole number."}, nil)
				return
			}
			f.Options[opt] = n
		}
	}
	res, err := s.api.AdminSaveWorkerKind(r.Context(), r.PathValue("kind"), f)
	s.workersDone(w, r, res, err)
}

func (s *Frontend) adminResumeWorkerKind(w http.ResponseWriter, r *http.Request, _ models.User) {
	res, err := s.api.AdminResumeWorkerKind(r.Context(), r.PathValue("kind"))
	s.workersDone(w, r, res, err)
}

func (s *Frontend) adminSaveWorkersGlobal(w http.ResponseWriter, r *http.Request, _ models.User) {
	p, ok1 := wholeNumber(r, "default_priority")
	k, ok2 := wholeNumber(r, "keep_history_days")
	if !ok1 || !ok2 {
		s.workersDone(w, r, apiclient.AdminResult{Message: "Every setting must be a whole number."}, nil)
		return
	}
	res, err := s.api.AdminSaveWorkersGlobal(r.Context(), apiclient.WorkersGlobalForm{DefaultPriority: p, KeepHistoryDays: k})
	s.workersDone(w, r, res, err)
}

func (s *Frontend) adminCancelJob(w http.ResponseWriter, r *http.Request, _ models.User) {
	res, err := s.api.AdminCancelJob(r.Context(), r.PathValue("id"))
	s.workersDone(w, r, res, err)
}

func (s *Frontend) adminResumeSchedule(w http.ResponseWriter, r *http.Request, _ models.User) {
	id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	res, err := s.api.AdminResumeSchedule(r.Context(), id, r.PathValue("kind"))
	s.workersDone(w, r, res, err)
}

// adminSetPriority is on the users table of the main Admin page, so it answers there.
func (s *Frontend) adminSetPriority(w http.ResponseWriter, r *http.Request, _ models.User) {
	id, ok := targetID(w, r)
	if !ok {
		return
	}
	n, good := wholeNumber(r, "priority")
	if !good {
		adminRedirect(w, r, "The priority must be a whole number from 1 to 100.")
		return
	}
	res, err := s.api.AdminSetJobPriority(r.Context(), id, n)
	s.adminDone(w, r, res, err)
}
