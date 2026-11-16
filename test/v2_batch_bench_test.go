package test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"distributed-db/internal/v2client"
)

// BenchmarkV2TxnBatch3Node measures concurrent transaction pipelining through
// CommitBatch on the standard three-node speed cluster. Every transaction is a
// single-key 1PC transaction. Batch sizes 1/16/64/256 show the fixed-overhead
// regime (1), the default-concurrency regime (16 and 64, since the default limit
// is 32) and the queued regime (256). The numbers are reported, never asserted:
// they exist for trend comparison between runs.
//
// Run with:
//
//	go test -run '^$' -bench BenchmarkV2TxnBatch3Node -benchtime=20x ./test
func BenchmarkV2TxnBatch3Node(b *testing.B) {
	c := newSpeedCluster(b)
	speedWarmup(b, c.client)
	ctx := context.Background()

	for _, size := range []int{1, 16, 64, 256} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			buf := make([]byte, 0, 64)
			var seq atomic.Uint64

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				txns := make([]*v2client.Txn, size)
				for j := range txns {
					txn, err := c.client.Begin(ctx)
					if err != nil {
						b.Fatalf("size=%d begin %d: %v", size, j, err)
					}
					txn.Set(speedAppendKey(buf, "bench-batch/", seq.Add(1)), speedValue)
					txns[j] = txn
				}
				for j, err := range c.client.CommitBatch(ctx, txns) {
					if err != nil {
						b.Fatalf("size=%d commit slot %d: %v", size, j, err)
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(size), "txn/op")
			if elapsed := b.Elapsed(); elapsed > 0 {
				b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N*size), "ns/txn")
			}
		})
	}
}
