package mainbackend

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
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog/log"
	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/must"
	"github.com/trueforge-org/dexafix/pkg/store"
	"golang.org/x/oauth2"
)

const (
	providerTimeout  = 15 * time.Second
	providerStateTTL = 10 * time.Minute
	providerCookie   = "dexafix_provider_state"
	appleIssuer      = "https://appleid.apple.com"
	appleSecretTTL   = 5 * time.Minute
	maxProviderBody  = 1 << 20
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
}

const sealProvider = "provider"

func (s *Backend) sealState(st providerState) string {
	sealed, _ := must.Value2(s.sealValue(sealProvider, st, providerStateTTL)) // cannot fail: a plain struct
	return sealed
}

// openState opens a provider state cookie. The nonce and expiry are for the caller to spend once.
func (s *Backend) openState(value string) (st providerState, nonce string, expires time.Time, ok bool) {
	nonce, expires, ok = s.openValue(sealProvider, value, &st)
	return st, nonce, expires, ok
}

func setProviderCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	c := &http.Cookie{Name: providerCookie, Value: value, Path: "/auth/", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if isSecure(r) { // Apple's cross-site POST only carries SameSite=None cookies
		c.SameSite, c.Secure = http.SameSiteNoneMode, true
	}
	http.SetCookie(w, c)
}

func (s *Backend) oidcContext(ctx context.Context) context.Context {
	return oidc.ClientContext(ctx, s.httpClient)
}

// oidcProvider returns the discovered provider, fetching its configuration on first use.
func (s *Backend) oidcProvider(ctx context.Context, p models.AuthProvider) (*oidc.Provider, error) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()

	if prov, ok := s.oidcCache[p.Issuer]; ok {
		return prov, nil
	}

	oidcCtx := s.oidcContext(ctx)
	prov, err := oidc.NewProvider(oidcCtx, p.Issuer)

	if err != nil {
		var mismatch *oidc.IssuerMismatchError

		// Retry only when the difference is a trailing slash.
		if errors.As(err, &mismatch) &&
			strings.TrimSuffix(mismatch.Provided, "/") ==
				strings.TrimSuffix(mismatch.Discovered, "/") {

			// Use the exact issuer advertised by the provider.
			prov, err = oidc.NewProvider(oidcCtx, mismatch.Discovered)
		}

		if err != nil {
			return nil, err
		}
	}

	s.oidcCache[p.Issuer] = prov
	return prov, nil
}

func (s *Backend) forgetOIDCProvider(issuer string) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	delete(s.oidcCache, issuer)
}

func (s *Backend) oauthConfig(ctx context.Context, p models.AuthProvider, r *http.Request) (*oauth2.Config, *oidc.Provider, error) {
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
	r, s := must.Value2(ecdsa.Sign(rand.Reader, key, digest[:]))
	sig := append(fixed32(r), fixed32(s)...)
	return signing + "." + enc.EncodeToString(sig), nil
}

func fixed32(n *big.Int) []byte { return n.FillBytes(make([]byte, 32)) }

// exchangeCode swaps the authorisation code and returns the verified subject and claims.
func (s *Backend) exchangeCode(ctx context.Context, p models.AuthProvider, r *http.Request, st providerState) (string, map[string]any, error) {
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
	must.Do(idt.Claims(&claims)) // cannot fail: the verified token's payload already parsed as an object
	return idt.Subject, claims, nil
}

func (s *Backend) fetchUserinfo(ctx context.Context, p models.AuthProvider, accessToken string) (string, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserinfoURL, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "DexaFix")
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

// cleanUsername turns a claim into something ValidUsername accepts: other characters become '-',
// leading '.', '_' and '-' are dropped and it is cut to the longest username. It returns "" when
// fewer than two characters are left.
func cleanUsername(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() == 0:
		case r == '.' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if out := truncateRunes(b.String(), models.MaxUsernameLen); len(out) >= models.MinUsernameLen {
		return out
	}
	return ""
}

// providerUserName picks the username for a new account from the provider's claims.
func providerUserName(p models.AuthProvider, subject string, claims map[string]any) string {
	for _, c := range []string{p.NameClaim, "preferred_username", "name", "email"} {
		if c == "" {
			continue
		}
		if v := cleanUsername(claimString(claims, c)); v != "" {
			return v
		}
	}
	return cleanUsername(p.Slug + "-" + subject) // a slug starts with a letter or digit, so this has two characters at least
}

// createProviderUser creates the account of a first provider sign-in. When the username from the
// claims is taken, "-2", "-3" and so on are tried. The first and last name come from the given_name
// and family_name claims when the provider sends them.
func (s *Backend) createProviderUser(ctx context.Context, p models.AuthProvider, subject string, claims map[string]any) (models.User, error) {
	base := providerUserName(p, subject, claims)
	name := base
	var (
		u   models.User
		err error
	)
	for n := 2; ; n++ {
		u, err = s.createUserWithIdentity(ctx, name, verifiedEmail(claims), models.Identity{Provider: p.Slug, Subject: subject})
		if !errors.Is(err, store.ErrUsernameTaken) {
			break
		}
		suffix := "-" + strconv.Itoa(n)
		name = truncateRunes(base, models.MaxUsernameLen-len(suffix)) + suffix
	}
	if err != nil {
		return u, err
	}
	if first, last := claimString(claims, "given_name"), claimString(claims, "family_name"); first != "" || last != "" {
		if err := s.store.UpdateUserProfile(ctx, u.ID, u.Username, first, last, u.Email, nil); err != nil {
			log.Warn().Err(err).Int64("user", u.ID).Msg("could not store the names from the provider")
		} else {
			u.FirstName, u.LastName = strings.TrimSpace(first), strings.TrimSpace(last)
		}
	}
	return u, nil
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

// verifiedEmail returns the provider's e-mail claim, but only when the provider says it is verified.
func verifiedEmail(claims map[string]any) string {
	switch v := claims["email_verified"].(type) {
	case bool:
		if !v {
			return ""
		}
	case string: // Apple sends booleans as strings
		if v != "true" {
			return ""
		}
	default: // a provider that does not say is not trusted: the address becomes a login name
		return ""
	}
	email, ok := parseEmail(claimString(claims, "email"))
	if !ok {
		return ""
	}
	return email
}

// keepsSignIn reports whether a user can still sign in after losing the given provider.
func (s *Backend) keepsSignIn(ctx context.Context, providers []string, losing string) bool {
	password := s.passwordEnabled(ctx)
	return slices.ContainsFunc(providers, func(p string) bool {
		return p != losing && (p != models.ProviderLocal || password)
	})
}
