package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

const (
	sessionCookie = "dexafix_session"
	sessionTTL    = 30 * 24 * time.Hour

	minPasswordLen   = 8
	maxPasswordBytes = 72 // bcrypt's limit

	tokenPrefix       = "dxf_"
	tokenPrefixShown  = 12
	settingCookieKey  = "auth.cookie_key"
	settingPasswordOn = "auth.password_login"
	settingSignup     = "auth.signup"
	valueDisabled     = "disabled"
)

// authenticator resolves a request to a user. It returns ok=false when the request does not carry
// this kind of credential or the credential is not valid.
//
// Every way of proving who someone is lives in this chain, so another one, such as a header set by
// a trusted reverse proxy, is one more entry in Server.authChain.
type authenticator func(r *http.Request) (u models.User, ok bool, err error)

type userKey struct{}

// currentUser returns the authenticated user of the request, or nil.
func currentUser(r *http.Request) *models.User {
	u, _ := r.Context().Value(userKey{}).(*models.User)
	return u
}

// withAuth runs the authenticator chain and stores the user in the request context.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		for _, a := range s.authChain {
			u, ok, err := a(r)
			if err != nil {
				log.Error().Err(err).Msg("authentication failed")
				continue
			}
			if ok {
				r = r.WithContext(context.WithValue(r.Context(), userKey{}, &u))
				break
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authSession(r *http.Request) (models.User, bool, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return models.User{}, false, nil
	}
	return foundUser(s.store.UserBySession(r.Context(), hashToken(c.Value)))
}

// authBearer accepts personal access tokens and ID tokens issued by a configured provider. Bearer
// and Basic credentials only work on the API, so a leaked token cannot change a password or mint
// new tokens through the web forms.
func (s *Server) authBearer(r *http.Request) (models.User, bool, error) {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || !strings.HasPrefix(r.URL.Path, "/api/") {
		return models.User{}, false, nil
	}
	if strings.HasPrefix(token, tokenPrefix) {
		return foundUser(s.store.UserByTokenHash(r.Context(), hashToken(token)))
	}
	return s.authProviderToken(r.Context(), token)
}

func (s *Server) authBasic(r *http.Request) (models.User, bool, error) {
	name, password, found := r.BasicAuth()
	if !found || !strings.HasPrefix(r.URL.Path, "/api/") || !s.passwordEnabled(r.Context()) {
		return models.User{}, false, nil
	}
	return s.verifyPassword(r.Context(), name, password)
}

// authProviderToken verifies a JWT issued by a configured provider. The issuer picks the provider,
// and the token must be signed by it and issued for this application, so a token obtained by some
// other application cannot be replayed here. Opaque access tokens are refused for that reason:
// nothing in them says which application they were issued to.
func (s *Server) authProviderToken(ctx context.Context, raw string) (models.User, bool, error) {
	iss := unverifiedIssuer(raw)
	if iss == "" {
		return models.User{}, false, nil
	}
	providers, err := s.store.ListAuthProviders(ctx)
	if err != nil {
		return models.User{}, false, err
	}
	for _, p := range providers {
		if p.Kind == models.ProviderKindOAuth2 || p.Issuer != iss {
			continue
		}
		prov, err := s.oidcProvider(ctx, p)
		if err != nil {
			return models.User{}, false, err
		}
		idt, err := prov.Verifier(&oidc.Config{ClientID: p.ClientID}).Verify(s.oidcContext(ctx), raw)
		if err != nil {
			continue // another provider may share the issuer with a different client id
		}
		id, err := s.store.GetIdentity(ctx, p.Slug, idt.Subject)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return models.User{}, false, err
		}
		return foundUser(s.store.GetUser(ctx, id.UserID))
	}
	return models.User{}, false, nil
}

// unverifiedIssuer reads the iss claim of a JWT. It only selects a provider; the signature is
// checked afterwards.
func unverifiedIssuer(raw string) string {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return ""
	}
	return c.Iss
}

func foundUser(u models.User, err error) (models.User, bool, error) {
	if errors.Is(err, store.ErrNotFound) {
		return models.User{}, false, nil
	}
	return u, err == nil, err
}

// ---- passwords ----

var dummyHash = sync.OnceValue(func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("dexa-fix-dummy"), bcrypt.DefaultCost)
	return h
})

func hashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(h), err
}

// validatePassword returns a message for the user, or "" if the password is acceptable.
func validatePassword(password, confirm string) string {
	switch {
	case len(password) < minPasswordLen:
		return "Passwords need at least " + strconv.Itoa(minPasswordLen) + " characters."
	case len(password) > maxPasswordBytes:
		return "Passwords can be at most " + strconv.Itoa(maxPasswordBytes) + " bytes long."
	case password != confirm:
		return "The two passwords do not match."
	}
	return ""
}

func localSubject(id int64) string { return strconv.FormatInt(id, 10) }

// verifyPassword checks a name and password. Unknown names cost the same time as wrong passwords,
// so response times do not reveal which accounts exist.
func (s *Server) verifyPassword(ctx context.Context, name, password string) (models.User, bool, error) {
	fail := func() (models.User, bool, error) {
		bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
		return models.User{}, false, nil
	}
	u, err := s.store.GetUserByName(ctx, strings.TrimSpace(name))
	if errors.Is(err, store.ErrNotFound) {
		return fail()
	}
	if err != nil {
		return models.User{}, false, err
	}
	id, err := s.store.GetIdentity(ctx, models.ProviderLocal, localSubject(u.ID))
	if errors.Is(err, store.ErrNotFound) {
		return fail()
	}
	if err != nil {
		return models.User{}, false, err
	}
	if bcrypt.CompareHashAndPassword([]byte(id.Secret), []byte(password)) != nil {
		return models.User{}, false, nil
	}
	return u, true, nil
}

func (s *Server) setPassword(ctx context.Context, userID int64, password string) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	return s.store.PutIdentity(ctx, models.Identity{UserID: userID, Provider: models.ProviderLocal, Subject: localSubject(userID), Secret: hash})
}

// passwordEnabled reports whether username and password sign-in is allowed.
func (s *Server) passwordEnabled(ctx context.Context) bool {
	if s.ForcePasswordLogin {
		return true
	}
	v, err := s.store.GetSetting(ctx, settingPasswordOn)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Error().Err(err).Msg("read password login setting")
	}
	return v != valueDisabled
}

// signupOpen reports whether new accounts can be created by people signing up. The first account
// can always be created, or a new server could never get an administrator.
func (s *Server) signupOpen(ctx context.Context) (bool, error) {
	v, err := s.settingOrEmpty(ctx, settingSignup)
	if err != nil || v != valueDisabled {
		return true, err
	}
	users, err := s.store.ListUsers(ctx)
	return len(users) == 0, err
}

// ---- tokens and cookies ----

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashToken hashes session ids and access tokens. They are long random values, so a fast hash is
// enough: unlike passwords they cannot be guessed from a dictionary.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Server) loadCookieKey(ctx context.Context) ([]byte, error) {
	v, err := s.store.GetSetting(ctx, settingCookieKey)
	if errors.Is(err, store.ErrNotFound) {
		if v, err = randomHex(32); err == nil {
			err = s.store.PutSetting(ctx, settingCookieKey, v)
		}
	}
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(v)
}

func isSecure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// externalURL builds an absolute URL for this server as seen by the browser.
func externalURL(r *http.Request, path string) string {
	scheme := "http"
	if isSecure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

// startSession signs the user in, whichever way they proved who they are.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64, method string) error {
	token, err := randomHex(32)
	if err != nil {
		return err
	}
	expires := time.Now().Add(sessionTTL)
	if err := s.store.CreateSession(r.Context(), hashToken(token), userID, method, expires); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires, MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r),
	})
	return nil
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r)})
}

// ---- request guards ----

// user allows a request to act only on the signed-in user's own pages. Other users' pages answer
// 404 so that they do not reveal which ids exist.
func (s *Server) user(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id != u.ID {
			http.NotFound(w, r)
			return
		}
		h(w, r, *u)
	}
}

// ---- sign-in page ----

type loginProvider struct{ Slug, Label string }

type loginView struct {
	PasswordEnabled bool
	SignupOpen      bool
	FirstUser       bool
	ClaimName       string // existing administrator that can set a first password
	Providers       []loginProvider
}

func (s *Server) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /signup", s.signup)
	mux.HandleFunc("POST /claim", s.claim)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /auth/{provider}/login", s.providerStart)
	mux.HandleFunc("GET /auth/{provider}/callback", s.providerCallback)
	mux.HandleFunc("POST /auth/{provider}/callback", s.providerCallback)
	mux.HandleFunc("POST /u/{id}/profile", s.user(s.saveProfile))
	mux.HandleFunc("POST /u/{id}/password", s.user(s.changePassword))
	mux.HandleFunc("POST /u/{id}/tokens", s.user(s.createToken))
	mux.HandleFunc("POST /u/{id}/tokens/{item}/delete", s.user(s.deleteToken))
	mux.HandleFunc("POST /u/{id}/identities/{provider}/delete", s.user(s.unlinkIdentity))
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if u := currentUser(r); u != nil {
		http.Redirect(w, r, userPath(u.ID, ""), http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, "", http.StatusOK)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string, status int) {
	ctx := r.Context()
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	providers, err := s.store.ListAuthProviders(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	view := &loginView{PasswordEnabled: s.passwordEnabled(ctx), FirstUser: len(users) == 0}
	for _, p := range providers {
		view.Providers = append(view.Providers, loginProvider{p.Slug, p.Label})
	}
	if view.SignupOpen, err = s.signupOpen(ctx); err != nil {
		s.fail(w, err)
		return
	}
	if view.PasswordEnabled {
		if claim, err := s.claimableAdmin(ctx, users); err != nil {
			s.fail(w, err)
			return
		} else if claim != nil {
			view.ClaimName = claim.Name
		}
	}
	s.render(w, "index.html", status, pageData{Title: "Sign in · dexa-fix", Error: errMsg, Login: view})
}

// claimableAdmin returns the administrator of a database from before accounts existed. It stops
// being claimable as soon as any administrator can sign in.
func (s *Server) claimableAdmin(ctx context.Context, users []models.User) (*models.User, error) {
	ids, err := s.store.IdentityProviders(ctx)
	if err != nil {
		return nil, err
	}
	var first *models.User
	for i := range users {
		if !users[i].IsAdmin() {
			continue
		}
		if len(ids[users[i].ID]) > 0 {
			return nil, nil
		}
		if first == nil || users[i].ID < first.ID {
			first = &users[i]
		}
	}
	return first, nil
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.passwordEnabled(r.Context()) {
		s.renderLogin(w, r, "Signing in with a password is turned off.", http.StatusForbidden)
		return
	}
	u, ok, err := s.verifyPassword(r.Context(), r.PostFormValue("name"), r.PostFormValue("password"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		s.renderLogin(w, r, "Wrong name or password.", http.StatusUnauthorized)
		return
	}
	s.finishSignIn(w, r, u, models.ProviderLocal)
}

func (s *Server) finishSignIn(w http.ResponseWriter, r *http.Request, u models.User, method string) {
	if err := s.startSession(w, r, u.ID, method); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, userPath(u.ID, ""), http.StatusSeeOther)
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	if !s.passwordEnabled(r.Context()) {
		s.renderLogin(w, r, "Creating an account with a password is turned off.", http.StatusForbidden)
		return
	}
	if open, err := s.signupOpen(r.Context()); err != nil {
		s.fail(w, err)
		return
	} else if !open {
		s.renderLogin(w, r, "New accounts are closed. Ask your administrator for one.", http.StatusForbidden)
		return
	}
	password := r.PostFormValue("password")
	if msg := validatePassword(password, r.PostFormValue("confirm")); msg != "" {
		s.renderLogin(w, r, msg, http.StatusUnprocessableEntity)
		return
	}
	hash, err := hashPassword(password)
	if err != nil {
		s.fail(w, err)
		return
	}
	u, err := s.store.CreateUserWithIdentity(r.Context(), r.PostFormValue("name"), models.Identity{Provider: models.ProviderLocal, Secret: hash})
	switch {
	case errors.Is(err, store.ErrExists):
		s.renderLogin(w, r, "That name is already taken.", http.StatusUnprocessableEntity)
	case errors.Is(err, store.ErrInvalid):
		s.renderLogin(w, r, "Please enter a name of 1-64 characters.", http.StatusUnprocessableEntity)
	case err != nil:
		s.fail(w, err)
	default:
		s.finishSignIn(w, r, u, models.ProviderLocal)
	}
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	u, err := s.claimableAdmin(ctx, users)
	if err != nil {
		s.fail(w, err)
		return
	}
	if u == nil || !s.passwordEnabled(ctx) {
		s.renderLogin(w, r, "This account already has a sign-in method.", http.StatusForbidden)
		return
	}
	password := r.PostFormValue("password")
	if msg := validatePassword(password, r.PostFormValue("confirm")); msg != "" {
		s.renderLogin(w, r, msg, http.StatusUnprocessableEntity)
		return
	}
	if err := s.setPassword(ctx, u.ID, password); err != nil {
		s.fail(w, err)
		return
	}
	s.finishSignIn(w, r, *u, models.ProviderLocal)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.store.DeleteSession(r.Context(), hashToken(c.Value)); err != nil {
			s.fail(w, err)
			return
		}
	}
	clearSessionCookie(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// flashParam reads the message passed to a page after a redirect.
func flashParam(r *http.Request) string {
	msg := r.URL.Query().Get("msg")
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	return msg
}
