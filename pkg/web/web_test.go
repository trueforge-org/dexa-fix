package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

func newTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	return newServerWith(t, withings.New(withings.Config{}, nil))
}

func newServerWith(t *testing.T, wi *withings.Importer) (*httptest.Server, *http.Client) {
	t.Helper()
	ts, c, _ := newServerAndStore(t, wi)
	return ts, c
}

func newServerAndStore(t *testing.T, wi *withings.Importer) (*httptest.Server, *http.Client, *sqlstore.Store) {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := New(st, importer.NewRegistry(wi))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return ts, client, st
}

const testPassword = "correct horse"

// cookieTransport sends a session cookie with every request of a client.
type cookieTransport struct{ cookie *http.Cookie }

func (ct cookieTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.AddCookie(ct.cookie)
	return http.DefaultTransport.RoundTrip(r)
}

// signup creates an account and makes the client act as it.
func signup(t *testing.T, ts *httptest.Server, c *http.Client, name string) {
	t.Helper()
	resp := post(t, c, ts.URL+"/signup", url.Values{"name": {name}, "password": {testPassword}, "confirm": {testPassword}})
	if resp.StatusCode != 303 {
		t.Fatalf("signup %s: %d", name, resp.StatusCode)
	}
	useSession(t, c, resp)
}

func useSession(t *testing.T, c *http.Client, resp *http.Response) {
	t.Helper()
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie && ck.MaxAge > 0 {
			c.Transport = cookieTransport{ck}
			return
		}
	}
	t.Fatalf("no session cookie in %v", resp.Cookies())
}

func post(t *testing.T, c *http.Client, u string, form url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func body(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func TestUserAndMeasurementFlow(t *testing.T) {
	ts, c := newTestServer(t)

	signup(t, ts, c, "alice")
	if code, _ := body(t, c, ts.URL+"/"); code != 303 {
		t.Fatalf("index when signed in: %d", code)
	}

	r := post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T07:30"}, "weight_kg": {"81,5"}, "muscle_mass_kg": {"30.2"}})
	if r.StatusCode != 303 {
		t.Fatalf("scale create: %d", r.StatusCode)
	}
	if code, b := body(t, c, ts.URL+"/u/1/scale"); code != 200 || !strings.Contains(b, "81.50") || !strings.Contains(b, "2026-03-01 07:30") {
		t.Fatalf("scale page: %d\n%s", code, b)
	}
	r = post(t, c, ts.URL+"/u/1/dexa", url.Values{"measured_at": {"2026-03-02"}, "weight_kg": {"80"}, "bmd_g_cm2": {"1.25"}})
	if r.StatusCode != 303 {
		t.Fatalf("dexa create: %d", r.StatusCode)
	}
	if code, b := body(t, c, ts.URL+"/u/1/dexa"); code != 200 || !strings.Contains(b, "1.25") || !strings.Contains(b, "2026-03-02") {
		t.Fatalf("dexa page: %d", code)
	}

	// validation
	if r := post(t, c, ts.URL+"/u/1/dexa", url.Values{"measured_at": {"2026-03-02"}}); r.StatusCode != 422 {
		t.Fatalf("no values: %d", r.StatusCode)
	}
	if r := post(t, c, ts.URL+"/u/1/dexa", url.Values{"measured_at": {"nope"}, "weight_kg": {"1"}}); r.StatusCode != 422 {
		t.Fatalf("bad date: %d", r.StatusCode)
	}
	if r := post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T07:30"}, "weight_kg": {"abc"}}); r.StatusCode != 422 {
		t.Fatalf("bad number: %d", r.StatusCode)
	}

	// delete
	if r := post(t, c, ts.URL+"/u/1/scale/1/delete", nil); r.StatusCode != 303 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if r := post(t, c, ts.URL+"/u/1/scale/1/delete", nil); r.StatusCode != 404 {
		t.Fatalf("delete again: %d", r.StatusCode)
	}
}

func TestUnknownUser(t *testing.T) {
	ts, c := newTestServer(t)
	signup(t, ts, c, "alice")
	for _, p := range []string{"/u/99/dexa", "/u/99/scale", "/u/x/scale"} {
		if code, _ := body(t, c, ts.URL+p); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}
	if code, _ := body(t, c, ts.URL+"/static/app.css"); code != 200 {
		t.Errorf("static: %d", code)
	}
}

func TestEstimatesPage(t *testing.T) {
	ts, c := newTestServer(t)
	signup(t, ts, c, "bob")
	if code, b := body(t, c, ts.URL+"/u/1/estimates"); code != 200 || !strings.Contains(b, "No data yet") || strings.Contains(b, `id="overview"`) {
		t.Fatalf("empty: %d", code)
	}
	post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T07:30"}, "weight_kg": {"82"}})
	post(t, c, ts.URL+"/u/1/dexa", url.Values{"measured_at": {"2026-03-01"}, "weight_kg": {"80"}})
	post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-02T07:30"}, "weight_kg": {"82"}})
	_, b := body(t, c, ts.URL+"/u/1/estimates")
	// DEXA day shows 80; the series is a constant 82, so the offset is -2 and the following days
	// (a measured one, then held up to today) are corrected to 80 as well.
	for _, want := range []string{"80.00", "2026-03-02", "Weight -2.00 kg", "Smart scale", "held", "corr.", `id="overview"`, `class="tile"`} {
		if !strings.Contains(b, want) {
			t.Errorf("estimates page missing %q", want)
		}
	}
	if code, _ := body(t, c, ts.URL+"/u/1"); code != 303 {
		t.Errorf("home redirect: %d", code)
	}
}

func TestWithingsNotSetUp(t *testing.T) {
	ts, c := newTestServer(t)
	signup(t, ts, c, "bob")
	if _, b := body(t, c, ts.URL+"/u/1/settings"); !strings.Contains(b, "Not set up") || !strings.Contains(b, "DEXAFIX_WITHINGS_CLIENT_ID") || strings.Contains(b, "/integrations/withings/connect") {
		t.Errorf("settings should explain the missing setup:\n%s", b)
	}
	if _, b := body(t, c, ts.URL+"/u/1/estimates"); !strings.Contains(b, "Set up Withings") || strings.Contains(b, "/integrations/withings/connect") {
		t.Errorf("estimates should link to the setup:\n%s", b)
	}
}

func TestAPI(t *testing.T) {
	ts, c := newTestServer(t)
	do := func(method, path, payload string) (int, string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(payload))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, sb.String()
	}
	if code, _ := do("GET", "/api/v1/users", ""); code != 401 {
		t.Fatalf("users without sign-in: %d", code)
	}
	signup(t, ts, c, "admin")
	if code, b := do("GET", "/api/v1/users", ""); code != 200 || !strings.Contains(b, `"name":"admin"`) || !strings.Contains(b, `"role":"admin"`) {
		t.Fatalf("users: %d %s", code, b)
	}
	if code, _ := do("POST", "/api/v1/users", `{"name":"carol"}`); code != 201 {
		t.Fatalf("create: %d", code)
	}
	if code, _ := do("POST", "/api/v1/users", `{"name":"carol"}`); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	if code, _ := do("POST", "/api/v1/users", `{"nope":1}`); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}
	batch := `[{"measured_at":"2026-03-01T07:30:00Z","weight_kg":81,"source":"withings","external_id":"g1"}]`
	for i := 0; i < 2; i++ { // idempotent
		if code, b := do("POST", "/api/v1/users/1/scale-readings", batch); code != 200 || !strings.Contains(b, `"upserted":1`) {
			t.Fatalf("batch: %d %s", code, b)
		}
	}
	if _, b := do("GET", "/api/v1/users/1/scale-readings", ""); strings.Count(b, `"external_id":"g1"`) != 1 {
		t.Fatalf("expected one row: %s", b)
	}
	if code, _ := do("POST", "/api/v1/users/1/dexa-scans", `{"weight_kg":80}`); code != 400 {
		t.Fatalf("dexa without date: %d", code)
	}
	if code, _ := do("POST", "/api/v1/users/1/dexa-scans", `{"measured_at":"2026-03-01T00:00:00Z","weight_kg":80}`); code != 201 {
		t.Fatalf("dexa create: %d", code)
	}
	if code, b := do("GET", "/api/v1/users/1/estimates", ""); code != 200 || !strings.Contains(b, `"origin":"dexa"`) {
		t.Fatalf("estimates: %d %s", code, b)
	}
	if code, _ := do("GET", "/api/v1/users/9/estimates", ""); code != 404 {
		t.Fatalf("unknown user: %d", code)
	}
	if code, b := do("POST", "/api/v1/users/1/import/withings", ""); code != 501 || !strings.Contains(b, "not configured") {
		t.Fatalf("withings import: %d %s", code, b)
	}
	if code, _ := do("POST", "/api/v1/users/1/import/nope", ""); code != 404 {
		t.Fatalf("unknown provider: %d", code)
	}
	if code, _ := do("POST", "/api/v1/users/1/import/withings?since=yesterday", ""); code != 400 {
		t.Fatalf("bad since: %d", code)
	}
	if code, _ := do("POST", "/api/v1/users/1/import/withings?day=tomorrow", ""); code != 400 {
		t.Fatalf("bad day: %d", code)
	}
	if code, _ := do("POST", "/api/v1/users/9/import/withings", ""); code != 404 {
		t.Fatalf("unknown user import: %d", code)
	}
	for _, p := range []string{"/u/1/integrations/withings/connect", "/integrations/withings/callback"} {
		if code, _ := do("GET", p, ""); code != 404 { // unconfigured: no OAuth flow
			t.Fatalf("%s: %d", p, code)
		}
	}
	if code, _ := do("GET", "/u/1/integrations/nope/connect", ""); code != 404 {
		t.Fatalf("unknown provider connect: %d", code)
	}
	if code, _ := do("GET", "/healthz", ""); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
}

// fakeWithingsAPI answers the token and getmeas endpoints with two weigh-ins.
func fakeWithingsAPI(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/oauth2":
			w.Write([]byte(`{"status":0,"body":{"userid":1,"access_token":"at","refresh_token":"rt","expires_in":10800}}`))
		case "/measure":
			r.ParseForm()
			start, _ := strconv.ParseInt(r.PostForm.Get("startdate"), 10, 64)
			end, err := strconv.ParseInt(r.PostForm.Get("enddate"), 10, 64)
			if err != nil {
				end = 1 << 40
			}
			// 2026-03-01 and 2026-03-02 at 07:00 UTC
			var out []string
			for i, date := range []int64{1772348400, 1772434800} {
				if date >= start && date <= end {
					out = append(out, fmt.Sprintf(`{"grpid":%d,"category":1,"date":%d,"measures":[{"value":%d,"type":1,"unit":-3}]}`, i+1, date, 80500-int64(i)*500))
				}
			}
			fmt.Fprintf(w, `{"status":0,"body":{"more":0,"offset":0,"measuregrps":[%s]}}`, strings.Join(out, ","))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestWithingsFlow(t *testing.T) {
	api := fakeWithingsAPI(t)
	ts, c := newServerWith(t, withings.New(withings.Config{ClientID: "id", ClientSecret: "s", RedirectURL: "http://app/integrations/withings/callback", APIBase: api.URL}, nil))
	signup(t, ts, c, "dora")

	flash := func(resp *http.Response) string {
		t.Helper()
		if resp.StatusCode != 303 {
			t.Fatalf("want redirect, got %d", resp.StatusCode)
		}
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || loc.Path != "/u/1/estimates" {
			t.Fatalf("bad location %q", resp.Header.Get("Location"))
		}
		return loc.Query().Get("msg")
	}

	if _, b := body(t, c, ts.URL+"/u/1/estimates"); !strings.Contains(b, "Connect Withings") || strings.Contains(b, "Import from Withings") {
		t.Fatalf("expected connect link:\n%s", b)
	}
	if _, b := body(t, c, ts.URL+"/u/1/settings"); !strings.Contains(b, "Not connected") || !strings.Contains(b, "/u/1/integrations/withings/connect") {
		t.Fatalf("expected connect link in settings:\n%s", b)
	}
	// not connected yet
	if msg := flash(post(t, c, ts.URL+"/u/1/import/withings", nil)); !strings.Contains(msg, "not connected") {
		t.Fatalf("msg %q", msg)
	}
	if code, _ := func() (int, string) {
		resp := post(t, c, ts.URL+"/api/v1/users/1/import/withings", nil)
		return resp.StatusCode, ""
	}(); code != 409 {
		t.Fatalf("api not connected: %d", code)
	}

	// connect: redirect to Withings with a state that is also kept in a cookie
	resp, err := c.Get(ts.URL + "/u/1/integrations/withings/connect")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	auth, _ := url.Parse(resp.Header.Get("Location"))
	state := auth.Query().Get("state")
	if resp.StatusCode != 303 || auth.Host != "account.withings.com" || len(state) < 16 || auth.Query().Get("client_id") != "id" {
		t.Fatalf("connect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != "oauth_state_withings" || cookies[0].Value != "1."+state || !cookies[0].HttpOnly {
		t.Fatalf("state cookie: %+v", cookies)
	}

	callback := func(query string, cs ...*http.Cookie) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/integrations/withings/callback?"+query, nil)
		for _, ck := range cs {
			req.AddCookie(ck)
		}
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r
	}
	if r := callback("code=x&state=" + state); r.StatusCode != 400 {
		t.Fatalf("no cookie: %d", r.StatusCode)
	}
	if r := callback("code=x&state=wrong", cookies[0]); r.StatusCode != 400 {
		t.Fatalf("wrong state: %d", r.StatusCode)
	}
	if msg := flash(callback("error=access_denied&state="+state, cookies[0])); !strings.Contains(msg, "not authorised") {
		t.Fatalf("denied: %q", msg)
	}
	if msg := flash(callback("code=abc&state="+state, cookies[0])); !strings.Contains(msg, "connected") {
		t.Fatalf("connected: %q", msg)
	}

	if _, b := body(t, c, ts.URL+"/u/1/estimates"); !strings.Contains(b, "Import from Withings") || strings.Contains(b, "Connect Withings") {
		t.Fatalf("expected import button:\n%s", b)
	}
	if _, b := body(t, c, ts.URL+"/u/1/settings"); !strings.Contains(b, "Import from Withings") || !strings.Contains(b, "Disconnect Withings") || strings.Contains(b, "/integrations/withings/connect") {
		t.Fatalf("expected import and disconnect in settings:\n%s", b)
	}
	// a manual reading on the first day must be protected from the bulk import
	post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T09:00"}, "weight_kg": {"81"}})
	if msg := flash(post(t, c, ts.URL+"/u/1/import/withings", nil)); !strings.Contains(msg, "1 imported") || !strings.Contains(msg, "1 day(s) skipped") || !strings.Contains(msg, "2026-03-01") {
		t.Fatalf("bulk msg %q", msg)
	}
	_, b := body(t, c, ts.URL+"/u/1/estimates")
	for _, want := range []string{`name="day" value="2026-03-01"`, `name="day" value="2026-03-02"`, "Re-import this day from Withings", "80.00"} {
		if !strings.Contains(b, want) {
			t.Errorf("estimates page missing %q", want)
		}
	}

	// the per-day button re-imports the protected day on request
	if msg := flash(post(t, c, ts.URL+"/u/1/import/withings", url.Values{"day": {"2026-03-01"}})); !strings.Contains(msg, "for 2026-03-01") || !strings.Contains(msg, "1 imported") {
		t.Fatalf("day msg %q", msg)
	}
	if msg := flash(post(t, c, ts.URL+"/u/1/import/withings", url.Values{"day": {"2026-03-02"}})); !strings.Contains(msg, "1 unchanged") {
		t.Fatalf("unchanged msg %q", msg)
	}
	if msg := flash(post(t, c, ts.URL+"/u/1/import/withings", url.Values{"day": {"nope"}})); !strings.Contains(msg, "day must be") {
		t.Fatalf("bad day msg %q", msg)
	}
	if code, b := body(t, c, ts.URL+"/u/1/estimates?msg=hello+there"); code != 200 || !strings.Contains(b, "hello there") {
		t.Fatalf("flash not shown: %d", code)
	}

	if msg := flash(post(t, c, ts.URL+"/u/1/integrations/withings/disconnect", nil)); !strings.Contains(msg, "disconnected") {
		t.Fatalf("disconnect: %q", msg)
	}
	if _, b := body(t, c, ts.URL+"/u/1/estimates"); strings.Contains(b, "Re-import this day") || !strings.Contains(b, "Connect Withings") {
		t.Fatalf("buttons must be gone after disconnect")
	}
}

func TestMethodsAthleteModeAndSettings(t *testing.T) {
	ts, c := newTestServer(t)
	signup(t, ts, c, "carol")
	api := func(method, path, payload string) (int, string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(payload))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// settings: defaults, then save
	if code, b := api("GET", "/api/v1/users/1/settings", ""); code != 200 || !strings.Contains(b, `"athlete_default":false`) || !strings.Contains(b, `"method_ranking":["hydrostatic","bodpod","caliper","scale","tape","visual-ai"]`) {
		t.Fatalf("default settings: %d %s", code, b)
	}
	if r := post(t, c, ts.URL+"/u/1/settings", url.Values{"athlete_default": {"1"}, "rank_visual-ai": {"1"}, "rank_hydrostatic": {"2"}}); r.StatusCode != 200 {
		t.Fatalf("save settings: %d", r.StatusCode)
	}
	if code, b := api("GET", "/api/v1/users/1/settings", ""); code != 200 || !strings.Contains(b, `"athlete_default":true`) || !strings.Contains(b, `"method_ranking":["visual-ai","hydrostatic"`) {
		t.Fatalf("saved settings: %d %s", code, b)
	}
	if code, b := body(t, c, ts.URL+"/u/1/settings"); code != 200 || !strings.Contains(b, "Athlete mode by default") || !strings.Contains(b, "checked") {
		t.Fatalf("settings page: %d", code)
	}
	if r := post(t, c, ts.URL+"/u/1/settings", url.Values{"rank_scale": {"zero"}}); r.StatusCode != 422 {
		t.Fatalf("bad rank: %d", r.StatusCode)
	}
	if code, _ := api("PUT", "/api/v1/users/1/settings", `{"athlete_default":false,"method_ranking":["nope"]}`); code != 400 {
		t.Fatalf("unknown method: %d", code)
	}
	if code, b := api("PUT", "/api/v1/users/1/settings", `{"athlete_default":false,"method_ranking":["scale"]}`); code != 200 || !strings.Contains(b, `"method_ranking":["scale","hydrostatic"`) {
		t.Fatalf("put settings: %d %s", code, b)
	}
	api("PUT", "/api/v1/users/1/settings", `{"athlete_default":true,"method_ranking":[]}`)

	// scale form: dropdown, athlete checkbox preset from the user default
	if _, b := body(t, c, ts.URL+"/u/1/scale"); !strings.Contains(b, `<option value="caliper">`) || !strings.Contains(b, `name="athlete_mode" value="1" checked`) {
		t.Fatalf("scale form lacks method/athlete controls:\n%s", b)
	}
	if r := post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T07:30"}, "weight_kg": {"81"}, "method": {"caliper"}, "athlete_mode": {"1"}}); r.StatusCode != 303 {
		t.Fatalf("scale with method: %d", r.StatusCode)
	}
	if _, b := body(t, c, ts.URL+"/u/1/scale"); !strings.Contains(b, "Skinfold caliper, athlete") {
		t.Fatalf("scale list lacks method tag:\n%s", b)
	}
	if r := post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T07:30"}, "weight_kg": {"81"}, "method": {"nope"}}); r.StatusCode != 422 {
		t.Fatalf("bad method: %d", r.StatusCode)
	}

	// API: athlete_mode falls back to the user default unless given
	batch := `[{"measured_at":"2026-03-05T07:00:00Z","weight_kg":80,"method":"tape"},{"measured_at":"2026-03-06T07:00:00Z","weight_kg":80,"athlete_mode":false}]`
	if code, b := api("POST", "/api/v1/users/1/scale-readings", batch); code != 200 {
		t.Fatalf("post readings: %d %s", code, b)
	}
	_, b := api("GET", "/api/v1/users/1/scale-readings", "")
	if !strings.Contains(b, `"method":"tape","athlete_mode":true`) || !strings.Contains(b, `"method":"scale","athlete_mode":false`) {
		t.Fatalf("readings: %s", b)
	}
	if code, _ := api("POST", "/api/v1/users/1/scale-readings", `[{"measured_at":"2026-03-07T07:00:00Z","weight_kg":80,"method":"nope"}]`); code != 400 {
		t.Fatalf("unknown method via API: %d", code)
	}
}
