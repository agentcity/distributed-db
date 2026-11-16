package v2client

import (
	"context"
	"testing"
	"time"
)

// BenchmarkAllocTS measures local batch consumption. The fake meta adds a
// 100µs delay to each AllocTS call, approximating a replicated TSO RPC; one
// such call serves a whole batch.
func BenchmarkAllocTS(b *testing.B) {
	fake := newFakeMeta("meta1")
	fake.allocDelay = 100 * time.Microsecond
	c := newTestClient(fake)
	c.tsBatchSize = defaultTSOBatchSize
	c.tsBatchTTL = time.Second
	c.tsIdleCheck = time.Hour
	c.tsValidateEvery = 1 << 62
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.AllocTS(ctx); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	allocs, _, _, _ := fake.stats()
	b.ReportMetric(float64(allocs)/float64(b.N), "meta-alloc/op")
}

// BenchmarkAllocTSUnbatched is the same workload with a batch size of one, so
// every AllocTS call performs a meta RPC.
func BenchmarkAllocTSUnbatched(b *testing.B) {
	fake := newFakeMeta("meta1")
	fake.allocDelay = 100 * time.Microsecond
	c := newTestClient(fake)
	c.tsBatchSize = 1
	c.tsBatchTTL = time.Second
	c.tsIdleCheck = time.Hour
	c.tsValidateEvery = 1 << 62
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.AllocTS(ctx); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	allocs, _, _, _ := fake.stats()
	b.ReportMetric(float64(allocs)/float64(b.N), "meta-alloc/op")
}
