package mainbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/store"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

func mustGet(t *testing.T, c *http.Client, u string) []byte {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

// testServers lets tests reach the Server behind a test HTTP server.
var testServers sync.Map

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
	if err := st.PutSetting(context.Background(), settingSignup, valueEnabled); err != nil { // sign-up is closed by default
		t.Fatal(err)
	}
	var ts *httptest.Server
	if splitTests() {
		ts = newSplitServer(t, st, wi)
	} else {
		srv, err := New(st, importer.NewRegistry(wi))
		if err != nil {
			t.Fatal(err)
		}
		ts = httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		testServers.Store(ts, srv)
	}
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
	resp := post(t, c, ts.URL+"/signup", url.Values{"name": {name}, "email": {name + "@example.com"}, "password": {testPassword}, "confirm": {testPassword}})
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
	post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T07:30"}, "weight_kg": {"82"}, "fat_ratio_pct": {"20"}})
	post(t, c, ts.URL+"/u/1/dexa", url.Values{"measured_at": {"2026-03-01"}, "weight_kg": {"80"}, "fat_ratio_pct": {"18"}})
	post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-02T07:30"}, "weight_kg": {"82"}, "fat_ratio_pct": {"20"}})
	_, b := body(t, c, ts.URL+"/u/1/estimates?range=all")
	if strings.Contains(b, ">est</span>") {
		t.Error("no value is interpolated yet, but a row is tagged")
	}
	post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-04T07:30"}, "weight_kg": {"82"}, "fat_ratio_pct": {"20"}})
	_, b = body(t, c, ts.URL+"/u/1/estimates?range=all")
	// DEXA day shows body fat 18; the reading is 20, so the offset is -2 and the following days
	// (a measured one, then held up to today) are corrected to 18 as well. Weight has no offset.
	for _, want := range []string{"18.00", "2026-03-02", "Body fat -2.00 %", "Smart scale", "held", "corr.", ">est</span>", `id="overview"`, `class="tile"`} {
		if !strings.Contains(b, want) {
			t.Errorf("estimates page missing %q", want)
		}
	}
	if strings.Contains(b, "Weight -") || strings.Contains(b, "Weight +") || strings.Contains(b, "Fat mass -") {
		t.Error("only body fat and bone mass may have an offset")
	}
	if code, _ := body(t, c, ts.URL+"/u/1"); code != 303 {
		t.Errorf("home redirect: %d", code)
	}
}

func TestWithingsNotSetUp(t *testing.T) {
	ts, c := newTestServer(t)
	signup(t, ts, c, "bob")
	if _, b := body(t, c, ts.URL+"/u/1/settings"); !strings.Contains(b, "Not set up") || strings.Contains(b, "DEXAFIX_WITHINGS") || strings.Contains(b, "/integrations/withings/connect") {
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
	if code, _ := do("POST", "/api/v1/users", `{"name":"carol","email":"carol@example.com"}`); code != 201 {
		t.Fatalf("create: %d", code)
	}
	if code, _ := do("POST", "/api/v1/users", `{"name":"carol","email":"carol@example.com"}`); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	if code, _ := do("POST", "/api/v1/users", `{"name":"no-email"}`); code != 400 {
		t.Fatalf("missing email: %d", code)
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
	_, b := body(t, c, ts.URL+"/u/1/estimates?range=all")
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
	if code, b := api("GET", "/api/v1/users/1/settings", ""); code != 200 || !strings.Contains(b, `"athlete_default":false`) || !strings.Contains(b, `"method_ranking":["hydrostatic","bodpod","caliper","scale","tape","visual-ai","manual-entry"]`) {
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

func TestMeasureFormsSuggestValues(t *testing.T) {
	ts, c := newTestServer(t)
	signup(t, ts, c, "bob")
	for _, page := range []string{"scale", "dexa"} {
		if _, b := body(t, c, ts.URL+"/u/1/"+page); !strings.Contains(b, "data-calc") || !strings.Contains(b, "/static/measure.js") {
			t.Errorf("%s form does not load the suggestions", page)
		}
	}
	if code, b := body(t, c, ts.URL+"/static/measure.js"); code != 200 || !strings.Contains(b, "fat_ratio_pct") {
		t.Errorf("measure.js: %d", code)
	}
}

func TestDailyTableFollowsChartRange(t *testing.T) {
	ts, c := newTestServer(t)
	signup(t, ts, c, "bob")
	post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T07:30"}, "weight_kg": {"82"}})
	oldRow := "<td>2026-03-01</td>"
	for _, tt := range []struct {
		query string
		rows  int
		old   bool
	}{{"", 90, false}, {"?range=30", 30, false}} {
		_, b := body(t, c, ts.URL+"/u/1/estimates"+tt.query)
		if got := strings.Count(b, "<tr><td>"); got != tt.rows || strings.Contains(b, oldRow) != tt.old {
			t.Errorf("%q: %d rows, want %d; old row shown: %v", tt.query, got, tt.rows, strings.Contains(b, oldRow))
		}
	}
	if _, b := body(t, c, ts.URL+"/u/1/estimates?range=all"); !strings.Contains(b, oldRow) || !strings.Contains(b, `id="estimates-data"`) {
		t.Error("all time must show every day")
	}
}

func TestEstimatesShowBodyIndices(t *testing.T) {
	ts, c := newTestServer(t)
	signup(t, ts, c, "bob")
	post(t, c, ts.URL+"/u/1/profile", url.Values{"name": {"bob"}, "email": {"bob@example.com"}, "height_cm": {"200"}})
	post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T07:30"}, "weight_kg": {"100"}})
	_, b := body(t, c, ts.URL+"/u/1/estimates?range=all")
	for _, want := range []string{">BMI <small>", ">FMI <small>", ">LMI <small>", ">A/G ratio<", ">25.00"} {
		if !strings.Contains(b, want) {
			t.Errorf("estimates page missing %q", want)
		}
	}
}

func TestDailyTableRemembersHiddenColumns(t *testing.T) {
	ts, c := newTestServer(t)
	signup(t, ts, c, "bob")
	post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T07:30"}, "weight_kg": {"82"}, "fat_ratio_pct": {"20"}})
	if _, b := body(t, c, ts.URL+"/u/1/estimates"); !strings.Contains(b, "<th>Body fat") || !strings.Contains(b, `value="fat_ratio_pct" checked`) {
		t.Fatal("every column starts out visible")
	}
	r := post(t, c, ts.URL+"/u/1/estimates/columns", url.Values{"column": {"weight_kg", "bogus"}, "range": {"30"}})
	if loc := r.Header.Get("Location"); r.StatusCode != 303 || loc != "/u/1/estimates?range=30" {
		t.Fatalf("save: %d %q", r.StatusCode, loc)
	}
	_, b := body(t, c, ts.URL+"/u/1/estimates")
	if !strings.Contains(b, "<th>Weight") || strings.Contains(b, "<th>Body fat") {
		t.Error("the hidden column must be gone, the visible one kept")
	}
	if !strings.Contains(b, `value="weight_kg" checked`) || strings.Contains(b, `value="fat_ratio_pct" checked`) || !strings.Contains(b, `value="fat_ratio_pct"`) {
		t.Error("the picker must still list hidden columns, unchecked")
	}
	if !strings.Contains(b, `id="daily-table"`) {
		t.Error("the picker needs the table to swap")
	}
}

// fakeSL is a minimal my.strengthlevel.com: one account (a@b.c / pw) with a few entries.
type fakeSL struct {
	mu      sync.Mutex
	entries map[string]map[string]any // by date
	posts   []map[string]any
	puts    []map[string]any
}

func (f *fakeSL) count() (posts, puts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts), len(f.puts)
}

func fakeStrengthLevel(t *testing.T, entries map[string]map[string]any) *fakeSL {
	t.Helper()
	const uid = "019474c6-b1b1-7299-bd69-57017308050a"
	f := &fakeSL{entries: entries}
	in := func(r *http.Request) bool { c, err := r.Cookie("sl"); return err == nil && c.Value == "ok" }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<input type="hidden" name="csrf_token" value="tok">`))
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostForm.Get("email") != "a@b.c" || r.PostForm.Get("password") != "pw" {
			w.Write([]byte("login failed"))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sl", Value: "ok", Path: "/"})
		http.Redirect(w, r, "/home", http.StatusFound)
	})
	mux.HandleFunc("GET /home", func(w http.ResponseWriter, r *http.Request) {
		if !in(r) {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`<meta name="csrf-token" content="page"><script>x={"otherUserViewModel":{"id":"` + uid + `"}}</script>`))
	})
	mux.HandleFunc("GET /api/bodyweights", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		data := []map[string]any{}
		for _, e := range f.entries {
			data = append(data, e)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	mux.HandleFunc("POST /api/bodyweights", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.posts = append(f.posts, b)
		b["id"] = "n" + b["date"].(string)
		f.entries[b["date"].(string)] = b
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"data":{}}`))
	})
	mux.HandleFunc("PUT /api/bodyweights/{id}", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		defer f.mu.Unlock()
		for date, e := range f.entries {
			if e["id"] == r.PathValue("id") {
				b["id"] = e["id"]
				f.entries[date] = b
				f.puts = append(f.puts, b)
				w.Write([]byte(`{"data":{}}`))
				return
			}
		}
		http.NotFound(w, r)
	})
	ts := httptest.NewServer(mux)
	old := strengthLevelBase
	strengthLevelBase = ts.URL
	t.Cleanup(func() { ts.Close(); strengthLevelBase = old })
	return f
}

// waitForSync waits until the settings page reports a finished background sync.
func waitForSync(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		_, b := body(t, c, base+"/u/2/settings")
		if strings.Contains(b, "Last sync") || strings.Contains(b, "The last sync stopped") {
			return b
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the background sync did not finish")
	return ""
}

func TestStrengthLevel(t *testing.T) {
	day := func(ago int) string { return time.Now().UTC().AddDate(0, 0, -ago).Format("2006-01-02") }
	fake := fakeStrengthLevel(t, map[string]map[string]any{
		day(0): {"id": "e0", "date": day(0), "bodyweight": 80.0},                                               // same as our estimate
		day(1): {"id": "e1", "date": day(1), "bodyweight": 70.0, "bodyfat": 12.0, "bodyfat_method": "caliper"}, // other value
	})
	strengthLevelDelay = 0
	ts, c, st := newServerAndStore(t, withings.New(withings.Config{}, nil))
	signup(t, ts, c, "alice") // first account is the administrator
	bob := &http.Client{CheckRedirect: c.CheckRedirect}
	signup(t, ts, bob, "bob")
	login := url.Values{"email": {"a@b.c"}, "password": {"pw"}}
	stored := func() error { _, err := st.GetIntegration(context.Background(), 2, "strengthlevel"); return err }
	admin := func(path string, on string) *http.Response {
		return post(t, c, ts.URL+"/admin/users/2/"+path, url.Values{"enabled": {on}})
	}

	// Off by default: no settings card, and saving a login, syncing or sending a day is refused.
	if _, b := body(t, bob, ts.URL+"/u/2/settings"); strings.Contains(b, `id="strengthlevel"`) {
		t.Fatal("card shown without the flag")
	}
	for _, p := range []string{"login", "sync", "day"} {
		if r := post(t, bob, ts.URL+"/u/2/strengthlevel/"+p, login); r.StatusCode != 403 && !(p == "day" && r.StatusCode == 403) {
			t.Fatalf("%s without flag: %d", p, r.StatusCode)
		}
	}
	// Only an administrator can set the switches; the multi-day one needs the first.
	if r := post(t, bob, ts.URL+"/admin/users/2/strengthlevel", url.Values{"enabled": {"1"}}); r.StatusCode != 404 {
		t.Fatalf("non-admin enable: %d", r.StatusCode)
	}
	admin("strengthlevel-bulk", "1")
	if _, b := body(t, c, ts.URL+"/admin"); strings.Contains(b, "Disallow multi-day sync") {
		t.Fatal("bulk switch set without the first switch")
	}
	if r := admin("strengthlevel", "1"); r.StatusCode != 303 {
		t.Fatalf("admin enable: %d", r.StatusCode)
	}
	if _, b := body(t, bob, ts.URL+"/u/2/settings"); !strings.Contains(b, `name="email"`) || strings.Contains(b, `name="days"`) {
		t.Fatalf("card without the multi-day form expected:\n%s", b)
	}
	if r := post(t, bob, ts.URL+"/u/2/strengthlevel/sync", nil); r.StatusCode != 403 {
		t.Fatalf("multi-day sync without its switch: %d", r.StatusCode)
	}

	// Incomplete or wrong logins are not stored.
	post(t, bob, ts.URL+"/u/2/strengthlevel/login", url.Values{"email": {"a@b.c"}})
	post(t, bob, ts.URL+"/u/2/strengthlevel/login", url.Values{"email": {"a@b.c"}, "password": {"wrong"}})
	if err := stored(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unusable login was stored: %v", err)
	}
	post(t, bob, ts.URL+"/u/2/strengthlevel/login", login)
	if _, b := body(t, bob, ts.URL+"/u/2/settings"); !strings.Contains(b, "a@b.c") || strings.Contains(b, `value="pw"`) {
		t.Fatalf("saved view must show the e-mail address and never the password:\n%s", b)
	}

	// Readings: today 80, yesterday 79.4, 200 days ago 90. Days between are interpolated.
	for _, d := range []struct {
		ago int
		kg  string
	}{{0, "80"}, {1, "79.4"}, {200, "90"}} {
		post(t, bob, ts.URL+"/u/2/scale", url.Values{"measured_at": {day(d.ago) + "T07:30"}, "weight_kg": {d.kg}})
	}

	// The estimates page offers a button per day, which overwrites the day on Strength Level.
	if _, b := body(t, bob, ts.URL+"/u/2/estimates"); !strings.Contains(b, `action="/u/2/strengthlevel/day"`) {
		t.Fatal("no single-day button on the estimates page")
	}
	r := post(t, bob, ts.URL+"/u/2/strengthlevel/day", url.Values{"day": {day(1)}})
	if r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), "overwritten") {
		t.Fatalf("send one day: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if _, puts := fake.count(); puts != 1 || fake.puts[0]["bodyweight"] != 79.4 || fake.puts[0]["bodyfat"] != 12.0 || fake.puts[0]["bodyfat_method"] != "caliper" {
		t.Fatalf("overwrite keeps the site's body fat and method: %+v", fake.puts)
	}
	r = post(t, bob, ts.URL+"/u/2/strengthlevel/day", url.Values{"day": {day(0)}})
	if !strings.Contains(r.Header.Get("Location"), "already+up+to+date") {
		t.Fatalf("same values: %s", r.Header.Get("Location"))
	}
	r = post(t, bob, ts.URL+"/u/2/strengthlevel/day", url.Values{"day": {day(5)}}) // interpolated, missing there
	if !strings.Contains(r.Header.Get("Location"), "added") {
		t.Fatalf("interpolated day: %s", r.Header.Get("Location"))
	}
	if r := post(t, bob, ts.URL+"/u/2/strengthlevel/day", url.Values{"day": {"nope"}}); r.StatusCode != 400 {
		t.Fatalf("bad day: %d", r.StatusCode)
	}
	if r := post(t, bob, ts.URL+"/u/2/strengthlevel/day", url.Values{"day": {day(300)}}); !strings.Contains(r.Header.Get("Location"), "no+weight+estimate") {
		t.Fatalf("day without estimate: %s", r.Header.Get("Location"))
	}

	// The multi-day sync runs in the background once the administrator allows it.
	admin("strengthlevel-bulk", "1")
	if _, b := body(t, bob, ts.URL+"/u/2/settings"); !strings.Contains(b, `name="days"`) {
		t.Fatal("multi-day form missing after allowing it")
	}
	before, _ := fake.count()
	r = post(t, bob, ts.URL+"/u/2/strengthlevel/sync", url.Values{"days": {"10"}})
	if r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), "started+in+the+background") {
		t.Fatalf("sync: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	page := waitForSync(t, bob, ts.URL)
	// Days 0..10: today and yesterday exist (and are left alone), day 5 was added above: 8 more.
	if posts, puts := fake.count(); posts-before != 8 || puts != 1 || !strings.Contains(page, "added 8, updated 0, 3 already up to date") {
		t.Fatalf("add-only sync: %d new posts, %d puts\n%s", posts-before, puts, page)
	}
	if r := post(t, bob, ts.URL+"/u/2/strengthlevel/sync", url.Values{"days": {"0"}}); !strings.Contains(r.Header.Get("Location"), "between+1+and+3650") {
		t.Fatalf("bad days: %s", r.Header.Get("Location"))
	}

	// With the update box ticked, days that exist with other values are overwritten too.
	fake.mu.Lock()
	fake.entries[day(2)]["bodyweight"] = 1.0
	fake.mu.Unlock()
	post(t, bob, ts.URL+"/u/2/strengthlevel/sync", url.Values{"days": {"10"}, "update": {"1"}})
	for i := 0; i < 100; i++ { // the previous result is still shown until the new job has finished
		_, b := body(t, bob, ts.URL+"/u/2/settings")
		if strings.Contains(b, "updating existing days") {
			page = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(page, "added 0, updated 1,") {
		t.Fatalf("update sync:\n%s", page)
	}

	// Taking the permissions away deletes the login.
	admin("strengthlevel", "0")
	if err := stored(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("login kept after disabling: %v", err)
	}
	if r := post(t, bob, ts.URL+"/u/2/strengthlevel/login", login); r.StatusCode != 403 {
		t.Fatalf("save after disable: %d", r.StatusCode)
	}
	if u, _ := st.GetUser(context.Background(), 2); u.StrengthLevelAuto || u.StrengthLevelBulk {
		t.Fatalf("switches after disabling: %+v", u)
	}
}
