package mainbackend

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/bodyapi"
)

// The rules are the same for every source of readings: the tests use a source that is not Withings.
const rcSource = "demo-scale"

func rcFl(v float64) *float64 { return &v }

func rcTs(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func rcFetch(ext, when string, w float64) models.ScaleReading {
	return models.ScaleReading{MeasuredAt: rcTs(when), WeightKg: rcFl(w), Source: rcSource, ExternalID: &ext}
}

func TestReconcileFollowsTheUsersAthleteDefaultAndIsIdempotent(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	if err := f.st.UpdateUserSettings(ctx, f.user.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	u, _ := f.st.GetUser(ctx, f.user.ID)
	fetch := func() []models.ScaleReading { return []models.ScaleReading{rcFetch("g1", "2026-03-01T07:00", 80.5)} }

	reconcile := func() (bodyapi.ImportResult, error) {
		return f.b.body.Reconcile(ctx, u, bodyapi.ReconcileRequest{Source: rcSource, Fetched: fetch(), Seen: []string{"g1"}})
	}
	res, err := reconcile()
	if err != nil || res.Imported != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	stored, _ := f.st.ListScaleReadings(ctx, u.ID)
	if len(stored) != 1 || !stored[0].AthleteMode || stored[0].Source != rcSource {
		t.Fatalf("stored: %+v", stored)
	}
	// Applying the same result again changes nothing, which is what lets a worker report twice.
	res, err = reconcile()
	if err != nil || res.Unchanged != 1 || res.Imported != 0 {
		t.Fatalf("second: %+v %v", res, err)
	}
	if stored, _ = f.st.ListScaleReadings(ctx, u.ID); len(stored) != 1 {
		t.Fatalf("not idempotent: %d rows", len(stored))
	}
}

func TestWithingsResultFromAWorkerIsValidated(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	apply := func(userID int64, result string) (string, error) {
		return applyWithingsImport(ctx, f.b, models.Job{UserID: userID, Kind: kindWithingsImport}, json.RawMessage(result))
	}
	ext := `"g1"`
	for name, result := range map[string]string{
		"not json":          `{`,
		"another source":    `{"readings":[{"source":"somewhere","external_id":` + ext + `,"measured_at":"2026-03-01T07:00:00Z","weight_kg":80}]}`,
		"no external id":    `{"readings":[{"source":"withings","measured_at":"2026-03-01T07:00:00Z","weight_kg":80}]}`,
		"no time":           `{"readings":[{"source":"withings","external_id":` + ext + `,"weight_kg":80}]}`,
		"unknown user":      "",
		"unreadable result": `[1,2]`,
	} {
		uid := f.user.ID
		if name == "unknown user" {
			uid, result = 9999, `{"readings":[]}`
		}
		if _, err := apply(uid, result); !errors.Is(err, errBadResult) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A reading that names another user or id is stored for the job's user, with a fresh id.
	good := `{"readings":[{"id":777,"user_id":` + strconv.FormatInt(f.admin.ID, 10) + `,"source":"withings","external_id":` + ext + `,"measured_at":"2026-03-01T07:00:00Z","weight_kg":80}],"seen":["g1"]}`
	out, err := apply(f.user.ID, good)
	if err != nil || !strings.Contains(out, `"imported":1`) {
		t.Fatalf("good result: %q %v", out, err)
	}
	if rs, _ := f.st.ListScaleReadings(ctx, f.admin.ID); len(rs) != 0 {
		t.Fatalf("a worker put a reading on another user: %+v", rs)
	}
	if rs, _ := f.st.ListScaleReadings(ctx, f.user.ID); len(rs) != 1 || rs[0].ID == 777 {
		t.Fatalf("stored: %+v", rs)
	}
}

func TestWithingsAuthURLCarriesTheStateAndTheAdminsCredentials(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	p, _ := f.b.providerNamed(providerWithings)
	if ok, _ := p.OAuth.Configured(ctx, f.b); ok {
		t.Fatal("configured without credentials")
	}
	if _, ok := f.b.oauthProvider(ctx, providerWithings); ok {
		t.Fatal("an unconfigured provider offers a connection")
	}
	setWithingsApp(t, f.st)
	if _, ok := f.b.oauthProvider(ctx, providerWithings); !ok {
		t.Fatal("configured provider is not offered")
	}
	raw, err := p.OAuth.AuthURL(ctx, f.b, "st")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "account.withings.com" || q.Get("client_id") != "id" || q.Get("state") != "st" || q.Get("redirect_uri") != "http://app/integrations/withings/callback" || q.Get("scope") != "user.metrics" || q.Get("response_type") != "code" {
		t.Fatalf("auth url: %s", u)
	}
	if strings.Contains(raw, "secret") || strings.Contains(raw, "client_secret") {
		t.Fatal("the secret is in the URL")
	}
}
