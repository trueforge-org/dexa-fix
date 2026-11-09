package web

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// tlsView is the Admin page's section for secure connections.
type tlsView struct {
	Mode        string // off, upload or selfsigned
	Names       string // names for a self-signed certificate, as typed
	Active      bool   // a certificate is in force
	Subject     string
	CertNames   string
	NotAfter    string
	Fingerprint string
	HTTP3       bool
	HTTP3Active bool
	Port        string // HTTPS port as saved (TCP; HTTP/3 uses the same number on UDP)
	ListenError string // why HTTPS could not start, empty when all is well
}

func (s *Server) adminTLSRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/tls", s.admin(s.adminSaveTLS))
}

func (s *Server) tlsViewFor(r *http.Request) (tlsView, error) {
	ctx := r.Context()
	l := s.cfg()
	v := tlsView{Mode: l.tlsMode, Active: l.tlsCert != nil, HTTP3Active: l.http3, Port: l.httpsPort, ListenError: s.listenError()}
	var err error
	if v.Names, err = s.settingValue(ctx, settingTLSNames); err != nil {
		return v, err
	}
	h3, err := s.settingValue(ctx, settingHTTP3)
	v.HTTP3 = h3 == valueEnabled
	if l.tlsInfo != nil {
		v.Subject, v.CertNames = l.tlsInfo.Subject, strings.Join(l.tlsInfo.Names, ", ")
		v.NotAfter, v.Fingerprint = l.tlsInfo.NotAfter.UTC().Format(dateLayout), l.tlsInfo.Fingerprint
	}
	return v, err
}

// validName accepts a DNS name or an IP address for a self-signed certificate.
func validName(n string) bool {
	if n == "" || len(n) > 253 || strings.ContainsAny(n, " /\\@?#\t\r\n,") {
		return false
	}
	return true
}

func (s *Server) adminSaveTLS(w http.ResponseWriter, r *http.Request, _ models.User) {
	ctx := r.Context()
	mode := r.PostFormValue("mode")
	switch mode {
	case tlsOff, tlsUpload, tlsSelfSigned:
	default:
		adminRedirect(w, r, "Choose off, an uploaded certificate or a self-signed certificate.")
		return
	}
	port := strings.TrimSpace(r.PostFormValue("port"))
	if port == "" {
		port = defaultHTTPSPort
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		adminRedirect(w, r, "The HTTPS port is a number from 1 to 65535.")
		return
	}
	if _, plain, err := net.SplitHostPort(s.listenAddr); err == nil && plain == port {
		adminRedirect(w, r, "The HTTPS port must differ from the port of plain HTTP ("+plain+"): they are separate listeners.")
		return
	}
	http3 := r.PostFormValue("http3") != ""
	if http3 && mode == tlsOff {
		adminRedirect(w, r, "HTTP/3 needs TLS: choose a certificate first.")
		return
	}
	prev, err := s.settingValue(ctx, settingTLSMode)
	if err != nil {
		s.fail(w, err)
		return
	}
	switch mode {
	case tlsUpload:
		cert, key := strings.TrimSpace(r.PostFormValue("cert")), strings.TrimSpace(r.PostFormValue("key"))
		if cert == "" && key == "" && prev == tlsUpload {
			break // keep the stored one
		}
		if _, _, err := parseCertificate(cert, key); err != nil {
			adminRedirect(w, r, err.Error())
			return
		}
		if err := s.store.PutSetting(ctx, settingTLSCert, cert); err != nil {
			s.fail(w, err)
			return
		}
		if err := s.store.PutSetting(ctx, settingTLSKey, key); err != nil {
			s.fail(w, err)
			return
		}
	case tlsSelfSigned:
		names := strings.TrimSpace(r.PostFormValue("names"))
		for _, n := range strings.Split(names, ",") {
			if n = strings.TrimSpace(n); names != "" && !validName(n) {
				adminRedirect(w, r, "A name for the certificate is a host name or an IP address, without spaces.")
				return
			}
		}
		if prev != tlsSelfSigned { // never reuse an uploaded certificate as a self-signed one
			for _, name := range []string{settingTLSCert, settingTLSKey} {
				if err := s.store.DeleteSetting(ctx, name); err != nil {
					s.fail(w, err)
					return
				}
			}
		}
		if err := s.putOrClear(ctx, settingTLSNames, names); err != nil {
			s.fail(w, err)
			return
		}
	}
	h3 := valueDisabled
	if http3 {
		h3 = valueEnabled
	}
	if err := s.store.PutSetting(ctx, settingTLSMode, mode); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.putOrClear(ctx, settingHTTPS, strings.TrimSpace(r.PostFormValue("port"))); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.PutSetting(ctx, settingHTTP3, h3); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.reloadSettings(ctx); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "tls="+mode)
	switch {
	case mode != tlsOff && s.cfg().tlsCert == nil:
		adminRedirect(w, r, "The certificate could not be used, so TLS stays off. Check the server log.")
	case mode == tlsOff:
		adminRedirect(w, r, "TLS is off. The server accepts plain HTTP only.")
	case s.listenError() != "":
		adminRedirect(w, r, s.listenError())
	default:
		msg := "HTTPS is on, on port " + s.cfg().httpsPort + " (HTTP/2 and HTTP/1.1). Plain HTTP stays on its own port."
		if http3 {
			msg += " HTTP/3 listens on UDP port " + s.cfg().httpsPort + "; make sure it is open too."
		}
		adminRedirect(w, r, msg)
	}
}
