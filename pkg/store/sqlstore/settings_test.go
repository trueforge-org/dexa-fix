package sqlstore

import (
	"context"
	"testing"
)

func TestSettingsGenerationGrowsWithEveryWrite(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	g0, err := st.SettingsGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting(ctx, "a", "1"); err != nil {
		t.Fatal(err)
	}
	g1, _ := st.SettingsGeneration(ctx)
	if err := st.PutSetting(ctx, "a", "2"); err != nil { // an update counts too
		t.Fatal(err)
	}
	g2, _ := st.SettingsGeneration(ctx)
	if err := st.DeleteSetting(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	g3, _ := st.SettingsGeneration(ctx)
	if !(g0 < g1 && g1 < g2 && g2 < g3) {
		t.Errorf("generations %d %d %d %d are not strictly growing", g0, g1, g2, g3)
	}
	// Reading does not change it.
	st.GetSetting(ctx, "a")
	if g, _ := st.SettingsGeneration(ctx); g != g3 {
		t.Errorf("a read changed the generation: %d -> %d", g3, g)
	}
}
