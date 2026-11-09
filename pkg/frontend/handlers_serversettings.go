package frontend

import (
	"net/http"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
)

// The server, e-mail and test e-mail forms of the Admin page. The backend validates and stores.

func (s *Frontend) adminSettingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/server", s.admin(s.adminSaveServerSettings))
	mux.HandleFunc("POST /admin/smtp", s.admin(s.adminSaveSMTP))
	mux.HandleFunc("POST /admin/smtp/test", s.admin(s.adminTestSMTP))
}

func (s *Frontend) adminSaveServerSettings(w http.ResponseWriter, r *http.Request, _ models.User) {
	res, err := s.api.AdminSaveServer(r.Context(), apiclient.ServerForm{
		BaseURL: r.PostFormValue("base_url"), TrustedProxies: r.PostFormValue("trusted_proxies"),
		ProxyProtocol: r.PostFormValue("proxy_protocol") != "",
		AuditDays:     r.PostFormValue("audit_retention_days"), KeyReminderDays: r.PostFormValue("key_reminder_days"),
	})
	s.adminDone(w, r, res, err)
}

func (s *Frontend) adminSaveSMTP(w http.ResponseWriter, r *http.Request, _ models.User) {
	res, err := s.api.AdminSaveSMTP(r.Context(), apiclient.SMTPForm{
		Host: r.PostFormValue("host"), Port: r.PostFormValue("port"), Username: r.PostFormValue("username"),
		Password: r.PostFormValue("password"), From: r.PostFormValue("from"), TLS: r.PostFormValue("tls"),
		RemovePassword: r.PostFormValue("remove_password") != "",
	})
	s.adminDone(w, r, res, err)
}

func (s *Frontend) adminTestSMTP(w http.ResponseWriter, r *http.Request, _ models.User) {
	res, err := s.api.AdminTestSMTP(r.Context())
	s.adminDone(w, r, res, err)
}
