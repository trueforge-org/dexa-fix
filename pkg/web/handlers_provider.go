package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

const (
	providerTimeout  = 15 * time.Second
	providerStateTTL = 10 * time.Minute
	providerCookie   = "dexafix_provider_state"
	appleIssuer      = "https://appleid.apple.com"
	appleSecretTTL   = 5 * time.Minute
	maxProviderBody  = 1 << 20
	userNameLimit    = 64
)

// providerState travels through the browser during a sign-in. It is signed because Apple's
// callback is a cross-site POST that arrives without the session cookie, so the user who is
// linking an account has to be carried here and must not be forgeable.
type providerState struct {
	Slug     string `json:"p"`
	State    string `json:"s"`
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	Link     int64  `json:"l,omitempty"` // the user linking this account; 0 for a sign-in
	Expires  int64  `json:"e"`
}

func (s *Server) sealState(st providerState) (string, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + s.stateMAC(payload), nil
}

func (s *Server) openState(value string) (providerState, bool) {
	var st providerState
	payload, mac, found := strings.Cut(value, ".")
	if !found || !hmac.Equal([]byte(mac), []byte(s.stateMAC(payload))) {
		return st, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(raw, &st) != nil || time.Now().Unix() > st.Expires {
		return st, false
	}
	return st, true
}

func (s *Server) stateMAC(payload string) string {
	m := hmac.New(sha256.New, s.cookieKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func setProviderCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	c := &http.Cookie{Name: providerCookie, Value: value, Path: "/auth/", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if isSecure(r) { // Apple's cross-site POST only carries SameSite=None cookies
		c.SameSite, c.Secure = http.SameSiteNoneMode, true
	}
	http.SetCookie(w, c)
}

func (s *Server) oidcContext(ctx context.Context) context.Context {
	return oidc.ClientContext(ctx, s.httpClient)
}

// oidcProvider returns the discovered provider, fetching its configuration on first use.
func (s *Server) oidcProvider(ctx context.Context, p models.AuthProvider) (*oidc.Provider, error) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	if prov, ok := s.oidcCache[p.Issuer]; ok {
		return prov, nil
	}
	prov, err := oidc.NewProvider(s.oidcContext(ctx), p.Issuer)
	if err != nil {
		return nil, err
	}
	s.oidcCache[p.Issuer] = prov
	return prov, nil
}

func (s *Server) forgetOIDCProvider(issuer string) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	delete(s.oidcCache, issuer)
}

func (s *Server) oauthConfig(ctx context.Context, p models.AuthProvider, r *http.Request) (*oauth2.Config, *oidc.Provider, error) {
	cfg := &oauth2.Config{
		ClientID: p.ClientID, ClientSecret: p.ClientSecret,
		RedirectURL: externalURL(r, "/auth/"+p.Slug+"/callback"),
		Scopes:      strings.Fields(p.Scopes),
	}
	if p.Kind == models.ProviderKindOAuth2 {
		cfg.Endpoint = oauth2.Endpoint{AuthURL: p.AuthURL, TokenURL: p.TokenURL}
		return cfg, nil, nil
	}
	prov, err := s.oidcProvider(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	cfg.Endpoint = prov.Endpoint()
	if p.Kind == models.ProviderKindApple {
		cfg.Endpoint.AuthStyle = oauth2.AuthStyleInParams
		if cfg.ClientSecret, err = appleClientSecret(p, time.Now()); err != nil {
			return nil, nil, err
		}
	}
	return cfg, prov, nil
}

// parseAppleKey reads the .p8 private key Apple issues for Sign in with Apple.
func parseAppleKey(pemText string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("not a PEM private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an EC private key")
	}
	return ec, nil
}

// appleClientSecret signs the short-lived JWT that Apple uses instead of a static client secret.
func appleClientSecret(p models.AuthProvider, now time.Time) (string, error) {
	key, err := parseAppleKey(p.ClientSecret)
	if err != nil {
		return "", fmt.Errorf("apple key: %w", err)
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": p.AppleKeyID})
	claims, _ := json.Marshal(map[string]any{
		"iss": p.AppleTeamID, "sub": p.ClientID, "aud": appleIssuer,
		"iat": now.Unix(), "exp": now.Add(appleSecretTTL).Unix(),
	})
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := append(fixed32(r), fixed32(s)...)
	return signing + "." + enc.EncodeToString(sig), nil
}

func fixed32(n *big.Int) []byte { return n.FillBytes(make([]byte, 32)) }

// providerStart redirects to the provider, to sign in or, when already signed in, to link it.
func (s *Server) providerStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.store.GetAuthProvider(ctx, r.PathValue("provider"))
	if err != nil {
		s.fail(w, err)
		return
	}
	cfg, _, err := s.oauthConfig(ctx, p, r)
	if err != nil {
		log.Error().Err(err).Str("provider", p.Slug).Msg("provider setup failed")
		s.renderLogin(w, r, p.Label+" sign-in is not available right now.", http.StatusBadGateway)
		return
	}
	st := providerState{Slug: p.Slug, Verifier: oauth2.GenerateVerifier(), Expires: time.Now().Add(providerStateTTL).Unix()}
	if st.State, err = randomHex(16); err == nil {
		st.Nonce, err = randomHex(16)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if u := currentUser(r); u != nil {
		st.Link = u.ID
	}
	sealed, err := s.sealState(st)
	if err != nil {
		s.fail(w, err)
		return
	}
	setProviderCookie(w, r, sealed, int(providerStateTTL.Seconds()))
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(st.Verifier)}
	if p.Kind != models.ProviderKindOAuth2 {
		opts = append(opts, oidc.Nonce(st.Nonce))
	}
	if p.Kind == models.ProviderKindApple {
		opts = append(opts, oauth2.SetAuthURLParam("response_mode", "form_post"))
	}
	http.Redirect(w, r, cfg.AuthCodeURL(st.State, opts...), http.StatusSeeOther)
}

// providerCallback finishes a sign-in. Accounts are matched only by the provider's subject: never
// by name or e-mail, because that would let someone take over an account by claiming its address.
func (s *Server) providerCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("provider")
	p, err := s.store.GetAuthProvider(ctx, slug)
	if err != nil {
		s.fail(w, err)
		return
	}
	c, err := r.Cookie(providerCookie)
	if err != nil {
		http.Error(w, "invalid or expired sign-in, please start again", http.StatusBadRequest)
		return
	}
	setProviderCookie(w, r, "", -1)
	st, ok := s.openState(c.Value)
	if !ok || st.Slug != slug || !hmac.Equal([]byte(st.State), []byte(r.FormValue("state"))) {
		http.Error(w, "invalid or expired sign-in, please start again", http.StatusBadRequest)
		return
	}
	if r.FormValue("error") != "" || r.FormValue("code") == "" {
		s.providerFailed(w, r, st, p.Label+" did not complete the sign-in.")
		return
	}
	subject, claims, err := s.exchangeCode(ctx, p, r, st)
	if err != nil {
		log.Warn().Err(err).Str("provider", slug).Msg("provider sign-in failed")
		s.providerFailed(w, r, st, "Could not sign in with "+p.Label+".")
		return
	}
	id, err := s.store.GetIdentity(ctx, slug, subject)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	if st.Link != 0 {
		s.linkIdentity(w, r, p, st.Link, subject, id, found)
		return
	}
	if found {
		u, err := s.store.GetUser(ctx, id.UserID)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.finishSignIn(w, r, u, slug)
		return
	}
	open, err := s.signupOpen(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !p.AllowSignup || !open {
		s.renderLogin(w, r, "No account is linked to this "+p.Label+" login. Sign in another way and link it in Settings.", http.StatusForbidden)
		return
	}
	u, err := s.store.CreateUserWithIdentity(ctx, providerUserName(p, subject, claims), verifiedEmail(claims), models.Identity{Provider: slug, Subject: subject})
	if errors.Is(err, store.ErrExists) { // never linked automatically: the provider may not own that address
		s.renderLogin(w, r, "An account with that e-mail address already exists. Sign in and link "+p.Label+" in Settings.", http.StatusConflict)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.finishSignIn(w, r, u, slug)
}

func (s *Server) providerFailed(w http.ResponseWriter, r *http.Request, st providerState, msg string) {
	if st.Link != 0 {
		settingsRedirect(w, r, st.Link, msg)
		return
	}
	s.renderLogin(w, r, msg, http.StatusBadGateway)
}

func (s *Server) linkIdentity(w http.ResponseWriter, r *http.Request, p models.AuthProvider, userID int64, subject string, existing models.Identity, found bool) {
	if found && existing.UserID != userID {
		settingsRedirect(w, r, userID, "That "+p.Label+" login is already linked to another account.")
		return
	}
	err := s.store.PutIdentity(r.Context(), models.Identity{UserID: userID, Provider: p.Slug, Subject: subject})
	if err != nil {
		s.fail(w, err)
		return
	}
	settingsRedirect(w, r, userID, p.Label+" linked.")
}

// exchangeCode swaps the authorisation code and returns the verified subject and claims.
func (s *Server) exchangeCode(ctx context.Context, p models.AuthProvider, r *http.Request, st providerState) (string, map[string]any, error) {
	cfg, prov, err := s.oauthConfig(ctx, p, r)
	if err != nil {
		return "", nil, err
	}
	tok, err := cfg.Exchange(s.oidcContext(ctx), r.FormValue("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return "", nil, err
	}
	if p.Kind == models.ProviderKindOAuth2 {
		return s.fetchUserinfo(ctx, p, tok.AccessToken)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return "", nil, errors.New("token response has no id_token")
	}
	idt, err := prov.Verifier(&oidc.Config{ClientID: p.ClientID}).Verify(s.oidcContext(ctx), raw)
	if err != nil {
		return "", nil, err
	}
	if !hmac.Equal([]byte(idt.Nonce), []byte(st.Nonce)) {
		return "", nil, errors.New("id_token nonce mismatch")
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return "", nil, err
	}
	return idt.Subject, claims, nil
}

func (s *Server) fetchUserinfo(ctx context.Context, p models.AuthProvider, accessToken string) (string, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserinfoURL, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dexa-fix")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("userinfo: HTTP %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxProviderBody))
	dec.UseNumber() // numeric account ids must not turn into floats
	var claims map[string]any
	if err := dec.Decode(&claims); err != nil {
		return "", nil, fmt.Errorf("userinfo: %w", err)
	}
	subject := claimString(claims, cmpOr(p.SubjectClaim, "sub"))
	if subject == "" {
		return "", nil, errors.New("userinfo has no subject")
	}
	return subject, claims, nil
}

func cmpOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func claimString(claims map[string]any, name string) string {
	switch v := claims[name].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	}
	return ""
}

// providerUserName picks a display name for a new account. It is only a label.
func providerUserName(p models.AuthProvider, subject string, claims map[string]any) string {
	for _, c := range []string{p.NameClaim, "preferred_username", "name", "email"} {
		if c == "" {
			continue
		}
		if v := claimString(claims, c); v != "" {
			return truncateRunes(v, userNameLimit)
		}
	}
	return truncateRunes(p.Slug+"-"+subject, 24)
}

func truncateRunes(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len()+len(string(r)) > n {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// verifiedEmail returns the provider's e-mail claim, unless the provider says it is unverified.
func verifiedEmail(claims map[string]any) string {
	switch v := claims["email_verified"].(type) {
	case bool:
		if !v {
			return ""
		}
	case string: // Apple sends booleans as strings
		if v == "false" {
			return ""
		}
	}
	email, ok := parseEmail(claimString(claims, "email"))
	if !ok {
		return ""
	}
	return email
}

// keepsSignIn reports whether a user can still sign in after losing the given provider.
func (s *Server) keepsSignIn(ctx context.Context, providers []string, losing string) bool {
	password := s.passwordEnabled(ctx)
	return slices.ContainsFunc(providers, func(p string) bool {
		return p != losing && (p != models.ProviderLocal || password)
	})
}
