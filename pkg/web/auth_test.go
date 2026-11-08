package web

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func newAuthServer(t *testing.T) (*httptest.Server, *http.Client, *sqlstore.Store) {
	t.Helper()
	return newServerAndStore(t, withings.New(withings.Config{}, nil))
}

func apiCall(t *testing.T, c *http.Client, method, u, payload string, hdr http.Header) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(payload))
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func bearer(token string) http.Header { return http.Header{"Authorization": {"Bearer " + token}} }

func TestAccessIsPerUser(t *testing.T) {
	ts, alice, _ := newAuthServer(t)
	if code, b := body(t, alice, ts.URL+"/"); code != 200 || !strings.Contains(b, "first account") {
		t.Fatalf("empty sign-in page: %d", code)
	}
	if code, _ := body(t, alice, ts.URL+"/u/1/scale"); code != 303 {
		t.Fatalf("anonymous page: %d", code)
	}
	signup(t, ts, alice, "alice")
	bob := noRedirectClient()
	signup(t, ts, bob, "bob")

	if code, _ := body(t, bob, ts.URL+"/u/1/scale"); code != 404 {
		t.Errorf("other user's page: %d", code)
	}
	if code, _ := apiCall(t, bob, "GET", ts.URL+"/api/v1/users/1/estimates", "", nil); code != 404 {
		t.Errorf("other user's API: %d", code)
	}
	if code, _ := apiCall(t, bob, "GET", ts.URL+"/api/v1/users", "", nil); code != 403 {
		t.Errorf("non-admin user list: %d", code)
	}
	if code, _ := body(t, bob, ts.URL+"/admin"); code != 404 {
		t.Errorf("non-admin admin page: %d", code)
	}
	if _, b := body(t, bob, ts.URL+"/u/2/settings"); strings.Contains(b, `href="/admin"`) {
		t.Error("admin link shown to a non-admin")
	}
	if code, b := body(t, alice, ts.URL+"/admin"); code != 200 || !strings.Contains(b, "Server status") || !strings.Contains(b, "bob") {
		t.Errorf("admin page: %d", code)
	}
}

func TestPasswordsAreHashed(t *testing.T) {
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "alice")
	id, err := st.GetIdentity(context.Background(), models.ProviderLocal, "1")
	if err != nil {
		t.Fatal(err)
	}
	if id.Secret == testPassword || !strings.HasPrefix(id.Secret, "$2") {
		t.Fatalf("stored secret is not a bcrypt hash: %q", id.Secret)
	}
}

func TestLoginAndLogout(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	signup(t, ts, c, "alice")

	anon := noRedirectClient()
	if r := post(t, anon, ts.URL+"/login", url.Values{"email": {"alice@example.com"}, "password": {"wrong password"}}); r.StatusCode != 401 {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	if r := post(t, anon, ts.URL+"/login", url.Values{"email": {"nobody@example.com"}, "password": {testPassword}}); r.StatusCode != 401 {
		t.Fatalf("unknown user: %d", r.StatusCode)
	}
	if r := post(t, anon, ts.URL+"/signup", url.Values{"name": {"alice"}, "email": {"alice@example.com"}, "password": {testPassword}, "confirm": {testPassword}}); r.StatusCode != 422 {
		t.Fatalf("duplicate name: %d", r.StatusCode)
	}
	if r := post(t, anon, ts.URL+"/signup", url.Values{"name": {"x"}, "email": {"x@example.com"}, "password": {"short"}, "confirm": {"short"}}); r.StatusCode != 422 {
		t.Fatalf("short password: %d", r.StatusCode)
	}
	if r := post(t, anon, ts.URL+"/signup", url.Values{"name": {"x"}, "email": {"x@example.com"}, "password": {testPassword}, "confirm": {"different one"}}); r.StatusCode != 422 {
		t.Fatalf("mismatch: %d", r.StatusCode)
	}
	r := post(t, anon, ts.URL+"/login", url.Values{"email": {"alice@example.com"}, "password": {testPassword}})
	if r.StatusCode != 303 || r.Header.Get("Location") != "/u/1" {
		t.Fatalf("login: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	useSession(t, anon, r)
	if code, _ := body(t, anon, ts.URL+"/u/1/scale"); code != 200 {
		t.Fatalf("after login: %d", code)
	}
	if r := post(t, anon, ts.URL+"/logout", nil); r.StatusCode != 303 {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if code, _ := body(t, anon, ts.URL+"/u/1/scale"); code != 303 { // the same cookie no longer works
		t.Fatalf("after logout: %d", code)
	}
}

func TestCrossOriginFormRefused(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	signup(t, ts, c, "alice")
	hdr := http.Header{"Sec-Fetch-Site": {"cross-site"}, "Content-Type": {"application/x-www-form-urlencoded"}}
	if code, _ := apiCall(t, c, "POST", ts.URL+"/u/1/tokens", "name=x", hdr); code != 403 {
		t.Fatalf("cross-site POST: %d", code)
	}
}

var patRe = regexp.MustCompile(`dxf_[0-9a-f]{64}`)

func createPAT(t *testing.T, ts *httptest.Server, c *http.Client, id int, form url.Values) string {
	t.Helper()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/u/%d/tokens", ts.URL, id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	token := patRe.FindString(string(b))
	if resp.StatusCode != 200 || token == "" {
		t.Fatalf("create token: %d", resp.StatusCode)
	}
	return token
}

func TestPersonalAccessTokens(t *testing.T) {
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "alice")
	token := createPAT(t, ts, c, 1, url.Values{"name": {"script"}, "expires": {"never"}})

	anon := noRedirectClient()
	if code, _ := apiCall(t, anon, "GET", ts.URL+"/api/v1/users/1/settings", "", bearer(token)); code != 200 {
		t.Fatalf("token on API: %d", code)
	}
	if code, _ := apiCall(t, anon, "GET", ts.URL+"/u/1/settings", "", bearer(token)); code != 303 {
		t.Fatalf("token must not open web pages: %d", code)
	}
	if code, _ := apiCall(t, anon, "GET", ts.URL+"/api/v1/users/1/settings", "", bearer("dxf_nope")); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	if _, b := body(t, c, ts.URL+"/u/1/settings"); strings.Contains(b, token) || !strings.Contains(b, token[:tokenPrefixShown]) || !strings.Contains(b, "script") {
		t.Fatal("the full token must only be shown once; the list shows its prefix")
	}
	ctx := context.Background()
	list, err := st.ListAccessTokens(ctx, 1)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	if _, err := st.UserByTokenHash(ctx, token); err == nil {
		t.Fatal("token must be stored only as a hash")
	}
	if list[0].LastUsedAt == nil {
		t.Error("last used not recorded")
	}

	// expired tokens stop working
	past := time.Now().Add(-time.Hour)
	expired := models.AccessToken{UserID: 1, Name: "old", Prefix: "dxf_", Hash: hashToken("dxf_expired"), ExpiresAt: &past}
	if err := st.CreateAccessToken(ctx, &expired); err != nil {
		t.Fatal(err)
	}
	if code, _ := apiCall(t, anon, "GET", ts.URL+"/api/v1/users/1/settings", "", bearer("dxf_expired")); code != 401 {
		t.Fatalf("expired token: %d", code)
	}

	if r := post(t, c, fmt.Sprintf("%s/u/1/tokens/%d/delete", ts.URL, list[0].ID), nil); r.StatusCode != 303 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if code, _ := apiCall(t, anon, "GET", ts.URL+"/api/v1/users/1/settings", "", bearer(token)); code != 401 {
		t.Fatalf("revoked token: %d", code)
	}

	// someone else's token cannot be revoked
	bob := noRedirectClient()
	signup(t, ts, bob, "bob")
	if r := post(t, bob, fmt.Sprintf("%s/u/2/tokens/%d/delete", ts.URL, expired.ID), nil); r.StatusCode != 404 {
		t.Fatalf("foreign revoke: %d", r.StatusCode)
	}
}

func TestBasicAuthOnAPI(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	signup(t, ts, c, "alice")
	do := func(pw string) int {
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/1/settings", nil)
		req.SetBasicAuth("alice@example.com", pw)
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := do(testPassword); code != 200 {
		t.Fatalf("basic: %d", code)
	}
	if code := do("wrong password"); code != 401 {
		t.Fatalf("wrong basic: %d", code)
	}
}

func TestProfile(t *testing.T) {
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "alice")
	bob := noRedirectClient()
	signup(t, ts, bob, "bob")

	r := post(t, c, ts.URL+"/u/1/profile", url.Values{"name": {"Bob"}, "email": {"Alice.New@Example.com"}, "height_cm": {"172,5"}})
	if r.StatusCode != 303 { // names need not be unique
		t.Fatalf("save: %d", r.StatusCode)
	}
	u, err := st.GetUser(context.Background(), 1)
	if err != nil || u.Name != "Bob" || u.Email != "alice.new@example.com" || u.HeightCm == nil || *u.HeightCm != 172.5 {
		t.Fatalf("stored: %+v %v", u, err)
	}
	if _, b := body(t, c, ts.URL+"/u/1/settings"); !strings.Contains(b, `value="alice.new@example.com"`) || !strings.Contains(b, `value="172.5"`) {
		t.Fatalf("settings form not filled:\n%s", b)
	}
	ok := url.Values{"name": {"A"}, "email": {"alice.new@example.com"}}
	for name, form := range map[string]url.Values{
		"taken email": {"name": {"A"}, "email": {"BOB@example.com"}},
		"no email":    {"name": {"A"}},
		"bad email":   {"name": {"A"}, "email": {"nope"}},
		"empty name":  {"name": {" "}, "email": ok["email"]},
		"bad height":  {"name": {"A"}, "email": ok["email"], "height_cm": {"20"}},
		"text height": {"name": {"A"}, "email": ok["email"], "height_cm": {"tall"}},
	} {
		if r := post(t, c, ts.URL+"/u/1/profile", form); r.StatusCode != 422 {
			t.Errorf("%s: %d", name, r.StatusCode)
		}
	}
	// signing in follows the new address, in any letter case
	if r := post(t, noRedirectClient(), ts.URL+"/login", url.Values{"email": {"ALICE.new@example.com"}, "password": {testPassword}}); r.StatusCode != 303 {
		t.Fatalf("login with new address: %d", r.StatusCode)
	}
	if r := post(t, noRedirectClient(), ts.URL+"/login", url.Values{"email": {"alice@example.com"}, "password": {testPassword}}); r.StatusCode != 401 {
		t.Fatalf("login with old address: %d", r.StatusCode)
	}
	if r := post(t, noRedirectClient(), ts.URL+"/login", url.Values{"email": {"Bob"}, "password": {testPassword}}); r.StatusCode != 401 {
		t.Fatalf("name must not work when the account has an address: %d", r.StatusCode)
	}
	post(t, c, ts.URL+"/u/1/profile", ok)
	if u, _ := st.GetUser(context.Background(), 1); u.HeightCm != nil {
		t.Fatalf("height not cleared: %+v", u)
	}
}

func TestVerifiedEmail(t *testing.T) {
	for _, tt := range []struct {
		claims map[string]any
		want   string
	}{
		{map[string]any{"email": "A@Example.com", "email_verified": true}, "a@example.com"},
		{map[string]any{"email": "a@example.com"}, ""}, // a provider that does not say is not trusted
		{map[string]any{"email": "a@example.com", "email_verified": "yes"}, ""},
		{map[string]any{"email": "a@example.com", "email_verified": true}, "a@example.com"},
		{map[string]any{"email": "a@example.com", "email_verified": "true"}, "a@example.com"},
		{map[string]any{"email": "a@example.com", "email_verified": false}, ""},
		{map[string]any{"email": "a@example.com", "email_verified": "false"}, ""},
		{map[string]any{"email": "not an address"}, ""},
		{map[string]any{}, ""},
	} {
		if got := verifiedEmail(tt.claims); got != tt.want {
			t.Errorf("verifiedEmail(%v) = %q, want %q", tt.claims, got, tt.want)
		}
	}
}

func TestMethodOrderList(t *testing.T) {
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "alice")
	if _, b := body(t, c, ts.URL+"/u/1/settings"); !strings.Contains(b, "data-rank-list") || !strings.Contains(b, `name="rank_scale"`) || !strings.Contains(b, "/static/rank.js") {
		t.Fatalf("order list missing:\n%s", b)
	}
	if code, _ := body(t, c, ts.URL+"/static/rank.js"); code != 200 {
		t.Fatalf("rank.js: %d", code)
	}
	post(t, c, ts.URL+"/u/1/settings", url.Values{"rank_scale": {"1"}, "rank_hydrostatic": {"2"}, "rank_bodpod": {"3"}, "rank_caliper": {"4"}, "rank_tape": {"5"}, "rank_visual-ai": {"6"}})
	u, _ := st.GetUser(context.Background(), 1)
	if u.MethodRanking[0] != "scale" || u.MethodRanking[1] != "hydrostatic" {
		t.Fatalf("order: %v", u.MethodRanking)
	}
}

func TestChangePassword(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	signup(t, ts, c, "alice")
	other := noRedirectClient()
	useSession(t, other, post(t, other, ts.URL+"/login", url.Values{"email": {"alice@example.com"}, "password": {testPassword}}))

	token := createPAT(t, ts, c, 1, url.Values{"name": {"script"}})
	if code, _ := apiCall(t, noRedirectClient(), "GET", ts.URL+"/api/v1/users/1/settings", "", bearer(token)); code != 200 {
		t.Fatalf("token before change: %d", code)
	}

	const next = "a brand new password"
	if r := post(t, c, ts.URL+"/u/1/password", url.Values{"current": {"wrong one here"}, "password": {next}, "confirm": {next}}); r.StatusCode != 422 {
		t.Fatalf("wrong current: %d", r.StatusCode)
	}
	r := post(t, c, ts.URL+"/u/1/password", url.Values{"current": {testPassword}, "password": {next}, "confirm": {next}})
	if r.StatusCode != 200 {
		t.Fatalf("change: %d", r.StatusCode)
	}
	if code, _ := body(t, other, ts.URL+"/u/1/settings"); code != 303 {
		t.Fatalf("other sessions must end: %d", code)
	}
	if code, _ := apiCall(t, noRedirectClient(), "GET", ts.URL+"/api/v1/users/1/settings", "", bearer(token)); code != 401 {
		t.Fatalf("tokens must be revoked by a password change: %d", code)
	}
	if r := post(t, noRedirectClient(), ts.URL+"/login", url.Values{"email": {"alice@example.com"}, "password": {next}}); r.StatusCode != 303 {
		t.Fatalf("login with new password: %d", r.StatusCode)
	}
}

func TestAdminManagesUsers(t *testing.T) {
	ts, admin, st := newAuthServer(t)
	ctx := context.Background()
	signup(t, ts, admin, "root")
	bob := noRedirectClient()
	signup(t, ts, bob, "bob")

	msg := func(r *http.Response) string {
		t.Helper()
		loc, _ := url.Parse(r.Header.Get("Location"))
		if r.StatusCode != 303 || loc.Path != "/admin" {
			t.Fatalf("redirect: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
		return loc.Query().Get("msg")
	}
	if r := post(t, bob, ts.URL+"/admin/users/2/role", url.Values{"role": {"admin"}}); r.StatusCode != 404 {
		t.Fatalf("non-admin promote: %d", r.StatusCode)
	}
	if m := msg(post(t, admin, ts.URL+"/admin/users/1/role", url.Values{"role": {"user"}})); !strings.Contains(m, "at least one") {
		t.Fatalf("last admin: %q", m)
	}
	if m := msg(post(t, admin, ts.URL+"/admin/users/1/delete", nil)); !strings.Contains(m, "your own") {
		t.Fatalf("self delete: %q", m)
	}
	if m := msg(post(t, admin, ts.URL+"/admin/users/2/role", url.Values{"role": {"admin"}})); !strings.Contains(m, "admin") {
		t.Fatalf("promote: %q", m)
	}
	if code, _ := body(t, bob, ts.URL+"/admin"); code != 200 {
		t.Fatalf("promoted user: %d", code)
	}
	if m := msg(post(t, admin, ts.URL+"/admin/users/2/role", url.Values{"role": {"user"}})); !strings.Contains(m, "is now user") {
		t.Fatalf("demote: %q", m)
	}

	// an admin-set password ends the user's sessions
	const fresh = "set by the admin"
	msg(post(t, admin, ts.URL+"/admin/users/2/password", url.Values{"password": {fresh}}))
	if code, _ := body(t, bob, ts.URL+"/u/2/settings"); code != 303 {
		t.Fatalf("session after reset: %d", code)
	}
	if r := post(t, noRedirectClient(), ts.URL+"/login", url.Values{"email": {"bob@example.com"}, "password": {fresh}}); r.StatusCode != 303 {
		t.Fatalf("login after reset: %d", r.StatusCode)
	}

	if err := st.CreateDexaScan(ctx, &models.DexaScan{UserID: 2, MeasuredAt: time.Now(), WeightKg: new(float64)}); err != nil {
		t.Fatal(err)
	}
	msg(post(t, admin, ts.URL+"/admin/users/2/delete", nil))
	if _, err := st.GetUser(ctx, 2); err == nil {
		t.Fatal("user still exists")
	}
	if scans, _ := st.ListDexaScans(ctx, 2); len(scans) != 0 {
		t.Fatalf("data left behind: %d", len(scans))
	}
}

func TestAdminSavesWithingsCredentials(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	signup(t, ts, c, "root")
	if _, b := body(t, c, ts.URL+"/u/1/settings"); !strings.Contains(b, "Not set up") {
		t.Fatal("should start unconfigured")
	}
	const secret = "s3cret-value"
	post(t, c, ts.URL+"/admin/withings", url.Values{"client_id": {"cid"}, "client_secret": {secret}, "redirect_url": {"https://app.example/cb"}})
	if _, b := body(t, c, ts.URL+"/u/1/settings"); !strings.Contains(b, "/u/1/integrations/withings/connect") {
		t.Fatalf("connect link missing after saving:\n%s", b)
	}
	if _, b := body(t, c, ts.URL+"/admin"); strings.Contains(b, secret) {
		t.Fatal("the secret must never be sent back")
	}
	// keeps the secret when left empty
	post(t, c, ts.URL+"/admin/withings", url.Values{"client_id": {"cid2"}, "redirect_url": {"https://app.example/cb"}})
	if _, b := body(t, c, ts.URL+"/u/1/settings"); !strings.Contains(b, "/u/1/integrations/withings/connect") {
		t.Fatal("secret lost")
	}
	// an empty redirect URL defaults to this server's own callback
	post(t, c, ts.URL+"/admin/withings", url.Values{"client_id": {"cid3"}})
	if _, b := body(t, c, ts.URL+"/admin"); !strings.Contains(b, ts.URL+"/integrations/withings/callback") {
		t.Fatalf("default redirect URL missing:\n%s", b)
	}
}

// fakeIdP is a minimal OpenID Connect provider.
type fakeIdP struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu      sync.Mutex
	subject string
	nonce   string
	badNone bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key, subject: "sub-1"}
	mux := http.NewServeMux()
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": idp.URL, "authorization_endpoint": idp.URL + "/authorize", "token_endpoint": idp.URL + "/token",
			"jwks_uri": idp.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		e := big.NewInt(int64(key.E)).Bytes()
		enc := base64.RawURLEncoding
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1", "n": enc.EncodeToString(key.N.Bytes()), "e": enc.EncodeToString(e)}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		defer idp.mu.Unlock()
		nonce := idp.nonce
		if idp.badNone {
			nonce = "something-else"
		}
		clientID, _, ok := r.BasicAuth()
		if !ok {
			clientID = r.FormValue("client_id")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": idp.token(t, clientID, idp.subject, nonce)})
	})
	return idp
}

func (idp *fakeIdP) token(t *testing.T, aud, sub, nonce string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": idp.URL, "aud": aud, "sub": sub, "nonce": nonce, "email": sub + "@example.com", "email_verified": true,
		"name": "User " + sub, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()})
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + enc.EncodeToString(sig)
}

// signInVia runs the browser side of the provider flow and returns the callback response.
func (idp *fakeIdP) signInVia(t *testing.T, ts *httptest.Server, c *http.Client, slug string) *http.Response {
	t.Helper()
	resp, err := c.Post(ts.URL+"/auth/"+slug+"/login", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	auth, _ := url.Parse(resp.Header.Get("Location"))
	if auth.Query().Get("code_challenge") == "" || auth.Query().Get("nonce") == "" {
		t.Fatalf("missing PKCE or nonce: %s", auth)
	}
	idp.mu.Lock()
	idp.nonce = auth.Query().Get("nonce")
	idp.mu.Unlock()
	req, _ := http.NewRequest("GET", ts.URL+"/auth/"+slug+"/callback?code=abc&state="+auth.Query().Get("state"), nil)
	for _, ck := range resp.Cookies() {
		req.AddCookie(ck)
	}
	r, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	return r
}

func addProvider(t *testing.T, ts *httptest.Server, c *http.Client, idp *fakeIdP, signup bool) {
	t.Helper()
	form := url.Values{"kind": {"oidc"}, "slug": {"idp"}, "label": {"Test IdP"}, "issuer": {idp.URL}, "client_id": {"client-1"}, "client_secret": {"s3cret-value"}}
	if signup {
		form.Set("allow_signup", "1")
	}
	r := post(t, c, ts.URL+"/admin/providers", form)
	if loc, _ := url.Parse(r.Header.Get("Location")); r.StatusCode != 303 || !strings.Contains(loc.Query().Get("msg"), "saved") {
		t.Fatalf("add provider: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
}

func TestProviderSignIn(t *testing.T) {
	idp := newFakeIdP(t)
	ts, admin, st := newAuthServer(t)
	ctx := context.Background()
	signup(t, ts, admin, "root")

	if m := post(t, admin, ts.URL+"/admin/providers", url.Values{"kind": {"oidc"}, "slug": {"local"}, "label": {"x"}, "issuer": {idp.URL}, "client_id": {"c"}, "client_secret": {"s"}}); !strings.Contains(m.Header.Get("Location"), "short+name") {
		t.Fatalf("reserved slug: %s", m.Header.Get("Location"))
	}
	if m := post(t, admin, ts.URL+"/admin/providers", url.Values{"kind": {"oidc"}, "slug": {"x"}, "label": {"x"}, "issuer": {"http://example.com"}, "client_id": {"c"}, "client_secret": {"s"}}); !strings.Contains(m.Header.Get("Location"), "https") {
		t.Fatalf("plain http issuer: %s", m.Header.Get("Location"))
	}
	addProvider(t, ts, admin, idp, false)
	if _, b := body(t, admin, ts.URL+"/admin"); strings.Contains(b, "s3cret-value") || !strings.Contains(b, "/auth/idp/callback") {
		t.Fatal("admin page must show the callback URL and hide the secret")
	}
	if _, b := body(t, noRedirectClient(), ts.URL+"/"); !strings.Contains(b, "Continue with Test IdP") {
		t.Fatal("provider button missing")
	}

	// not linked and sign-up off: refused, never matched by name or e-mail
	if r := idp.signInVia(t, ts, noRedirectClient(), "idp"); r.StatusCode != 403 {
		t.Fatalf("unlinked: %d", r.StatusCode)
	}

	// passwords cannot be disabled until the admin has a linked account
	if r := post(t, admin, ts.URL+"/admin/password-login", url.Values{"enabled": {"0"}}); !strings.Contains(r.Header.Get("Location"), "locked") {
		t.Fatalf("disable before link: %s", r.Header.Get("Location"))
	}

	// link while signed in
	if r := idp.signInVia(t, ts, admin, "idp"); r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), "linked") {
		t.Fatalf("link: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	id, err := st.GetIdentity(ctx, "idp", "sub-1")
	if err != nil || id.UserID != 1 {
		t.Fatalf("identity: %+v %v", id, err)
	}

	// sign in through the provider
	c := noRedirectClient()
	r := idp.signInVia(t, ts, c, "idp")
	if r.StatusCode != 303 || r.Header.Get("Location") != "/u/1" {
		t.Fatalf("sign in: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	useSession(t, c, r)
	if code, _ := body(t, c, ts.URL+"/u/1/settings"); code != 200 {
		t.Fatalf("session: %d", code)
	}

	// the provider's own token works on the API, but only when issued for this application
	token := idp.token(t, "client-1", "sub-1", "")
	if code, _ := apiCall(t, noRedirectClient(), "GET", ts.URL+"/api/v1/users/1/settings", "", bearer(token)); code != 200 {
		t.Fatalf("provider token: %d", code)
	}
	if code, _ := apiCall(t, noRedirectClient(), "GET", ts.URL+"/api/v1/users/1/settings", "", bearer(idp.token(t, "another-app", "sub-1", ""))); code != 401 {
		t.Fatalf("wrong audience: %d", code)
	}
	if code, _ := apiCall(t, noRedirectClient(), "GET", ts.URL+"/api/v1/users/1/settings", "", bearer(idp.token(t, "client-1", "stranger", ""))); code != 401 {
		t.Fatalf("unlinked subject: %d", code)
	}
	if code, _ := apiCall(t, noRedirectClient(), "GET", ts.URL+"/u/1/settings", "", bearer(token)); code != 303 {
		t.Fatalf("provider token on web page: %d", code)
	}
	forged := token[:strings.LastIndexByte(token, '.')] + ".AAAA"
	if code, _ := apiCall(t, noRedirectClient(), "GET", ts.URL+"/api/v1/users/1/settings", "", bearer(forged)); code != 401 {
		t.Fatalf("forged signature: %d", code)
	}

	// a wrong nonce is refused
	idp.mu.Lock()
	idp.badNone = true
	idp.mu.Unlock()
	if r := idp.signInVia(t, ts, noRedirectClient(), "idp"); r.StatusCode != 502 {
		t.Fatalf("bad nonce: %d", r.StatusCode)
	}
	idp.mu.Lock()
	idp.badNone = false
	idp.mu.Unlock()

	// turn passwords off
	if r := post(t, admin, ts.URL+"/admin/password-login", url.Values{"enabled": {"0"}}); !strings.Contains(r.Header.Get("Location"), "is+off") {
		t.Fatalf("disable: %s", r.Header.Get("Location"))
	}
	anon := noRedirectClient()
	if _, b := body(t, anon, ts.URL+"/"); strings.Contains(b, `action="/login"`) || strings.Contains(b, `action="/signup"`) {
		t.Fatal("password forms must be hidden")
	}
	if r := post(t, anon, ts.URL+"/login", url.Values{"email": {"root@example.com"}, "password": {testPassword}}); r.StatusCode != 403 {
		t.Fatalf("password login: %d", r.StatusCode)
	}
	if r := post(t, anon, ts.URL+"/signup", url.Values{"name": {"new"}, "email": {"new@example.com"}, "password": {testPassword}, "confirm": {testPassword}}); r.StatusCode != 403 {
		t.Fatalf("password signup: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/1/settings", nil)
	req.SetBasicAuth("root@example.com", testPassword)
	if resp, err := anon.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("basic with passwords off: %v %v", resp, err)
	}
	pat := createPAT(t, ts, admin, 1, url.Values{"name": {"script"}, "expires": {"30"}})
	if code, _ := apiCall(t, anon, "GET", ts.URL+"/api/v1/users/1/settings", "", bearer(pat)); code != 200 {
		t.Fatalf("PAT with passwords off: %d", code)
	}

	// the admin's only way in cannot be removed while passwords are off
	if r := post(t, admin, ts.URL+"/admin/providers/idp/delete", nil); !strings.Contains(r.Header.Get("Location"), "only+way") {
		t.Fatalf("lockout guard: %s", r.Header.Get("Location"))
	}
	if r := post(t, admin, ts.URL+"/u/1/identities/idp/delete", nil); !strings.Contains(r.Header.Get("Location"), "another+way") {
		t.Fatalf("unlink guard: %s", r.Header.Get("Location"))
	}

	// with passwords back on the provider can go, and its identities go with it
	post(t, admin, ts.URL+"/admin/password-login", url.Values{"enabled": {"1"}})
	post(t, admin, ts.URL+"/admin/providers/idp/delete", nil)
	if _, err := st.GetIdentity(ctx, "idp", "sub-1"); err == nil {
		t.Fatal("identity should be gone")
	}
}

func TestProviderSignupAndConflicts(t *testing.T) {
	idp := newFakeIdP(t)
	ts, admin, st := newAuthServer(t)
	ctx := context.Background()
	signup(t, ts, admin, "root")
	addProvider(t, ts, admin, idp, true)

	r := idp.signInVia(t, ts, noRedirectClient(), "idp")
	if r.StatusCode != 303 || r.Header.Get("Location") != "/u/2" {
		t.Fatalf("signup: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	u, err := st.GetUser(ctx, 2)
	if err != nil || u.Name != "User sub-1" || u.Email != "sub-1@example.com" || u.IsAdmin() {
		t.Fatalf("new user: %+v %v", u, err)
	}
	// a second account may share the name, but not the address
	idp.mu.Lock()
	idp.subject = "sub-1b"
	idp.mu.Unlock()
	if err := st.UpdateUserProfile(ctx, 2, "User sub-1b", "sub-1@example.com", nil); err != nil {
		t.Fatal(err)
	}
	if r := idp.signInVia(t, ts, noRedirectClient(), "idp"); r.Header.Get("Location") != "/u/3" {
		t.Fatalf("same name: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if u, _ := st.GetUser(ctx, 3); u.Name != "User sub-1b" || u.Email != "sub-1b@example.com" {
		t.Fatalf("user: %+v", u)
	}
	// an address already in use is refused, never linked automatically
	if err := st.UpdateUserProfile(ctx, 1, "root", "sub-2@example.com", nil); err != nil {
		t.Fatal(err)
	}
	idp.mu.Lock()
	idp.subject = "sub-2"
	idp.mu.Unlock()
	if r := idp.signInVia(t, ts, noRedirectClient(), "idp"); r.StatusCode != 409 {
		t.Fatalf("email clash: %d", r.StatusCode)
	}
	if _, err := st.GetIdentity(ctx, "idp", "sub-2"); err == nil {
		t.Fatal("identity must not be created")
	}
	// a login already linked to someone else cannot be linked again
	idp.mu.Lock()
	idp.subject = "sub-1"
	idp.mu.Unlock()
	if r := idp.signInVia(t, ts, admin, "idp"); !strings.Contains(r.Header.Get("Location"), "already+linked") {
		t.Fatalf("conflict: %s", r.Header.Get("Location"))
	}
}

func TestSignupToggle(t *testing.T) {
	idp := newFakeIdP(t)
	ts, admin, st := newAuthServer(t)
	ctx := context.Background()
	signup(t, ts, admin, "root")
	addProvider(t, ts, admin, idp, true)
	form := url.Values{"name": {"new"}, "email": {"new@example.com"}, "password": {testPassword}, "confirm": {testPassword}}

	bob := noRedirectClient()
	signup(t, ts, bob, "bob")
	if r := post(t, bob, ts.URL+"/admin/signup", url.Values{"enabled": {"0"}}); r.StatusCode != 404 {
		t.Fatalf("non-admin toggle: %d", r.StatusCode)
	}
	if r := post(t, bob, ts.URL+"/admin/users", url.Values{"name": {"x"}, "email": {"x@example.com"}, "password": {testPassword}}); r.StatusCode != 404 {
		t.Fatalf("non-admin add: %d", r.StatusCode)
	}

	post(t, admin, ts.URL+"/admin/signup", url.Values{"enabled": {"0"}})
	anon := noRedirectClient()
	if _, b := body(t, anon, ts.URL+"/"); strings.Contains(b, `action="/signup"`) || !strings.Contains(b, `action="/login"`) {
		t.Fatal("sign-up form must be hidden and sign-in kept")
	}
	if r := post(t, anon, ts.URL+"/signup", form); r.StatusCode != 403 {
		t.Fatalf("closed signup: %d", r.StatusCode)
	}
	if r := idp.signInVia(t, ts, noRedirectClient(), "idp"); r.StatusCode != 403 {
		t.Fatalf("closed provider signup: %d", r.StatusCode)
	}
	if r := post(t, noRedirectClient(), ts.URL+"/login", url.Values{"email": {"bob@example.com"}, "password": {testPassword}}); r.StatusCode != 303 {
		t.Fatalf("existing user login: %d", r.StatusCode)
	}

	// an administrator can still add people
	r := post(t, admin, ts.URL+"/admin/users", url.Values{"name": {"carol"}, "email": {"carol@example.com"}, "password": {testPassword}})
	if !strings.Contains(r.Header.Get("Location"), "added") {
		t.Fatalf("add user: %s", r.Header.Get("Location"))
	}
	if u, err := st.GetUserByEmail(ctx, "Carol@Example.com"); err != nil || u.IsAdmin() {
		t.Fatalf("carol: %+v %v", u, err)
	}
	if r := post(t, noRedirectClient(), ts.URL+"/login", url.Values{"email": {"carol@example.com"}, "password": {testPassword}}); r.StatusCode != 303 {
		t.Fatalf("added user login: %d", r.StatusCode)
	}
	if r := post(t, admin, ts.URL+"/admin/users", url.Values{"name": {"carol"}, "email": {"carol@example.com"}, "password": {testPassword}}); !strings.Contains(r.Header.Get("Location"), "already+exists") {
		t.Fatalf("duplicate: %s", r.Header.Get("Location"))
	}

	post(t, admin, ts.URL+"/admin/signup", url.Values{"enabled": {"1"}})
	if r := post(t, noRedirectClient(), ts.URL+"/signup", form); r.StatusCode != 303 {
		t.Fatalf("reopened signup: %d", r.StatusCode)
	}
}

func TestFirstAccountIgnoresClosedSignup(t *testing.T) {
	ts, c, st := newAuthServer(t)
	if err := st.PutSetting(context.Background(), settingSignup, valueDisabled); err != nil {
		t.Fatal(err)
	}
	signup(t, ts, c, "root")
}

func TestProviderCallbackRejectsTampering(t *testing.T) {
	idp := newFakeIdP(t)
	ts, admin, _ := newAuthServer(t)
	signup(t, ts, admin, "root")
	addProvider(t, ts, admin, idp, true)

	c := noRedirectClient()
	resp, err := c.Get(ts.URL + "/auth/idp/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	auth, _ := url.Parse(resp.Header.Get("Location"))
	state := auth.Query().Get("state")
	cookie := resp.Cookies()[0]
	call := func(query string, cks ...*http.Cookie) int {
		req, _ := http.NewRequest("GET", ts.URL+"/auth/idp/callback?"+query, nil)
		for _, ck := range cks {
			req.AddCookie(ck)
		}
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if code := call("code=x&state=" + state); code != 400 {
		t.Errorf("no cookie: %d", code)
	}
	if code := call("code=x&state=wrong", cookie); code != 400 {
		t.Errorf("wrong state: %d", code)
	}
	tampered := *cookie
	tampered.Value = "x" + cookie.Value
	if code := call("code=x&state="+state, &tampered); code != 400 {
		t.Errorf("tampered cookie: %d", code)
	}
	if code := call("error=access_denied&state="+state, cookie); code != 502 {
		t.Errorf("denied: %d", code)
	}
}

func TestAppleClientSecret(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	p := models.AuthProvider{ClientID: "com.example.app", AppleTeamID: "TEAM", AppleKeyID: "KID",
		ClientSecret: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))}
	now := time.Unix(1_800_000_000, 0)
	jwt, err := appleClientSecret(p, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	enc := base64.RawURLEncoding
	var header, claims map[string]any
	h, _ := enc.DecodeString(parts[0])
	c, _ := enc.DecodeString(parts[1])
	json.Unmarshal(h, &header)
	json.Unmarshal(c, &claims)
	if header["alg"] != "ES256" || header["kid"] != "KID" || claims["iss"] != "TEAM" || claims["sub"] != "com.example.app" || claims["aud"] != appleIssuer {
		t.Fatalf("jwt: %v %v", header, claims)
	}
	sig, _ := enc.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(&key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("signature does not verify")
	}
	if _, err := appleClientSecret(models.AuthProvider{ClientSecret: "nope"}, now); err == nil {
		t.Fatal("bad key accepted")
	}
}
