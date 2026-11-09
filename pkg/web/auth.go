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
	valueEnabled      = "enabled"
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
	login, password, found := r.BasicAuth()
	if !found || !strings.HasPrefix(r.URL.Path, "/api/") || !s.passwordEnabled(r.Context()) {
		return models.User{}, false, nil
	}
	u, ok, err := s.throttledVerify(r, login, password)
	if errors.Is(err, errThrottled) {
		return models.User{}, false, nil
	}
	if err == nil && !ok {
		s.auditAs(r, 0, 0, auditAPIAuthFailed, "")
	}
	if err == nil && ok { // a password alone is not enough for an account with two-step verification
		has, herr := s.hasTwoFactor(r.Context(), u.ID)
		if herr != nil {
			return models.User{}, false, herr
		}
		if has || s.twoFactorRequired(r.Context(), u) {
			s.auditAs(r, u.ID, u.ID, auditAPIAuthFailed, "second_factor")
			return models.User{}, false, nil
		}
	}
	return u, ok, err
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

// bcryptCost is the work factor for new password hashes. Older hashes keep working and are
// upgraded the next time their owner signs in. Tests lower it to keep them fast.
var bcryptCost = 12

var dummyHash = sync.OnceValue(func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("dexa-fix-dummy"), bcryptCost)
	return h
})

func hashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
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

// verifyPassword checks a login and password. Unknown logins cost the same time as wrong
// passwords, so response times do not reveal which accounts exist.
var errThrottled = errors.New("too many failed sign-in attempts")

// throttledVerify checks a password after the failure limiter let the attempt through. A blocked
// client does not even cost a bcrypt run.
func (s *Server) throttledVerify(r *http.Request, login, password string) (models.User, bool, error) {
	if s.throttled(r, login) {
		return models.User{}, false, errThrottled
	}
	u, ok, err := s.verifyPassword(r.Context(), login, password)
	switch {
	case err != nil:
	case ok:
		s.limiter.Reset(loginKey(login))
	default:
		s.recordFailure(r, login)
	}
	return u, ok, err
}

func (s *Server) verifyPassword(ctx context.Context, login, password string) (models.User, bool, error) {
	fail := func() (models.User, bool, error) {
		bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
		return models.User{}, false, nil
	}
	u, err := s.store.GetUserByEmail(ctx, strings.TrimSpace(login))
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
	if cost, err := bcrypt.Cost([]byte(id.Secret)); err == nil && cost < bcryptCost {
		if err := s.setPassword(ctx, u.ID, password); err != nil { // best effort: the sign-in succeeded
			log.Warn().Err(err).Msg("upgrade password hash")
		}
	}
	return u, true, nil
}

func (s *Server) setPassword(ctx context.Context, userID int64, password string) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	if err := s.store.PutIdentity(ctx, models.Identity{UserID: userID, Provider: models.ProviderLocal, Subject: localSubject(userID), Secret: hash}); err != nil {
		return err
	}
	s.forgetResets(ctx, userID) // a link made before the change must not set the password again
	return nil
}

// passwordEnabled reports whether username and password sign-in is allowed.
func (s *Server) passwordEnabled(ctx context.Context) bool {
	if s.ForcePasswordLogin {
		return true
	}
	v, err := s.store.GetSetting(ctx, settingPasswordOn)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		// Fail closed: a database error must not re-enable a sign-in method an admin turned off.
		log.Error().Err(err).Msg("read password login setting")
		return false
	}
	return v != valueDisabled
}

// signupOpen reports whether new accounts can be created by people signing up. That is off until
// an administrator turns it on, so a server that is reachable from the internet does not hand out
// accounts. The first account can always be created, or a new server could never get an
// administrator.
func (s *Server) signupOpen(ctx context.Context) (bool, error) {
	v, err := s.settingOrEmpty(ctx, settingSignup)
	if err != nil || v == valueEnabled {
		return v == valueEnabled, err
	}
	has, err := s.store.HasUsers(ctx)
	return !has, err
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
	Providers       []loginProvider
	CanReset        bool // people can ask for a reset e-mail
	Passkeys        bool // the browser can use passkeys here
}

func (s *Server) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /signup", s.signup)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /auth/{provider}/login", s.providerStart)
	mux.HandleFunc("POST /auth/{provider}/login", s.providerStart)
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
	hasUsers, err := s.store.HasUsers(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	providers, err := s.store.ListAuthProviders(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	view := &loginView{PasswordEnabled: s.passwordEnabled(ctx), FirstUser: !hasUsers, CanReset: s.selfServiceReset(ctx), Passkeys: passkeysReady(r)}
	for _, p := range providers {
		view.Providers = append(view.Providers, loginProvider{p.Slug, p.Label})
	}
	if view.SignupOpen, err = s.signupOpen(ctx); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "index.html", status, pageData{Title: "Sign in · dexa-fix", Error: errMsg, Login: view})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.passwordEnabled(r.Context()) {
		s.renderLogin(w, r, "Signing in with a password is turned off.", http.StatusForbidden)
		return
	}
	u, ok, err := s.throttledVerify(r, r.PostFormValue("email"), r.PostFormValue("password"))
	if errors.Is(err, errThrottled) {
		s.auditAs(r, 0, 0, auditLoginThrottled, "")
		s.renderLogin(w, r, "Too many failed attempts. Please wait a few minutes and try again.", http.StatusTooManyRequests)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		s.auditAs(r, 0, 0, auditLoginFailed, "")
		s.renderLogin(w, r, "Wrong e-mail address or password.", http.StatusUnauthorized)
		return
	}
	s.signIn(w, r, u, models.ProviderLocal)
}

// signIn is where every way of signing in ends, once the user has proved who they are by method
// (ProviderLocal, a provider slug, ProviderPasskey). It redirects to the page to continue on.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, u models.User, method string) {
	target, err := s.signInTarget(w, r, u, method)
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// signInJSON is signIn for scripts that sign in with fetch, such as the passkey page: it answers
// {"redirect": target} instead of redirecting.
func (s *Server) signInJSON(w http.ResponseWriter, r *http.Request, u models.User, method string) {
	target, err := s.signInTarget(w, r, u, method)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": target})
}

// signInTarget decides what follows a proven identity and returns where to send the browser. It
// is the place for rules that depend on how the user signed in (a second step, a required
// enrolment); without such a rule it starts the session.
func (s *Server) signInTarget(w http.ResponseWriter, r *http.Request, u models.User, method string) (string, error) {
	if method == models.ProviderLocal { // see handlers_twofactor.go for why only the password asks
		if next, err := s.secondStep(w, r, u, method); err != nil || next != "" {
			return next, err
		}
	}
	return s.completeSignIn(w, r, u, method)
}

// completeSignIn starts the session and records the sign-in.
func (s *Server) completeSignIn(w http.ResponseWriter, r *http.Request, u models.User, method string) (string, error) {
	if err := s.startSession(w, r, u.ID, method); err != nil {
		return "", err
	}
	s.auditAs(r, u.ID, u.ID, auditLogin, method)
	if u.Email == "" {
		return settingsPath(u.ID, "Please add your e-mail address: it is how you sign in from now on."), nil
	}
	return userPath(u.ID, ""), nil
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	if s.limiter.Blocked(ipKey(r), maxIPFail) {
		s.renderLogin(w, r, "Too many attempts. Please wait a few minutes and try again.", http.StatusTooManyRequests)
		return
	}
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
	email, ok := parseEmail(r.PostFormValue("email"))
	if !ok {
		s.renderLogin(w, r, "Please enter a valid e-mail address.", http.StatusUnprocessableEntity)
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
	u, err := s.store.CreateUserWithIdentity(r.Context(), r.PostFormValue("name"), email, models.Identity{Provider: models.ProviderLocal, Secret: hash})
	switch {
	case errors.Is(err, store.ErrExists):
		s.limiter.Fail(ipKey(r)) // repeated probing for registered addresses is throttled
		s.renderLogin(w, r, "An account with that e-mail address already exists.", http.StatusUnprocessableEntity)
	case errors.Is(err, store.ErrInvalid):
		s.renderLogin(w, r, "Please enter a name of 1-64 characters.", http.StatusUnprocessableEntity)
	case err != nil:
		s.fail(w, err)
	default:
		s.auditAs(r, u.ID, u.ID, auditSignup, models.ProviderLocal)
		s.signIn(w, r, u, models.ProviderLocal)
	}
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if u := currentUser(r); u != nil {
		s.audit(r, u.ID, auditLogout, "")
	}
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
