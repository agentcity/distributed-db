package test

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"

	"distributed-db/internal/kvserver"
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

// BenchmarkV2Workstations measures aggregate throughput when several
// workstations each keep a pipeline of independent transactions in flight.
// pipeline=1 is the one-in-flight-per-client case; larger values emulate
// batching/pipelining on each workstation with CommitBatch.
func BenchmarkV2Workstations(b *testing.B) {
	benchmarkWorkstations(b, kvserver.DurabilityFast, []int{1, 16, 64, 256})
}

// BenchmarkV2WorkstationsSync is the production sync counterpart.
func BenchmarkV2WorkstationsSync(b *testing.B) {
	benchmarkWorkstations(b, kvserver.DurabilitySync, []int{1, 16, 64, 256})
}

func benchmarkWorkstations(b *testing.B, durability string, pipelines []int) {
	c := newSpeedClusterWithDurability(b, durability)
	speedWarmup(b, c.client)
	ctx := context.Background()

	for _, pipeline := range pipelines {
		b.Run(fmt.Sprintf("pipeline=%d", pipeline), func(b *testing.B) {
			var seq atomic.Uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				buf := make([]byte, 0, 64)
				for pb.Next() {
					txns := make([]*v2client.Txn, pipeline)
					for j := range txns {
						txn, err := c.client.Begin(ctx)
						if err != nil {
							b.Errorf("pipeline=%d begin: %v", pipeline, err)
							return
						}
						txn.Set(speedAppendKey(buf, "bench-workstation/", seq.Add(1)), speedValue)
						txns[j] = txn
					}
					for j, err := range c.client.CommitBatch(ctx, txns) {
						if err != nil {
							b.Errorf("pipeline=%d commit slot %d: %v", pipeline, j, err)
							return
						}
					}
				}
			})
			b.StopTimer()
			b.ReportMetric(float64(pipeline), "txn/op")
			if elapsed := b.Elapsed(); elapsed > 0 {
				b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N*pipeline), "ns/txn")
			}
		})
	}
}

// BenchmarkV2MultiRangeWorkstations measures 10 clients with keys spread over
// multiple ranges. Range i owns keys "NN/"; leaders are distributed by Raft, so
// this is the fan-out test for independent range leaders.
func BenchmarkV2MultiRangeWorkstations(b *testing.B) {
	for _, ranges := range []int{1, 4, 8} {
		b.Run(fmt.Sprintf("ranges=%d", ranges), func(b *testing.B) {
			c := newSpeedMultiRangeCluster(b, ranges, kvserver.DurabilityFast)
			ctx := context.Background()
			for _, pipeline := range []int{1, 64, 256} {
				b.Run(fmt.Sprintf("pipeline=%d", pipeline), func(b *testing.B) {
					var seq atomic.Uint64
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						prefixes := make([][]byte, ranges)
						for i := range prefixes {
							prefixes[i] = []byte(fmt.Sprintf("%02d/bench/", i))
						}
						for pb.Next() {
							txns := make([]*v2client.Txn, pipeline)
							for j := range txns {
								id := seq.Add(1)
								rid := int(id % uint64(ranges))
								txn, err := c.client.Begin(ctx)
								if err != nil {
									b.Errorf("ranges=%d pipeline=%d begin: %v", ranges, pipeline, err)
									return
								}
								key := append(append([]byte(nil), prefixes[rid]...), strconv.AppendUint(nil, id, 10)...)
								txn.Set(key, speedValue)
								txns[j] = txn
							}
							for j, err := range c.client.CommitBatch(ctx, txns) {
								if err != nil {
									b.Errorf("ranges=%d pipeline=%d slot %d: %v", ranges, pipeline, j, err)
									return
								}
							}
						}
					})
					b.StopTimer()
					b.ReportMetric(float64(pipeline), "txn/op")
					if elapsed := b.Elapsed(); elapsed > 0 {
						b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N*pipeline), "ns/txn")
					}
				})
			}
		})
	}
}
