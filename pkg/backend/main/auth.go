package mainbackend

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
	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/store"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = apiclient.SessionCookie
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

type userKey struct{}

// currentUser returns the authenticated user of the request, or nil.
func currentUser(r *http.Request) *models.User {
	u, _ := r.Context().Value(userKey{}).(*models.User)
	return u
}

// authenticator resolves a request to a user. It returns ok=false when the request does not carry
// this kind of credential or the credential is not valid.
//
// Every way of proving who someone is lives in this chain, so another one, such as a header set by
// a trusted reverse proxy, is one more entry in Server.authChain.
// bcryptWait is how long a Basic-auth request waits for a password check slot.
var bcryptWait = 2 * time.Second

type authenticator func(r *http.Request) (u models.User, ok bool, err error)

// withAuth runs the authenticator chain and stores the user in the request context.
func (s *Backend) withAuth(next http.Handler) http.Handler {
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

func (s *Backend) authSession(r *http.Request) (models.User, bool, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return models.User{}, false, nil
	}
	return foundUser(s.sessions.UserBySession(r.Context(), hashToken(c.Value)))
}

// authBearer accepts personal access tokens and ID tokens issued by a configured provider. Bearer
// and Basic credentials only work on the API, so a leaked token cannot change a password or mint
// new tokens through the web forms.
func (s *Backend) authBearer(r *http.Request) (models.User, bool, error) {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || !strings.HasPrefix(r.URL.Path, "/api/") {
		return models.User{}, false, nil
	}
	if strings.HasPrefix(token, tokenPrefix) {
		return foundUser(s.store.UserByTokenHash(r.Context(), hashToken(token)))
	}
	return s.authProviderToken(r.Context(), token)
}

func (s *Backend) authBasic(r *http.Request) (models.User, bool, error) {
	login, password, found := r.BasicAuth()
	if !found || !strings.HasPrefix(r.URL.Path, "/api/") || !s.passwordEnabled(r.Context()) {
		return models.User{}, false, nil
	}
	u, ok, err := s.throttledVerify(r, login, password)
	if errors.Is(err, errThrottled) || errors.Is(err, errBusy) {
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
func (s *Backend) authProviderToken(ctx context.Context, raw string) (models.User, bool, error) {
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

// bcryptCost is the work factor for new password hashes. Older hashes keep working and are
// upgraded the next time their owner signs in. Tests lower it to keep them fast.
var bcryptCost = 12

var dummyHash = sync.OnceValue(func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("dexafix-dummy"), bcryptCost)
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

// errBusy is a password check that found every slot taken for bcryptWait.
var errBusy = errors.New("every password check slot is busy")

// bcryptSlot takes one of the few places where a password may be checked. Checking costs a lot of
// CPU, and a flood of sign-ins, Basic credentials or confirmations must not use all of it, so only
// a few run at once; the rest wait a moment and then give up. The caller releases the slot.
func (s *Backend) bcryptSlot(ctx context.Context) (release func(), ok bool) {
	select {
	case s.bcryptSlots <- struct{}{}:
		return func() { <-s.bcryptSlots }, true
	case <-time.After(bcryptWait):
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// throttledVerify checks a password after the failure limiter let the attempt through. A blocked
// client does not even cost a bcrypt run.
func (s *Backend) throttledVerify(r *http.Request, login, password string) (models.User, bool, error) {
	ctx := r.Context()
	if s.throttled(r, login) { // only reads: a client that is blocked costs no slot and no attempt
		return models.User{}, false, errThrottled
	}
	release, ok := s.bcryptSlot(ctx)
	if !ok {
		return models.User{}, false, errBusy // nothing was counted: the check did not happen
	}
	defer release()
	// The attempt is counted before the password is checked, so that a burst of parallel guesses
	// cannot all pass the check above before the first failure is on record.
	if login != "" && !s.limiter.Attempt(ctx, loginKey(login), maxLoginFail) {
		return models.User{}, false, errThrottled
	}
	u, ok, err := s.verifyPassword(ctx, login, password)
	switch {
	case err != nil:
	case ok:
		s.limiter.Reset(ctx, loginKey(login))
	default:
		s.recordFailure(r)
	}
	return u, ok, err
}

// verifyPassword runs inside a password check slot (throttledVerify takes it).
func (s *Backend) verifyPassword(ctx context.Context, login, password string) (models.User, bool, error) {
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

func (s *Backend) setPassword(ctx context.Context, userID int64, password string) error {
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
func (s *Backend) passwordEnabled(ctx context.Context) bool {
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
func (s *Backend) signupOpen(ctx context.Context) (bool, error) {
	v, err := s.settingOrEmpty(ctx, settingSignup)
	if err != nil || v == valueEnabled {
		return v == valueEnabled, err
	}
	has, err := s.store.HasUsers(ctx)
	return !has, err
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails: since Go 1.24 a broken random source ends the program
	return hex.EncodeToString(b)
}

// hashToken hashes session ids and access tokens. They are long random values, so a fast hash is
// enough: unlike passwords they cannot be guessed from a dictionary.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Backend) loadCookieKey(ctx context.Context) ([]byte, error) {
	v, err := s.store.GetSetting(ctx, settingCookieKey)
	if errors.Is(err, store.ErrNotFound) {
		v = randomHex(32)
		err = s.store.PutSetting(ctx, settingCookieKey, v)
	}
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(v)
}

// startSession signs the user in, whichever way they proved who they are.
func (s *Backend) startSession(w http.ResponseWriter, r *http.Request, userID int64, method string) error {
	token := randomHex(32)
	expires := time.Now().Add(sessionTTL)
	if err := s.sessions.CreateSession(r.Context(), hashToken(token), userID, method, expires); err != nil {
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

// signInTarget decides what follows a proven identity and returns where to send the browser. It
// is the place for rules that depend on how the user signed in (a second step, a required
// enrolment); without such a rule it starts the session.
func (s *Backend) signInTarget(w http.ResponseWriter, r *http.Request, u models.User, method string) (string, error) {
	if method == models.ProviderLocal { // see handlers_twofactor.go for why only the password asks
		if next, err := s.secondStep(w, r, u, method); err != nil || next != "" {
			return next, err
		}
	}
	return s.completeSignIn(w, r, u, method)
}

// completeSignIn starts the session and records the sign-in.
func (s *Backend) completeSignIn(w http.ResponseWriter, r *http.Request, u models.User, method string) (string, error) {
	if err := s.startSession(w, r, u.ID, method); err != nil {
		return "", err
	}
	s.auditAs(r, u.ID, u.ID, auditLogin, method)
	if u.Email == "" {
		return settingsPath(u.ID, "Please add your e-mail address: it is how you sign in from now on."), nil
	}
	return userPath(u.ID, ""), nil
}
