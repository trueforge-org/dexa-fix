package sqlstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// A sequence that cannot reach the highest copied id makes the reset after the copy fail, and the copy
// with it (PostgreSQL only: SQLite has no sequences to reset).
func TestCopyFailsWhenASequenceCannotBeReset(t *testing.T) {
	ctx := context.Background()
	dst := newStore(t)
	if dst.driver != DriverPostgres {
		t.Skip("needs PostgreSQL: DEXAFIX_TEST_POSTGRES_DSN is not set")
	}
	src, err := Open(ctx, DriverSQLite, "file:"+filepath.Join(t.TempDir(), "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for i := 0; i < 3; i++ {
		if err := src.AddAuditEvent(ctx, &models.AuditEvent{Action: "login"}); err != nil {
			t.Fatal(err)
		}
	}
	// The copy inserts the ids 1 to 3; setval(..., 3) is out of bounds for this sequence.
	if _, err := dst.db.ExecContext(ctx, `ALTER SEQUENCE audit_log_id_seq MAXVALUE 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, src, dst); err == nil || !strings.Contains(err.Error(), "reset sequence audit_log") {
		t.Fatalf("want the failed sequence reset, got %v", err)
	}
	// The copy is one transaction: nothing of it stays.
	if n, err := dst.db.NewSelect().Model((*auditRow)(nil)).Count(ctx); err != nil || n != 0 {
		t.Fatalf("rows left after the failed copy: %d, %v", n, err)
	}
}
