package mainbackend

import (
	"context"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
	"github.com/trueforge-org/dexafix/pkg/workers/builtin"
)

// breakTable makes every later query on the table fail, as if the database had lost it, so that a
// test can see what a handler does when the store lets it down. Foreign keys are on in the test
// stores, which rules out dropping a table that others refer to; renaming it is enough.
func breakTable(t *testing.T, st *sqlstore.Store, table string) {
	t.Helper()
	if err := st.ExecForTests(context.Background(), "ALTER TABLE "+table+" RENAME TO "+table+"_gone"); err != nil {
		t.Fatalf("break table %s: %v", table, err)
	}
}

func TestABrokenTableFailsEveryQueryOnItAndNoOther(t *testing.T) {
	_, _, st := newServerAndStore(t, builtin.Options{})
	ctx := context.Background()
	if err := st.PutSetting(ctx, "greeting", "hello"); err != nil {
		t.Fatal(err)
	}
	breakTable(t, st, "app_settings")
	if _, err := st.GetSetting(ctx, "greeting"); err == nil {
		t.Error("a setting was read from a table that is gone")
	}
	if _, err := st.ListUsers(ctx); err != nil {
		t.Errorf("a table that was not broken failed too: %v", err)
	}
}
