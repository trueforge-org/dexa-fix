package edge

import (
	"strconv"
	"testing"
)

// BenchmarkLimiterAllow measures the one lock every request goes through, with many callers and a
// spread of client addresses.
func BenchmarkLimiterAllow(b *testing.B) {
	l := &Limiter{Rate: 1e9, Burst: 1 << 30}
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = "192.0.2." + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			l.Allow(keys[i%len(keys)])
			i++
		}
	})
}
