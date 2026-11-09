package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

var deleteForms = regexp.MustCompile(`action="/u/1/scale/\d+/delete"`)

func serverOf(ts *httptest.Server) *Server {
	s, _ := testServers.Load(ts)
	return s.(*Server)
}

func get(t *testing.T, c *http.Client, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readBody(resp *http.Response) string {
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}

func seedReadings(t *testing.T, st store.Store, n int) {
	t.Helper()
	base := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	var rs []models.ScaleReading
	for i := 0; i < n; i++ {
		w := 70 + float64(i%10)
		rs = append(rs, models.ScaleReading{MeasuredAt: base.Add(time.Duration(i) * 24 * time.Hour), WeightKg: &w, Source: "manual", Method: "scale"})
	}
	if _, err := st.UpsertScaleReadings(t.Context(), 1, rs); err != nil {
		t.Fatal(err)
	}
}

func TestAPIListsArePaged(t *testing.T) {
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "root")
	seedReadings(t, st, 25)

	url := ts.URL + "/api/v1/users/1/scale-readings?limit=10"
	total, pages := 0, 0
	for url != "" {
		resp := get(t, c, url, nil)
		var page []models.ScaleReading
		if err := json.Unmarshal([]byte(readBody(resp)), &page); err != nil {
			t.Fatal(err)
		}
		total += len(page)
		pages++
		url = ""
		if next := resp.Header.Get("X-Next-Cursor"); next != "" {
			url = ts.URL + "/api/v1/users/1/scale-readings?limit=10&before=" + next
		}
	}
	if total != 25 || pages != 3 {
		t.Errorf("walked %d readings in %d pages", total, pages)
	}

	// Without parameters everything up to the default limit comes back, with no cursor.
	resp := get(t, c, ts.URL+"/api/v1/users/1/scale-readings", nil)
	var all []models.ScaleReading
	json.Unmarshal([]byte(readBody(resp)), &all)
	if len(all) != 25 || resp.Header.Get("X-Next-Cursor") != "" {
		t.Errorf("default listing: %d readings, cursor %q", len(all), resp.Header.Get("X-Next-Cursor"))
	}

	// since and until are inclusive, and a plain date as until covers the whole day.
	resp = get(t, c, ts.URL+"/api/v1/users/1/scale-readings?since=2026-01-03&until=2026-01-05", nil)
	var win []models.ScaleReading
	json.Unmarshal([]byte(readBody(resp)), &win)
	if len(win) != 3 {
		t.Errorf("window of three days returned %d readings", len(win))
	}

	for _, bad := range []string{"limit=0", "limit=abc", "limit=100000", "before=nonsense", "since=yesterday", "until=2026-13-45"} {
		resp := get(t, c, ts.URL+"/api/v1/users/1/scale-readings?"+bad, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("?%s: %d", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if resp := get(t, c, ts.URL+"/api/v1/users/1/dexa-scans?limit=5", nil); resp.StatusCode != 200 {
		t.Errorf("dexa-scans paging: %d", resp.StatusCode)
	}
}

func TestScalePageIsPaged(t *testing.T) {
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "root")
	seedReadings(t, st, 150)

	first := readBody(get(t, c, ts.URL+"/u/1/scale", nil))
	if !strings.Contains(first, "Older readings") || strings.Contains(first, "Newest readings") {
		t.Fatalf("first page links wrong:\n%s", first)
	}
	if n := len(deleteForms.FindAllString(first, -1)); n != scalePageSize {
		t.Errorf("first page has %d rows, want %d", n, scalePageSize)
	}
	i := strings.Index(first, "?before=")
	link := first[i : i+strings.IndexByte(first[i:], '"')]
	second := readBody(get(t, c, ts.URL+"/u/1/scale"+link, nil))
	if strings.Contains(second, "Older readings") || !strings.Contains(second, "Newest readings") {
		t.Errorf("last page links wrong")
	}
	if n := len(deleteForms.FindAllString(second, -1)); n != 50 {
		t.Errorf("last page has %d rows, want 50", n)
	}
	if resp := get(t, c, ts.URL+"/u/1/scale?before=junk", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("junk cursor: %d", resp.StatusCode)
	}
}

func TestEstimatesAreCachedAndInvalidatedByEveryWrite(t *testing.T) {
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "root")
	seedReadings(t, st, 20)
	srv := serverOf(ts)
	url := ts.URL + "/api/v1/users/1/estimates"

	first := get(t, c, url, nil)
	etag := first.Header.Get("ETag")
	body1 := readBody(first)
	if first.StatusCode != 200 || etag == "" || first.Header.Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("first response: %d %v", first.StatusCode, first.Header)
	}

	// A client that kept the answer is told it is still good, and the server computes nothing.
	missesBefore := srv.estCache.misses
	if resp := get(t, c, url, map[string]string{"If-None-Match": etag}); resp.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation: %d", resp.StatusCode)
	}
	// A client without a copy is served from the server's cache.
	if body2 := readBody(get(t, c, url, nil)); body2 != body1 {
		t.Error("cached response differs from the first")
	}
	if srv.estCache.misses != missesBefore || srv.estCache.hits == 0 {
		t.Errorf("not served from the cache: hits %d, misses %d (was %d)", srv.estCache.hits, srv.estCache.misses, missesBefore)
	}

	// Every kind of change must make the old answer stale, whichever way it was made.
	changes := map[string]func(){
		"adding a reading through the API": func() {
			code, _ := apiCall(t, c, "POST", ts.URL+"/api/v1/users/1/scale-readings", `[{"measured_at":"2026-03-01T08:00:00Z","weight_kg":99}]`, nil)
			if code != 201 && code != 200 {
				t.Fatalf("post reading: %d", code)
			}
		},
		"adding a scan": func() {
			code, _ := apiCall(t, c, "POST", ts.URL+"/api/v1/users/1/dexa-scans", `{"measured_at":"2026-02-01T00:00:00Z","weight_kg":80,"fat_ratio_pct":20}`, nil)
			if code != 201 {
				t.Fatalf("post scan: %d", code)
			}
		},
		"changing the settings": func() {
			code, _ := apiCall(t, c, "PUT", ts.URL+"/api/v1/users/1/settings", `{"athlete_default":true,"method_ranking":[]}`, nil)
			if code != 200 {
				t.Fatalf("put settings: %d", code)
			}
		},
		"deleting a reading behind the web app's back": func() {
			rs, _ := st.ListScaleReadings(t.Context(), 1)
			if err := st.DeleteScaleReading(t.Context(), 1, rs[0].ID); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range changes {
		before := get(t, c, url, nil)
		oldTag := before.Header.Get("ETag")
		readBody(before)
		change()
		if resp := get(t, c, url, map[string]string{"If-None-Match": oldTag}); resp.StatusCode != 200 {
			t.Errorf("%s: the old copy was still considered good (%d)", name, resp.StatusCode)
		} else if resp.Header.Get("ETag") == oldTag {
			t.Errorf("%s: the ETag did not change", name)
		}
	}

	// Another user's data never leaks through the cache.
	if resp := get(t, c, ts.URL+"/api/v1/users/2/estimates", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("other user's estimates: %d", resp.StatusCode)
	}
}

func TestEstimateCacheEvictsAndKeepsNewerVersions(t *testing.T) {
	c := newEstimateCache()
	calls := 0
	res := func() func() (estimate.Result, error) {
		return func() (estimate.Result, error) { calls++; return estimate.Result{}, nil }
	}
	key := func(v int64) estimateKey { return estimateKey{version: v, day: "2026-01-01"} }

	c.get(1, key(5), res())
	c.get(1, key(5), res())
	if calls != 1 {
		t.Errorf("same key computed %d times", calls)
	}
	c.get(1, key(6), res())
	c.get(1, key(5), res()) // a slow request from before the write must not replace the newer result...
	c.get(1, key(6), res()) // ...so version 6 is still cached
	if calls != 3 {
		t.Errorf("calls %d, want 3 (v5, v6, late v5)", calls)
	}
	for id := int64(10); id < 10+estimateCacheUsers+5; id++ {
		c.get(id, key(1), res())
	}
	if len(c.entries) > estimateCacheUsers {
		t.Errorf("cache holds %d users, limit %d", len(c.entries), estimateCacheUsers)
	}
}
