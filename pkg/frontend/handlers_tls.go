package frontend

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
)

// tlsView is the Admin page's section for secure connections: the backend's view plus whether this
// process's listener could start.
type tlsView struct {
	apiclient.TLSView
	ListenError string // why HTTPS could not start, empty when all is well
}

func (s *Frontend) adminTLSRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/tls", s.admin(s.adminSaveTLS))
}

func (s *Frontend) adminSaveTLS(w http.ResponseWriter, r *http.Request, _ models.User) {
	// The frontend owns the listeners, so it checks the port conflict itself.
	port := strings.TrimSpace(r.PostFormValue("port"))
	if port == "" {
		port = defaultHTTPSPort
	}
	if n, err := strconv.Atoi(port); err == nil && n >= 1 && n <= 65535 {
		if _, plain, err := net.SplitHostPort(s.listenAddr); err == nil && plain == port {
			adminRedirect(w, r, "The HTTPS port must differ from the port of plain HTTP ("+plain+"): they are separate listeners.")
			return
		}
	}
	res, err := s.api.AdminSaveTLS(r.Context(), apiclient.TLSForm{
		Mode: r.PostFormValue("mode"), Port: r.PostFormValue("port"), Cert: r.PostFormValue("cert"),
		Key: r.PostFormValue("key"), Names: r.PostFormValue("names"), HTTP3: r.PostFormValue("http3") != "",
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.afterAdminChange(r, res)
	if res.ListenCheck {
		if msg := s.listenError(); msg != "" {
			res.Message = msg
		}
	}
	adminRedirect(w, r, res.Message)
}
