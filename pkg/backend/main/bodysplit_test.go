package mainbackend

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bodybackend "github.com/trueforge-org/dexa-fix/pkg/backend/body"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
	"github.com/trueforge-org/dexa-fix/pkg/workers/builtin"
)

// The body service as a process of its own: the main backend on a database that has no scans or
// readings, the body service on a database that has no users, a key of its own, and a real TCP
// connection between them.

func countTables(t *testing.T, path string, names ...string) map[string]bool {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got := map[string]bool{}
	for _, n := range names {
		var c int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, n).Scan(&c); err != nil {
			t.Fatal(err)
		}
		got[n] = c == 1
	}
	return got
}

func TestBodyServiceInItsOwnDatabase(t *testing.T) {
	if splitTests() {
		t.Skip("builds its own split")
	}
	ctx := context.Background()
	dir := t.TempDir()
	mainPath, bodyPath := filepath.Join(dir, "main.db"), filepath.Join(dir, "body.db")
	mainKey, _ := sqlstore.NewCipher(bytes.Repeat([]byte{1}, sqlstore.KeySize))
	bodyKey, _ := sqlstore.NewCipher(bytes.Repeat([]byte{2}, sqlstore.KeySize)) // a key of its own

	mainSt, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, "file:"+mainPath, sqlstore.WithoutBody(), sqlstore.WithCipher(mainKey))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mainSt.Close() })
	if err := mainSt.PutSetting(ctx, settingSignup, valueEnabled); err != nil {
		t.Fatal(err)
	}
	bodySt, err := sqlstore.OpenBody(ctx, sqlstore.DriverSQLite, "file:"+bodyPath, sqlstore.WithCipher(bodyKey))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bodySt.Close() })

	_, pub, priv, err := bodyapi.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	const secret = "the-body-secret-of-this-test-0123456789"
	svc, err := bodybackend.New(bodySt, secret, bodyapi.NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	bodyTS := httptest.NewServer(svc.Handler())
	t.Cleanup(bodyTS.Close)
	bodyClt, err := bodyapi.New(bodyTS.URL, bodyapi.HTTPClient(nil), secret, bodyapi.NewSigner(priv))
	if err != nil {
		t.Fatal(err)
	}

	srv, err := newStack(mainSt, builtin.Options{}, WithBody(bodyClt))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	signup(t, ts, c, "root") // user 1

	api := ts.URL + "/api/v1/users/1"
	if code, out := apiCall(t, c, "POST", api+"/scale-readings", `[{"measured_at":"2026-03-01T08:00:00Z","weight_kg":81.234,"notes":"a private note"},{"measured_at":"2026-03-02T08:00:00Z","weight_kg":80.1}]`, nil); code != 200 {
		t.Fatalf("post readings: %d %s", code, out)
	}
	if code, out := apiCall(t, c, "POST", api+"/dexa-scans", `{"measured_at":"2026-03-01T00:00:00Z","weight_kg":80,"fat_ratio_pct":20}`, nil); code != 201 {
		t.Fatalf("post scan: %d %s", code, out)
	}
	if code, out := apiCall(t, c, "GET", api+"/scale-readings", "", nil); code != 200 || !strings.Contains(out, "81.234") || !strings.Contains(out, "a private note") {
		t.Fatalf("list readings: %d %s", code, out)
	}
	first := get(t, c, api+"/estimates", nil)
	etag := first.Header.Get("ETag")
	readBody(first)
	if first.StatusCode != 200 || etag == "" {
		t.Fatalf("estimates: %d %q", first.StatusCode, etag)
	}
	if resp := get(t, c, api+"/estimates", map[string]string{"If-None-Match": etag}); resp.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation through the body service: %d", resp.StatusCode)
	}
	// The pages that show the data work too.
	if code, page := body(t, c, ts.URL+"/u/1/scale"); code != 200 || !strings.Contains(page, "81.2") {
		t.Errorf("scale page: %d", code)
	}

	// What each database holds: the main one has no scans or readings, the body one has no users.
	mainTables := countTables(t, mainPath, "users", "sessions", "scale_readings", "dexa_scans", "body_user_state", "body_meta")
	for _, n := range []string{"scale_readings", "dexa_scans", "body_user_state", "body_meta"} {
		if mainTables[n] {
			t.Errorf("the main database has %s", n)
		}
	}
	if !mainTables["users"] || !mainTables["sessions"] {
		t.Errorf("the main database lacks its own tables: %v", mainTables)
	}
	bodyTables := countTables(t, bodyPath, "users", "sessions", "app_settings", "audit_log", "scale_readings", "dexa_scans", "body_user_state", "body_meta")
	for _, n := range []string{"users", "sessions", "app_settings", "audit_log"} {
		if bodyTables[n] {
			t.Errorf("the body database has %s", n)
		}
	}
	for _, n := range []string{"scale_readings", "dexa_scans", "body_user_state", "body_meta"} {
		if !bodyTables[n] {
			t.Errorf("the body database lacks %s", n)
		}
	}
	raw, err := sql.Open("sqlite", "file:"+bodyPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var data string
	if err := raw.QueryRow(`SELECT data FROM scale_readings ORDER BY id LIMIT 1`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(data, "enc:v2:") || strings.Contains(data, "81.234") || strings.Contains(data, "private") {
		t.Errorf("the readings are not encrypted in the body database: %q", data)
	}

	// A user who is deleted from the main database leaves data in the body database until it is
	// removed: the sweep finds it by comparing the two.
	if ids, err := bodyClt.UserIDs(ctx); err != nil || len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("users with data: %v %v", ids, err)
	}
	bob, err := mainSt.CreateUser(ctx, "bob") // id 2
	if err != nil {
		t.Fatal(err)
	}
	w := 70.0
	if _, err := bodySt.UpsertScaleReadings(ctx, bob.ID, []models.ScaleReading{{MeasuredAt: time.Now(), WeightKg: &w}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Backend.sweepBodyOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if ids, _ := bodyClt.UserIDs(ctx); len(ids) != 2 {
		t.Fatalf("the sweep removed the data of a user who exists: %v", ids)
	}
	if err := mainSt.DeleteUser(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.Backend.sweepBodyOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if ids, _ := bodyClt.UserIDs(ctx); len(ids) != 1 || ids[0] != 1 {
		t.Errorf("after the sweep: %v, want only user 1", ids)
	}

	// With the body service stopped the readings and estimates are unavailable, and the rest works.
	bodyTS.Close()
	if code, out := apiCall(t, c, "GET", api+"/estimates", "", nil); code != http.StatusServiceUnavailable || !strings.Contains(out, "unavailable") {
		t.Errorf("estimates without the body service: %d %s", code, out)
	}
	if code, _ := apiCall(t, c, "GET", api+"/scale-readings", "", nil); code != http.StatusServiceUnavailable {
		t.Errorf("readings without the body service: %d", code)
	}
	if code, _ := body(t, c, ts.URL+"/u/1/scale"); code != http.StatusServiceUnavailable {
		t.Errorf("scale page without the body service: %d", code)
	}
	if code, _ := apiCall(t, c, "GET", api+"/settings", "", nil); code != 200 {
		t.Errorf("settings without the body service: %d", code)
	}
	if code, _ := body(t, c, ts.URL+"/healthz"); code != 200 {
		t.Errorf("healthz without the body service: %d", code)
	}
}
