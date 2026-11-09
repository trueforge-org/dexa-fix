package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// The backend half of passkeys (WebAuthn). The ceremony state lives in a sealed cookie that is
// read from the visitor's Cookie header and set with Set-Cookie, which the frontend relays. The
// finish steps receive the browser script's JSON body unchanged and parse it with the library.
// The relying party id and origin come from the visitor's host and scheme as the frontend
// forwarded them.

// passkeyCeremony is carried in a signed cookie between the two steps of a registration or a
// sign-in, so that the server stores nothing for visitors who are not signed in.
type passkeyCeremony struct {
	UID     int64                `json:"u,omitempty"`
	Name    string               `json:"n,omitempty"`
	Session webauthn.SessionData `json:"s"`
}

// passkeysReady reports whether the browser can use passkeys for this request: the address must be
// a host name (WebAuthn does not accept an IP address as the relying party) and the connection
// secure, or the host localhost, which browsers treat as secure.
func passkeysReady(r *http.Request) bool {
	host := r.Host
	if u, err := url.Parse(externalURL(r, "")); err == nil && u.Hostname() != "" {
		host = u.Hostname() // the name the relying party id will be taken from
	} else if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if net.ParseIP(host) != nil {
		return false
	}
	return isSecure(r) || host == "localhost"
}

// relyingParty configures WebAuthn for the address the browser used.
func (s *Server) relyingParty(r *http.Request) (*webauthn.WebAuthn, error) {
	origin := externalURL(r, "")
	u, err := url.Parse(origin)
	if err != nil {
		return nil, err
	}
	return webauthn.New(&webauthn.Config{
		RPDisplayName: "dexa-fix", RPID: u.Hostname(), RPOrigins: []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired, // the passkey holds the account, so no e-mail is typed
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
	})
}

// passkeyUser adapts a user and their stored credentials to the WebAuthn library.
type passkeyUser struct {
	user   models.User
	handle []byte
	creds  []webauthn.Credential
}

func (p *passkeyUser) WebAuthnID() []byte          { return p.handle }
func (p *passkeyUser) WebAuthnDisplayName() string { return p.user.Name }
func (p *passkeyUser) WebAuthnName() string {
	if p.user.Email != "" {
		return p.user.Email
	}
	return p.user.Name
}
func (p *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return p.creds }

func (s *Server) passkeyUserFor(ctx context.Context, u models.User, handle string) (*passkeyUser, error) {
	raw, err := base64.RawURLEncoding.DecodeString(handle)
	if err != nil {
		return nil, err
	}
	stored, err := s.store.ListPasskeys(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	pu := &passkeyUser{user: u, handle: raw}
	for _, p := range stored {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(p.Credential), &c); err != nil {
			log.Warn().Err(err).Int64("user", u.ID).Msg("skipping an unreadable passkey")
			continue
		}
		pu.creds = append(pu.creds, c)
	}
	return pu, nil
}

func credentialID(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

// ---- the signed cookie ----

func (s *Server) setPasskeyCookie(w http.ResponseWriter, r *http.Request, purpose string, c passkeyCeremony) error {
	sealed, _, err := s.sealValue(purpose, c, passkeyTTL)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: passkeyCookie, Value: sealed, Path: "/", MaxAge: int(passkeyTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isSecure(r),
	})
	return nil
}

func clearPasskeyCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: passkeyCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isSecure(r)})
}

// readPasskeyCookie opens the ceremony state; a state that was already used for a sign-in or a
// registration is refused.
func (s *Server) readPasskeyCookie(r *http.Request, purpose string) (c passkeyCeremony, nonce string, expires time.Time, ok bool) {
	ck, err := r.Cookie(passkeyCookie)
	if err != nil {
		return c, "", time.Time{}, false
	}
	nonce, expires, ok = s.openValue(purpose, ck.Value, &c)
	if ok && s.nonces.seen(nonce) {
		ok = false
	}
	return c, nonce, expires, ok
}

func (s *Server) internalPasskeyRoutes(mux *http.ServeMux) {
	p := apiclient.Prefix
	mux.HandleFunc("POST "+p+"/auth/passkey/begin", s.internalPasskeyLoginBegin)
	mux.HandleFunc("POST "+p+"/auth/passkey/finish", s.internalPasskeyLoginFinish)
	u := p + "/users/{id}/passkeys"
	mux.HandleFunc("POST "+u+"/begin", s.internalUser(s.internalPasskeyRegisterBegin))
	mux.HandleFunc("POST "+u+"/finish", s.internalUser(s.internalPasskeyRegisterFinish))
	mux.HandleFunc("POST "+u+"/{cred}/rename", s.internalUser(s.internalPasskeyRename))
	mux.HandleFunc("DELETE "+u+"/{cred}", s.internalUser(s.internalPasskeyDelete))
}

// passkeyFail answers a refusal of a passkey step. The browser script shows message as it is.
func passkeyFail(w http.ResponseWriter, status int, msg string) {
	code := apiclient.CodeInvalid
	switch status {
	case http.StatusUnauthorized:
		code = apiclient.CodeUnauthorized
	case http.StatusTooManyRequests:
		code = apiclient.CodeThrottled
	case http.StatusInternalServerError:
		code = apiclient.CodeInternal
	}
	internalFail(w, status, code, msg)
}

const passkeyWentWrong = "Something went wrong."

// ---- registering a passkey (signed in) ----

func (s *Server) internalPasskeyRegisterBegin(w http.ResponseWriter, r *http.Request, u models.User) {
	var in struct{ Name, Current string }
	if !internalDecode(w, r, &in) {
		return
	}
	if !passkeysReady(r) {
		passkeyFail(w, http.StatusUnprocessableEntity, "Passkeys need HTTPS and a host name (not an IP address); localhost also works.")
		return
	}
	// Adding a way into the account asks for the password again, or for a recent sign-in when
	// there is no password.
	if r.PostForm == nil {
		r.PostForm = map[string][]string{}
	}
	r.PostForm.Set("current", in.Current) // checkReauth reads the password from the form field
	ok, status, msg, err := s.checkReauth(r, u, true)
	if err != nil {
		log.Error().Err(err).Msg("passkey re-authentication")
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	if !ok {
		passkeyFail(w, status, msg)
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = defaultPasskeyName
	}
	if len([]rune(name)) > maxPasskeyName {
		passkeyFail(w, http.StatusUnprocessableEntity, "Give the passkey a name of up to 64 characters.")
		return
	}
	ctx := r.Context()
	handle, err := s.store.EnsurePasskeyHandle(ctx, u.ID)
	if err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	pu, err := s.passkeyUserFor(ctx, u, handle)
	if err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	wa, err := s.relyingParty(r)
	if err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range pu.creds {
		exclude = append(exclude, c.Descriptor())
	}
	creation, session, err := wa.BeginRegistration(pu, webauthn.WithExclusions(exclude))
	if err != nil {
		log.Error().Err(err).Msg("passkey registration could not start")
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	if err := s.setPasskeyCookie(w, r, sealPasskeyRegister, passkeyCeremony{UID: u.ID, Name: name, Session: *session}); err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	internalJSON(w, http.StatusOK, creation)
}

func (s *Server) internalPasskeyRegisterFinish(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	c, nonce, expires, ok := s.readPasskeyCookie(r, sealPasskeyRegister)
	if !ok || c.UID != u.ID {
		passkeyFail(w, http.StatusBadRequest, "The registration expired. Start again.")
		return
	}
	handle, err := s.store.EnsurePasskeyHandle(ctx, u.ID)
	if err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	pu, err := s.passkeyUserFor(ctx, u, handle)
	if err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	wa, err := s.relyingParty(r)
	if err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	cred, err := wa.FinishRegistration(pu, c.Session, r)
	if err != nil {
		log.Info().Err(err).Int64("user", u.ID).Msg("passkey registration refused")
		passkeyFail(w, http.StatusUnprocessableEntity, "The passkey could not be added. Check that the device unlocks with a fingerprint, face or PIN, and try again.")
		return
	}
	if !s.nonces.use(nonce, expires) {
		passkeyFail(w, http.StatusBadRequest, "The registration was already used. Start again.")
		return
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	err = s.store.AddPasskey(ctx, models.Passkey{CredentialID: credentialID(cred.ID), UserID: u.ID, Name: c.Name,
		Credential: string(raw), SignCount: cred.Authenticator.SignCount})
	if errors.Is(err, store.ErrExists) {
		passkeyFail(w, http.StatusUnprocessableEntity, "That passkey is already registered.")
		return
	}
	if err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	clearPasskeyCookie(w, r)
	s.audit(r, u.ID, auditPasskeyAdded, "")
	signInRedirect(w, settingsPath(u.ID, "Passkey added. You can sign in with it from now on."))
}

// ---- managing passkeys ----

func (s *Server) internalPasskeyRename(w http.ResponseWriter, r *http.Request, u models.User) {
	var in struct{ Name string }
	if !internalDecode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > maxPasskeyName {
		internalFail(w, http.StatusUnprocessableEntity, apiclient.CodeInvalid, "Give the passkey a name of 1 to 64 characters.")
		return
	}
	if err := s.store.RenamePasskey(r.Context(), u.ID, r.PathValue("cred"), name); err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusNoContent, nil)
}

func (s *Server) internalPasskeyDelete(w http.ResponseWriter, r *http.Request, u models.User) {
	var in struct{ Current string }
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	if !s.internalConfirmPassword(w, r, u, in.Current) {
		return
	}
	answer := func(msg string) { internalJSON(w, http.StatusOK, map[string]string{"message": msg}) }
	keys, err := s.store.ListPasskeys(ctx, u.ID)
	if err != nil {
		internalError(w, err)
		return
	}
	if len(keys) == 1 { // the last one: the account must keep another way in
		mine, err := s.store.UserProviders(ctx, u.ID)
		if err != nil {
			internalError(w, err)
			return
		}
		if !s.keepsSignIn(ctx, mine, models.ProviderPasskey) {
			answer("Add another way to sign in before removing your last passkey.")
			return
		}
	}
	if err := s.store.DeletePasskey(ctx, u.ID, r.PathValue("cred")); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditPasskeyRemoved, "")
	answer("Passkey removed.")
}

// passkeysFor builds the settings section.
func (s *Server) passkeysFor(r *http.Request, u models.User) (passkeysView, error) {
	v := passkeysView{Ready: passkeysReady(r)}
	keys, err := s.store.ListPasskeys(r.Context(), u.ID)
	if err != nil {
		return v, err
	}
	for _, k := range keys {
		item := passkeyItem{ID: k.CredentialID, Name: k.Name, Created: k.CreatedAt.UTC().Format(dateLayout), LastUsed: dayOrDash(k.LastUsedAt, "Never")}
		v.Items = append(v.Items, item)
	}
	return v, nil
}

// ---- signing in with a passkey ----

func (s *Server) internalPasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !passkeysReady(r) {
		passkeyFail(w, http.StatusUnprocessableEntity, "Passkeys need HTTPS and a host name (not an IP address); localhost also works.")
		return
	}
	if s.limiter.Blocked(ipKey(r), maxIPFail) {
		passkeyFail(w, http.StatusTooManyRequests, "Too many attempts. Please wait a few minutes.")
		return
	}
	wa, err := s.relyingParty(r)
	if err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	assertion, session, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		log.Error().Err(err).Msg("passkey sign-in could not start")
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	if err := s.setPasskeyCookie(w, r, sealPasskeyLogin, passkeyCeremony{Session: *session}); err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	internalJSON(w, http.StatusOK, assertion)
}

const passkeyLoginFailed = "That passkey did not work. Try again, or sign in another way."

func (s *Server) internalPasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if s.limiter.Blocked(ipKey(r), maxIPFail) {
		passkeyFail(w, http.StatusTooManyRequests, "Too many attempts. Please wait a few minutes.")
		return
	}
	c, nonce, expires, ok := s.readPasskeyCookie(r, sealPasskeyLogin)
	if !ok {
		passkeyFail(w, http.StatusBadRequest, "The sign-in expired. Try again.")
		return
	}
	wa, err := s.relyingParty(r)
	if err != nil {
		passkeyFail(w, http.StatusInternalServerError, passkeyWentWrong)
		return
	}
	var signedIn models.User
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		u, err := s.store.UserByPasskeyHandle(ctx, base64.RawURLEncoding.EncodeToString(userHandle))
		if err != nil {
			return nil, err
		}
		signedIn = u
		return s.passkeyUserFor(ctx, u, base64.RawURLEncoding.EncodeToString(userHandle))
	}
	_, cred, err := wa.FinishPasskeyLogin(handler, c.Session, r)
	if err != nil {
		s.limiter.Fail(ipKey(r))
		s.auditAs(r, 0, 0, auditPasskeyFailed, "")
		passkeyFail(w, http.StatusUnauthorized, passkeyLoginFailed)
		return
	}
	if cred.Authenticator.CloneWarning { // the counter went backwards: the credential may have been copied
		s.limiter.Fail(ipKey(r))
		s.auditAs(r, signedIn.ID, signedIn.ID, auditPasskeyClone, "")
		passkeyFail(w, http.StatusUnauthorized, passkeyLoginFailed)
		return
	}
	if !s.nonces.use(nonce, expires) {
		passkeyFail(w, http.StatusBadRequest, "The sign-in was already used. Try again.")
		return
	}
	raw, err := json.Marshal(cred)
	if err == nil {
		err = s.store.UpdatePasskeyUse(ctx, credentialID(cred.ID), string(raw), cred.Authenticator.SignCount)
	}
	if err != nil {
		log.Error().Err(err).Msg("could not record the passkey use")
	}
	clearPasskeyCookie(w, r)
	s.limiter.Reset(ipKey(r))
	s.markAdminVerified(w, r, signedIn.ID) // a passkey with its unlock is two factors: the Admin pages do not ask again
	s.internalSignIn(w, r, signedIn, models.ProviderPasskey)
}
