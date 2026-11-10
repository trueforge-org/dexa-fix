package strengthlevel

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/workers/runner"
)

var goodLogin = workerapi.Login{Email: "a@b.c", Password: "pw"}

func newWorker(t *testing.T) (*Worker, *fake, *httptest.Server) {
	t.Helper()
	f := &fake{entries: map[string]map[string]any{
		"2026-11-06": {"id": "e1", "date": "2026-11-06", "bodyweight": 72.7, "bodyfat": 15.8, "bodyfat_method": "caliper", "timezone": "Europe/Amsterdam", "timezone_offset_mins": -60},
	}}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	return New(Config{Base: ts.URL}, nil), f, ts
}

func TestCheckLoginAnswersEvenWhenTheLoginIsWrong(t *testing.T) {
	ctx := context.Background()
	w, f, _ := newWorker(t)
	res, err := w.checkLogin(ctx, workerapi.CheckLoginPayload{Login: goodLogin})
	if err != nil || res.(workerapi.CheckLoginResult).LoginOK != true {
		t.Fatalf("good login: %+v %v", res, err)
	}
	// A refused login is an answer the backend acts on, not a failed job to retry.
	res, err = w.checkLogin(ctx, workerapi.CheckLoginPayload{Login: workerapi.Login{Email: "a@b.c", Password: "wrong"}})
	got := res.(workerapi.CheckLoginResult)
	if err != nil || got.LoginOK || got.Message != "the e-mail address or password was refused." {
		t.Fatalf("wrong login: %+v %v", res, err)
	}
	f.badPage = true
	if _, err := w.checkLogin(ctx, workerapi.CheckLoginPayload{Login: goodLogin}); !runner.IsPermanent(err) {
		t.Fatalf("a changed site: %v", err)
	}
}

func TestUnreachableServiceIsRetried(t *testing.T) {
	ctx := context.Background()
	w, _, ts := newWorker(t)
	ts.Close()
	_, err := w.checkLogin(ctx, workerapi.CheckLoginPayload{Login: goodLogin})
	if ok, _ := runner.IsRetryable(err); !ok || err.Error() != "Strength Level could not be reached." {
		t.Fatalf("check: %v", err)
	}
	_, err = w.sendDay(ctx, workerapi.ExportPayload{Login: goodLogin, Entries: []workerapi.ExportEntry{{Day: "2026-11-07", WeightKg: 73}}})
	if ok, _ := runner.IsRetryable(err); !ok || err.Error() != "Could not sign in to Strength Level: Strength Level could not be reached." {
		t.Fatalf("day: %v", err)
	}
}

func TestSendDayAddsOverwritesAndLeavesAlone(t *testing.T) {
	ctx := context.Background()
	w, f, _ := newWorker(t)
	send := func(e workerapi.ExportEntry) workerapi.ExportResult {
		t.Helper()
		res, err := w.sendDay(ctx, workerapi.ExportPayload{Login: goodLogin, Entries: []workerapi.ExportEntry{e}, Update: true})
		if err != nil {
			t.Fatal(err)
		}
		return res.(workerapi.ExportResult)
	}
	if r := send(workerapi.ExportEntry{Day: "2026-11-07", WeightKg: 73}); r.Added != 1 || len(f.posted) != 1 {
		t.Fatalf("new day: %+v", r)
	}
	if r := send(workerapi.ExportEntry{Day: "2026-11-06", WeightKg: 71.0, FatPct: fp(14.2)}); r.Updated != 1 || len(f.puts) != 1 {
		t.Fatalf("existing day with other values: %+v", r)
	}
	if r := send(workerapi.ExportEntry{Day: "2026-11-06", WeightKg: 71.0, FatPct: fp(14.2)}); r.Unchanged != 1 || len(f.puts) != 1 {
		t.Fatalf("same values: %+v", r)
	}
}

func TestSendDayFailuresReadLikeTheOldMessages(t *testing.T) {
	ctx := context.Background()
	w, _, _ := newWorker(t)
	entry := []workerapi.ExportEntry{{Day: "2026-11-07", WeightKg: 73}}
	_, err := w.sendDay(ctx, workerapi.ExportPayload{Login: workerapi.Login{Email: "a@b.c", Password: "nope"}, Entries: entry})
	if !runner.IsPermanent(err) || err.Error() != "Could not sign in to Strength Level: the e-mail address or password was refused." {
		t.Fatalf("refused: %v", err)
	}
	if _, err := w.sendDay(ctx, workerapi.ExportPayload{Login: goodLogin}); !runner.IsPermanent(err) {
		t.Fatalf("no day: %v", err)
	}
}

func TestSyncReportsProgressAndCounts(t *testing.T) {
	ctx := context.Background()
	w, f, _ := newWorker(t)
	p := workerapi.ExportPayload{Login: goodLogin, Since: "2026-11-01", Entries: []workerapi.ExportEntry{
		{Day: "2026-11-08", WeightKg: 72.74},
		{Day: "2026-11-07", WeightKg: 73},
		{Day: "2026-11-06", WeightKg: 99}, // exists with another value: left alone
		{Day: "2026-10-01", WeightKg: 70}, // before since
	}}
	var progress []workerapi.ExportProgress
	res, err := w.sync(ctx, p, 0, func(pr workerapi.ExportProgress) { progress = append(progress, pr) })
	if err != nil {
		t.Fatal(err)
	}
	got := res.(workerapi.ExportResult)
	if got.Added != 2 || got.Unchanged != 1 || got.Failed != 0 || len(f.posted) != 2 {
		t.Fatalf("result %+v, posts %d", got, len(f.posted))
	}
	if len(progress) != 3 || progress[2] != (workerapi.ExportProgress{Done: 3, Total: 3}) {
		t.Fatalf("progress: %+v", progress)
	}
	// With update, the existing day is overwritten too.
	p.Update = true
	if res, err = w.sync(ctx, p, 0, func(workerapi.ExportProgress) {}); err != nil || res.(workerapi.ExportResult).Updated != 1 {
		t.Fatalf("update: %+v %v", res, err)
	}
}

func TestSyncStopsWhenTheJobIsCanceled(t *testing.T) {
	w, _, _ := newWorker(t)
	ctx, cancel := context.WithCancel(context.Background())
	p := workerapi.ExportPayload{Login: goodLogin, Entries: []workerapi.ExportEntry{{Day: "2026-11-07", WeightKg: 73}, {Day: "2026-11-08", WeightKg: 74}}}
	_, err := w.sync(ctx, p, 0, func(pr workerapi.ExportProgress) {
		if pr.Done == 1 {
			cancel() // the heartbeat said stop
		}
	})
	if err == nil || ctx.Err() == nil || strings.Contains(err.Error(), "could not") {
		t.Fatalf("a canceled sync must return the context's error: %v", err)
	}
}

func TestSyncSignInFailureReadsLikeTheOldStatus(t *testing.T) {
	w, _, _ := newWorker(t)
	_, err := w.sync(context.Background(), workerapi.ExportPayload{Login: workerapi.Login{Email: "a@b.c", Password: "nope"}}, 0, func(workerapi.ExportProgress) {})
	if !runner.IsPermanent(err) || err.Error() != "could not sign in: the e-mail address or password was refused." {
		t.Fatalf("%v", err)
	}
}
