package sqlstore

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

func TestReadingsForAnImportAreThoseInTheWindowAndThoseWithAFetchedID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC)
	w := 80.0
	mk := func(daysAgo int, source, ext string) models.ScaleReading {
		r := models.ScaleReading{MeasuredAt: base.AddDate(0, 0, -daysAgo), WeightKg: &w, Source: source}
		if ext != "" {
			r.ExternalID = &ext
		}
		return r
	}
	rs := []models.ScaleReading{
		mk(0, "demo", "new"),    // in the window, with a fetched id
		mk(1, "manual", ""),     // in the window, no id
		mk(400, "demo", "old"),  // far outside the window, with a fetched id
		mk(400, "demo", "gone"), // far outside the window, id not asked for
		mk(400, "manual", ""),   // far outside the window
		mk(400, "other", "old"), // another source with the same id
	}
	if _, err := s.UpsertScaleReadings(ctx, u.ID, rs); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertScaleReadings(ctx, other.ID, []models.ScaleReading{mk(0, "demo", "new")}); err != nil {
		t.Fatal(err)
	}
	window := store.ListFilter{Since: base.AddDate(0, 0, -3), Until: base.AddDate(0, 0, 3)}
	keys := func(got []models.ScaleReading) map[string]int {
		m := map[string]int{}
		for _, r := range got {
			k := r.Source
			if r.ExternalID != nil {
				k += "/" + *r.ExternalID
			}
			m[k]++
		}
		return m
	}

	t.Run("the window and the ids together, each reading once", func(t *testing.T) {
		got, err := s.ScaleReadingsForReconcile(ctx, u.ID, window, "demo", []string{"new", "old", "missing"})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int{"demo/new": 1, "manual": 1, "demo/old": 1}
		if k := keys(got); len(k) != len(want) || k["demo/new"] != 1 || k["manual"] != 1 || k["demo/old"] != 1 {
			t.Fatalf("got %v, want %v", k, want)
		}
	})
	t.Run("only the window", func(t *testing.T) {
		got, err := s.ScaleReadingsForReconcile(ctx, u.ID, window, "demo", nil)
		if err != nil || len(got) != 2 {
			t.Fatalf("got %d readings, %v; want the 2 in the window", len(got), err)
		}
	})
	t.Run("only the ids", func(t *testing.T) {
		got, err := s.ScaleReadingsForReconcile(ctx, u.ID, store.ListFilter{}, "demo", []string{"old"})
		if k := keys(got); err != nil || len(got) != 1 || k["demo/old"] != 1 {
			t.Fatalf("got %v, %v; want the demo reading with that id and not the other source's", k, err)
		}
	})
	t.Run("nothing asked, nothing read", func(t *testing.T) {
		got, err := s.ScaleReadingsForReconcile(ctx, u.ID, store.ListFilter{}, "demo", nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("got %d readings, %v", len(got), err)
		}
	})
	t.Run("more ids than one query holds", func(t *testing.T) {
		ids := make([]string, 0, upsertChunk+50)
		for i := 0; i < upsertChunk+49; i++ {
			ids = append(ids, "none-"+strconv.Itoa(i))
		}
		ids = append(ids, "old") // lands in the second chunk
		got, err := s.ScaleReadingsForReconcile(ctx, u.ID, store.ListFilter{}, "demo", ids)
		if err != nil || len(got) != 1 {
			t.Fatalf("got %d readings, %v; want the one in the second chunk", len(got), err)
		}
	})
	t.Run("another user's readings are never returned", func(t *testing.T) {
		got, err := s.ScaleReadingsForReconcile(ctx, other.ID, window, "demo", []string{"old"})
		if k := keys(got); err != nil || len(got) != 1 || k["demo/new"] != 1 {
			t.Fatalf("got %v, %v; want only the other user's own reading", k, err)
		}
	})
}
