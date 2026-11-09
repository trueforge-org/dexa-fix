package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// Passkeys (WebAuthn) are a way to sign in without a password. They replace the password rather
// than add to it: a passkey needs the device and its unlock (a fingerprint, a face or a PIN, which
// we require), so it is two factors in one and is never asked for a second step. They are not a
// second step after a password.
//
// The relying party id is the host name the browser used (the base URL when one is set), so
// passkeys belong to that name: changing it, for example moving to another domain, orphans them.
// Browsers only offer passkeys on HTTPS or on localhost.
//
// The ceremony itself runs in the backend (internal_passkey.go). This side forwards what the
// browser script sends and relays the sealed ceremony cookie the backend sets.
const (
	passkeyCookie       = "dexafix_webauthn"
	sealPasskeyRegister = "webauthn-register"
	sealPasskeyLogin    = "webauthn-login"
	passkeyTTL          = 5 * time.Minute
	maxPasskeyName      = 64
	defaultPasskeyName  = "Passkey"
)

func (s *Server) passkeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /passkey/login/begin", s.passkeyLoginBegin)
	mux.HandleFunc("POST /passkey/login/finish", s.passkeyLoginFinish)
	mux.HandleFunc("POST /u/{id}/passkeys/begin", s.user(s.passkeyRegisterBegin))
	mux.HandleFunc("POST /u/{id}/passkeys/finish", s.user(s.passkeyRegisterFinish))
	mux.HandleFunc("POST /u/{id}/passkeys/{cred}/rename", s.user(s.passkeyRename))
	mux.HandleFunc("POST /u/{id}/passkeys/{cred}/delete", s.user(s.passkeyDelete))
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// passkeyFail answers the browser script with the backend's refusal, in the {"error": ...} shape
// it shows. A failure of the backend itself is a plain server error.
func (s *Server) passkeyFail(w http.ResponseWriter, err error) {
	var e *apiclient.Error
	if errors.As(err, &e) {
		jsonError(w, e.Status, e.Message)
		return
	}
	jsonError(w, http.StatusInternalServerError, "Something went wrong.")
}

// passkeyOptions answers with the WebAuthn options the backend made.
func (s *Server) passkeyOptions(w http.ResponseWriter, opts json.RawMessage, err error) {
	if err != nil {
		s.passkeyFail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(opts)
}

// passkeyRedirect answers the browser script with where to go next.
func (s *Server) passkeyRedirect(w http.ResponseWriter, o apiclient.SignInOutcome, err error) {
	if err != nil {
		s.passkeyFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": o.Redirect})
}

// ---- registering a passkey (signed in) ----

func (s *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request, u models.User) {
	opts, err := s.api.PasskeyRegisterBegin(r.Context(), u.ID, r.PostFormValue("name"), r.PostFormValue("current"))
	s.passkeyOptions(w, opts, err)
}

func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request, u models.User) {
	o, err := s.api.PasskeyRegisterFinish(r.Context(), u.ID, http.MaxBytesReader(w, r.Body, maxBodyBytes))
	s.passkeyRedirect(w, o, err)
}

// ---- managing passkeys ----

func (s *Server) passkeyRename(w http.ResponseWriter, r *http.Request, u models.User) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	err := s.api.RenamePasskey(r.Context(), u.ID, r.PathValue("cred"), name)
	switch {
	case apiclient.IsCode(err, apiclient.CodeInvalid):
		var e *apiclient.Error
		errors.As(err, &e)
		settingsRedirect(w, r, u.ID, e.Message)
	case err != nil:
		s.fail(w, err)
	default:
		settingsRedirect(w, r, u.ID, "Passkey renamed.")
	}
}

func (s *Server) passkeyDelete(w http.ResponseWriter, r *http.Request, u models.User) {
	msg, err := s.api.DeletePasskey(r.Context(), u.ID, r.PathValue("cred"), r.PostFormValue("current"))
	if err != nil {
		s.settingsError(w, r, u, err)
		return
	}
	settingsRedirect(w, r, u.ID, msg)
}

// ---- signing in with a passkey ----

func (s *Server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	opts, err := s.api.PasskeyLoginBegin(r.Context())
	s.passkeyOptions(w, opts, err)
}

func (s *Server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	o, err := s.api.PasskeyLoginFinish(r.Context(), http.MaxBytesReader(w, r.Body, maxBodyBytes))
	s.passkeyRedirect(w, o, err)
}
