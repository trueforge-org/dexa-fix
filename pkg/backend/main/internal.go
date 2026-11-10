package mainbackend

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexafix/pkg/shared/edge"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// The internal API, /internal/v1, is what the frontend calls. It is private to the deployment: only
// a caller that knows the service secret gets in, and it is not a supported interface for anything
// else (the public one is /api/v1).
//
// A request arrives with the visitor's cookies, address, host and scheme in X-Dexafix-* headers.
// After the service secret checked out, they are put back where the rest of the code looks for
// them, so session lookup, sealed state cookies, audit addresses and absolute URLs work exactly as
// they do for a browser talking to the server directly. Answers carry Set-Cookie headers like any
// handler, and the frontend relays them.
//
// The internal API ignores the Authorization header: personal access tokens and Basic credentials
// only work on /api/, as always.

// SetServiceSecrets sets the secrets the internal API accepts. The first is the one the frontend
// sends; the others are accepted so that a secret can be rotated without downtime.
func (s *Backend) SetServiceSecrets(secrets ...string) {
	var hashes [][32]byte
	for _, sec := range secrets {
		if sec = strings.TrimSpace(sec); sec != "" {
			hashes = append(hashes, sha256.Sum256([]byte(sec)))
		}
	}
	s.serviceSecrets = hashes
}

// InternalHandler returns the handler of the internal API.
func (s *Backend) InternalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+apiclient.Prefix+"/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET "+apiclient.Prefix+"/session", s.internalSession)
	mux.HandleFunc("GET "+apiclient.Prefix+"/frontend-config", s.internalFrontendConfig)
	s.internalRoutes(mux)
	mux.HandleFunc(apiclient.Prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
	})
	var h http.Handler = s.withAuth(mux)
	if s.internalBudget != nil { // a separate backend: shed floods before any work is done
		h = s.internalBudget.Middleware(h)
	}
	pages := s.serviceAuth(s.visitorContext(h))
	workers := s.workerHandler()
	// Two doors with two secrets: the workers' endpoints take the worker secret only, everything else
	// the frontend's service secret only.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, workerapi.Prefix+"/") {
			workers.ServeHTTP(w, r)
			return
		}
		pages.ServeHTTP(w, r)
	})
}

// serviceAuth lets in only callers that present a service secret.
func (s *Backend) serviceAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.validServiceSecret(r.Header.Get(apiclient.HeaderService)) {
			internalFail(w, http.StatusUnauthorized, apiclient.CodeUnauthorized, "service authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Backend) validServiceSecret(presented string) bool {
	return matchSecret(s.serviceSecrets, presented)
}

// visitorContext puts the visitor's request back into r: cookies, address, host and scheme.
func (s *Backend) visitorContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.Header.Del("Cookie")
		if c := r.Header.Get(apiclient.HeaderCookie); c != "" {
			r2.Header.Set("Cookie", c)
		}
		r2.Header.Del(apiclient.HeaderCookie)
		if ip, err := netip.ParseAddr(r.Header.Get(apiclient.HeaderClientIP)); err == nil {
			r2.RemoteAddr = net.JoinHostPort(ip.Unmap().String(), "0")
		}
		if h := r.Header.Get(apiclient.HeaderHost); edge.ValidHost(h) {
			r2.Host = h
		}
		ctx := edge.WithSecure(r2.Context(), r.Header.Get(apiclient.HeaderSecure) == "1")
		if base := s.cfg().baseURL; base != nil {
			ctx = edge.WithBaseURL(ctx, base)
		}
		next.ServeHTTP(w, r2.WithContext(ctx))
	})
}

// ---- answering ----

func internalJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}

// internalFail answers with an error the frontend shows as it is: message is fit for the person.
func internalFail(w http.ResponseWriter, status int, code, message string) {
	internalJSON(w, status, apiclient.Error{Status: status, Code: code, Message: message})
}

// internalError maps an error from the store or an importer to an answer.
func internalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
	case errors.Is(err, store.ErrExists):
		internalFail(w, http.StatusConflict, apiclient.CodeExists, err.Error())
	case errors.Is(err, store.ErrInvalid):
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, err.Error())
	case errors.Is(err, bodyapi.ErrUnavailable):
		log.Warn().Err(err).Msg("the body service is unavailable")
		internalFail(w, http.StatusServiceUnavailable, apiclient.CodeUnavailable, "the readings and scans are unavailable, try again shortly")
	default:
		log.Error().Err(err).Msg("internal request failed")
		internalFail(w, http.StatusInternalServerError, apiclient.CodeInternal, "internal error")
	}
}

func internalDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(v); err != nil {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid request: "+err.Error())
		return false
	}
	return true
}

// internalUser allows a request to act only on the signed-in user's own data, like apiUser.
func (s *Backend) internalUser(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil {
			internalFail(w, http.StatusUnauthorized, apiclient.CodeUnauthorized, "sign in first")
			return
		}
		if id := r.PathValue("id"); id != "" && id != strconv.FormatInt(u.ID, 10) {
			internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
			return
		}
		h(w, r, *u)
	}
}

func (s *Backend) internalSession(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		internalFail(w, http.StatusUnauthorized, apiclient.CodeUnauthorized, "not signed in")
		return
	}
	internalJSON(w, http.StatusOK, apiclient.NewSessionUser(*u))
}

// internalRoutes registers the endpoints of the pages, one group per file.
func (s *Backend) internalRoutes(mux *http.ServeMux) {
	s.internalMeasureRoutes(mux)
	s.internalSignInRoutes(mux)
	s.internalSettingsRoutes(mux)
	s.internalSecurityRoutes(mux)
	s.internalTwoFactorRoutes(mux)
	s.internalPasskeyRoutes(mux)
	s.internalAdminRoutes(mux)
	s.internalIntegrationRoutes(mux)
	s.internalAdminWorkerRoutes(mux)
	s.internalStatusRoutes(mux)
}

// ---- the frontend side ----
