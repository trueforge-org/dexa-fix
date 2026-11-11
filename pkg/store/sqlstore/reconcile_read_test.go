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

func TestSeveralReadingsAreDeletedInOneTransactionWithOneVersionChange(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	alice, err := s.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	w := 80.0
	at := time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC)
	mk := func(d int) models.ScaleReading {
		return models.ScaleReading{MeasuredAt: at.AddDate(0, 0, -d), WeightKg: &w}
	}
	if _, err := s.UpsertScaleReadings(ctx, alice.ID, []models.ScaleReading{mk(0), mk(1), mk(2), mk(3)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertScaleReadings(ctx, bob.ID, []models.ScaleReading{mk(0)}); err != nil {
		t.Fatal(err)
	}
	mine, _ := s.ListScaleReadings(ctx, alice.ID)
	bobs, _ := s.ListScaleReadings(ctx, bob.ID)
	before, _ := s.BodyVersion(ctx, alice.ID)

	t.Run("ids that exist, that do not and that are somebody else's", func(t *testing.T) {
		ids := []int64{mine[0].ID, mine[1].ID, 987654, bobs[0].ID}
		n, err := s.DeleteScaleReadings(ctx, alice.ID, ids)
		if err != nil || n != 2 {
			t.Fatalf("deleted %d, %v; want her 2", n, err)
		}
		if left, _ := s.ListScaleReadings(ctx, alice.ID); len(left) != 2 {
			t.Fatalf("%d of her readings are left, want 2", len(left))
		}
		if left, _ := s.ListScaleReadings(ctx, bob.ID); len(left) != 1 {
			t.Fatal("a reading of another user was deleted")
		}
		if after, _ := s.BodyVersion(ctx, alice.ID); after != before+1 {
			t.Fatalf("the version went from %d to %d, want one change", before, after)
		}
	})
	t.Run("nothing to delete changes nothing", func(t *testing.T) {
		v, _ := s.BodyVersion(ctx, alice.ID)
		for _, ids := range [][]int64{nil, {987654}} {
			if n, err := s.DeleteScaleReadings(ctx, alice.ID, ids); err != nil || n != 0 {
				t.Fatalf("deleted %d, %v", n, err)
			}
		}
		if after, _ := s.BodyVersion(ctx, alice.ID); after != v {
			t.Fatalf("the version went from %d to %d without a deletion", v, after)
		}
	})
	t.Run("more ids than one statement holds", func(t *testing.T) {
		left, _ := s.ListScaleReadings(ctx, alice.ID)
		ids := make([]int64, 0, upsertChunk+10)
		for i := 0; i < upsertChunk+9; i++ {
			ids = append(ids, int64(1_000_000+i))
		}
		ids = append(ids, left[0].ID) // in the second chunk
		if n, err := s.DeleteScaleReadings(ctx, alice.ID, ids); err != nil || n != 1 {
			t.Fatalf("deleted %d, %v; want the one in the second chunk", n, err)
		}
	})
}
