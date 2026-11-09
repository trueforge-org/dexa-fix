package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// slowStore blocks a page of DEXA scans (a public API call) until released.
type slowStore struct {
	store.Store
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func (s *slowStore) ListDexaScansPage(ctx context.Context, userID int64, f store.ListFilter) ([]models.DexaScan, *store.Cursor, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return s.Store.ListDexaScansPage(ctx, userID, f)
}

// newBackendPair is a backend with a public and an internal listener on one database, a user and a
// personal access token.
func newBackendPair(t *testing.T) (pub, internal *httptest.Server, slow *slowStore, cookie *http.Cookie, token string) {
	t.Helper()
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "root")
	cookie = c.Transport.(cookieTransport).cookie
	token = "dxf_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := st.CreateAccessToken(context.Background(), &models.AccessToken{UserID: 1, Name: "t", Prefix: "dxf_", Hash: hashToken(token)}); err != nil {
		t.Fatal(err)
	}
	slow = &slowStore{Store: st, release: make(chan struct{}), started: make(chan struct{})}
	b, err := NewBackend(slow, importer.NewRegistry(withings.New(withings.Config{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	b.SetServiceSecrets("current-secret-0123456789abcdef")
	pub = httptest.NewServer(b.PublicHandler())
	internal = httptest.NewServer(b.InternalHandler())
	t.Cleanup(pub.Close)
	t.Cleanup(internal.Close)
	return
}

func TestBackendPublicListenerServesOnlyTheAPI(t *testing.T) {
	pub, _, _, _, token := newBackendPair(t)
	for path, want := range map[string]int{"/": 404, "/u/1": 404, "/login": 404, "/internal/v1/session": 404, "/healthz": 200} {
		resp, err := http.Get(pub.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: %d, want %d", path, resp.StatusCode, want)
		}
	}
	req, _ := http.NewRequest("GET", pub.URL+"/api/v1/users/1/settings", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("API with a token: %d", resp.StatusCode)
	}
}

// A flood on the public API is shed with 503, and the internal API keeps answering.
func TestBackendPublicFloodDoesNotStarveTheInternalAPI(t *testing.T) {
	pub, internal, slow, cookie, token := newBackendPair(t)
	t.Cleanup(func() { close(slow.release) })

	// Send more requests than the public listener admits at once. Each blocks in the store, so the
	// in-flight slots and the queue fill up; the rest are refused.
	var wg sync.WaitGroup
	statuses := make(chan int, 200)
	for range publicMaxInFlight + publicMaxQueue + 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", pub.URL+"/api/v1/users/1/dexa-scans", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	<-slow.started
	time.Sleep(300 * time.Millisecond)
	call := func() int {
		req, _ := http.NewRequest("GET", pub.URL+"/api/v1/users/1/dexa-scans", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return -1
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// The public API is saturated or rate limited: refused quickly, not queued behind the flood.
	start := time.Now()
	if got := call(); got != http.StatusServiceUnavailable && got != http.StatusTooManyRequests {
		t.Errorf("flooded public API answered %d", got)
	}
	if took := time.Since(start); took > budgetMaxWait+time.Second {
		t.Errorf("refusal took %v", took)
	}

	// The internal API, used by the frontend, still answers promptly.
	c, _ := apiclient.New(internal.URL, nil, "current-secret-0123456789abcdef")
	ctx := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: sessionCookie + "=" + cookie.Value, ClientIP: "198.51.100.7"})
	start = time.Now()
	u, err := c.Session(ctx)
	if err != nil || u == nil {
		t.Fatalf("internal API during the public flood: %v %v", u, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("internal API took %v during the flood", took)
	}
}

func TestBackendPicksUpSettingsChangedByAnotherProcess(t *testing.T) {
	old := settingsPollInterval
	settingsPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { settingsPollInterval = old })
	_, _, slow, _, _ := newBackendPair(t)
	b, err := NewBackend(slow.Store, importer.NewRegistry(withings.New(withings.Config{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	b.StartSettingsPoll()
	time.Sleep(50 * time.Millisecond)

	// Another process (here: the store directly, like `dexa-fix settings reset-network`).
	if err := slow.Store.PutSetting(context.Background(), settingBaseURL, "https://other.example.com"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for b.cfg().baseURL == nil || b.cfg().baseURL.Host != "other.example.com" {
		if time.Now().After(deadline) {
			t.Fatal("the backend did not reload the settings")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type fakePool struct {
	store.Store
	n int
}

func (f fakePool) MaxConns() int { return f.n }

func TestPublicAPIGetsAtMostHalfOfTheDatabaseConnections(t *testing.T) {
	for conns, want := range map[int]int{1: 1, 4: 2, 20: 10, 100: publicMaxInFlight} {
		if got := publicInFlight(fakePool{n: conns}); got != want {
			t.Errorf("%d connections: %d public requests in flight, want %d", conns, got, want)
		}
	}
	if got := publicInFlight(nil); got != publicMaxInFlight { // a store that does not say
		t.Errorf("unknown pool: %d", got)
	}
}

func TestBasicAuthFloodCannotTakeEveryPasswordCheck(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	signup(t, ts, c, "root")
	srv := serverOf(ts)
	old := bcryptWait
	bcryptWait = 50 * time.Millisecond
	t.Cleanup(func() { bcryptWait = old })

	for range cap(srv.bcryptSlots) { // every slot is busy
		srv.bcryptSlots <- struct{}{}
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/1/settings", nil)
	req.SetBasicAuth("root@example.com", testPassword)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || time.Since(start) > time.Second {
		t.Errorf("with every slot busy: %d after %v", resp.StatusCode, time.Since(start))
	}
	for range cap(srv.bcryptSlots) { // free again
		<-srv.bcryptSlots
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("with free slots: %d", resp.StatusCode)
	}
}
