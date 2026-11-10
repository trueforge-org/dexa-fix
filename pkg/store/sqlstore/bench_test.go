package sqlstore

import (
	"context"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// BenchmarkIdleLeasePass is what one idle worker slot costs the database each time its lease
// request looks for work and finds none: record the worker, then try to lease.
func BenchmarkIdleLeasePass(b *testing.B) {
	ctx := context.Background()
	s, err := Open(ctx, DriverSQLite, "file:"+b.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	kinds := []string{"withings", "strengthlevel"}
	req := models.LeaseRequest{Kinds: kinds, Owner: "bench", TokenHash: "h", LeaseFor: time.Minute, MaxRunning: map[string]int{}}
	b.ReportAllocs()
	for b.Loop() {
		now := time.Now()
		if err := s.TouchWorker(ctx, "bench", kinds, "", now); err != nil {
			b.Fatal(err)
		}
		if _, ok, err := s.LeaseJob(ctx, req, now); err != nil || ok {
			b.Fatalf("leased from an empty queue: %v %v", ok, err)
		}
	}
}
