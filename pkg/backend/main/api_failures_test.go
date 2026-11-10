package mainbackend

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// callAs makes a public API request with a token and returns the status and the body.
func (r *jobRig) callAs(token, method, path, payload string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.pub.URL+"/api/v1"+path, strings.NewReader(payload))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// rootToken gives the administrator an access token.
func (r *jobRig) rootToken() string {
	r.t.Helper()
	if r.rootTok != "" {
		return r.rootTok
	}
	token := "dxf_" + "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	if err := r.st.CreateAccessToken(context.Background(), &models.AccessToken{UserID: 1, Name: "root", Prefix: "dxf_", Hash: hashToken(token)}); err != nil {
		r.t.Fatal(err)
	}
	r.rootTok = token
	return token
}

func TestTheUsersOfThePublicAPIAreAdministeredCleanly(t *testing.T) {
	create := func(r *jobRig, payload string) (int, string) {
		return r.callAs(r.rootToken(), "POST", "/users", payload)
	}
	t.Run("the list fails when the database does", func(t *testing.T) {
		r := newJobRig(t)
		r.flaky.failOn("ListUsers")
		if s, _ := r.callAs(r.rootToken(), "GET", "/users", ""); s != 500 {
			t.Fatalf("status %d, want 500", s)
		}
	})
	t.Run("a user with a password", func(t *testing.T) {
		r := newJobRig(t)
		if s, b := create(r, `{"name":"carol","email":"carol@example.com","password":"a long enough password"}`); s != 201 || !strings.Contains(b, "carol") {
			t.Fatalf("status %d %.200q", s, b)
		}
	})
	t.Run("a password that is too short", func(t *testing.T) {
		r := newJobRig(t)
		if s, b := create(r, `{"name":"carol","email":"carol@example.com","password":"x"}`); s != 400 || !strings.Contains(b, "assword") {
			t.Fatalf("status %d %.200q", s, b)
		}
	})
	t.Run("an address that is taken, with a password", func(t *testing.T) {
		r := newJobRig(t)
		if s, _ := create(r, `{"name":"carol","email":"carol@example.com","password":"a long enough password"}`); s != 201 {
			t.Fatal(s)
		}
		if s, _ := create(r, `{"name":"carola","email":"carol@example.com","password":"a long enough password"}`); s != 409 {
			t.Fatalf("status %d, want 409", s)
		}
	})
	t.Run("an address that is taken, without a password, leaves no account behind", func(t *testing.T) {
		r := newJobRig(t)
		if s, _ := create(r, `{"name":"carol","email":"carol@example.com"}`); s != 201 {
			t.Fatal(s)
		}
		before, _ := r.st.ListUsers(context.Background())
		if s, _ := create(r, `{"name":"carola","email":"carol@example.com"}`); s != 409 {
			t.Fatalf("status %d, want 409", s)
		}
		if after, _ := r.st.ListUsers(context.Background()); len(after) != len(before) {
			t.Fatalf("%d users, want %d: the failed account was left behind", len(after), len(before))
		}
	})
	t.Run("a name that is not a name", func(t *testing.T) {
		r := newJobRig(t)
		if s, _ := create(r, `{"name":"","email":"carol@example.com"}`); s != 400 {
			t.Fatalf("status %d, want 400", s)
		}
	})
}

func TestTheMeasurementsOfThePublicAPIFailCleanly(t *testing.T) {
	status := func(r *jobRig, method, path, payload string) int {
		s, _, _ := r.call(method, path, payload)
		return s
	}
	withoutBody := func(t *testing.T) *jobRig {
		r := newJobRig(t)
		r.b.body = bodyService{c: bodyDown(t)}
		return r
	}
	t.Run("a list with a limit that is not a number", func(t *testing.T) {
		r := newJobRig(t)
		for _, path := range []string{"/dexa-scans?limit=many", "/scale-readings?limit=many", "/scale-readings?before=nonsense", "/scale-readings?since=never", "/scale-readings?until=never"} {
			if s := status(r, "GET", path, ""); s != 400 {
				t.Errorf("%s: status %d, want 400", path, s)
			}
		}
	})
	t.Run("a list window that is a date and a time", func(t *testing.T) {
		r := newJobRig(t)
		if s := status(r, "GET", "/scale-readings?since=2026-01-01T00:00:00Z&until=2026-02-01", ""); s != 200 {
			t.Fatalf("status %d, want 200", s)
		}
	})
	t.Run("lists and estimates without the body service", func(t *testing.T) {
		r := withoutBody(t)
		for _, path := range []string{"/dexa-scans", "/scale-readings", "/estimates"} {
			if s := status(r, "GET", path, ""); s != 503 {
				t.Errorf("%s: status %d, want 503", path, s)
			}
		}
	})
	t.Run("a scan that is not valid JSON", func(t *testing.T) {
		if s := status(newJobRig(t), "POST", "/dexa-scans", "{nope"); s != 400 {
			t.Fatalf("status %d, want 400", s)
		}
	})
	t.Run("a scan with no date", func(t *testing.T) {
		if s := status(newJobRig(t), "POST", "/dexa-scans", `{"weight_kg":80}`); s != 400 {
			t.Fatalf("status %d, want 400", s)
		}
	})
	t.Run("a scan with a weight that cannot be", func(t *testing.T) {
		if s := status(newJobRig(t), "POST", "/dexa-scans", `{"measured_at":"2026-03-01T00:00:00Z","weight_kg":-4}`); s != 400 {
			t.Fatalf("status %d, want 400", s)
		}
	})
	t.Run("a scan without the body service", func(t *testing.T) {
		if s := status(withoutBody(t), "POST", "/dexa-scans", `{"measured_at":"2026-03-01T00:00:00Z","weight_kg":80}`); s != 503 {
			t.Fatalf("status %d, want 503", s)
		}
	})
	t.Run("readings that are not valid JSON", func(t *testing.T) {
		if s := status(newJobRig(t), "POST", "/scale-readings", "{nope"); s != 400 {
			t.Fatalf("status %d, want 400", s)
		}
	})
	t.Run("a batch that is too large", func(t *testing.T) {
		r := newJobRig(t)
		if s, b, _ := r.call("POST", "/scale-readings", "["+strings.Repeat("{},", maxBatchItems)+"{}]"); s != 400 || !strings.Contains(b, "batch too large") {
			t.Fatalf("status %d %.100q, want 400 batch too large", s, b)
		}
	})
	t.Run("a reading that is not valid", func(t *testing.T) {
		r := newJobRig(t)
		if s, b, _ := r.call("POST", "/scale-readings", `[{"measured_at":"2026-03-01T08:00:00Z","weight_kg":-1}]`); s != 400 || !strings.Contains(b, "item 0") {
			t.Fatalf("status %d %.100q, want 400 item 0", s, b)
		}
	})
	t.Run("a reading with its own athlete mode", func(t *testing.T) {
		r := newJobRig(t)
		if s, b, _ := r.call("POST", "/scale-readings", `[{"measured_at":"2026-03-01T08:00:00Z","weight_kg":80,"athlete_mode":true}]`); s != 200 || !strings.Contains(b, `"upserted":1`) {
			t.Fatalf("status %d %.100q", s, b)
		}
	})
	t.Run("readings without the body service", func(t *testing.T) {
		if s := status(withoutBody(t), "POST", "/scale-readings", `[{"measured_at":"2026-03-01T08:00:00Z","weight_kg":80}]`); s != 503 {
			t.Fatalf("status %d, want 503", s)
		}
	})
	t.Run("settings that are not valid JSON", func(t *testing.T) {
		if s := status(newJobRig(t), "PUT", "/settings", "{nope"); s != 400 {
			t.Fatalf("status %d, want 400", s)
		}
	})
	t.Run("a ranking with a method that does not exist", func(t *testing.T) {
		if s := status(newJobRig(t), "PUT", "/settings", `{"athlete_default":false,"method_ranking":["magic"]}`); s != 400 {
			t.Fatalf("status %d, want 400", s)
		}
	})
	t.Run("settings the database refuses", func(t *testing.T) {
		r := newJobRig(t)
		refuseWrites(t, r.st, "users", "id", "2")
		if s := status(r, "PUT", "/settings", `{"athlete_default":true,"method_ranking":[]}`); s != 500 {
			t.Fatalf("status %d, want 500", s)
		}
	})
}
