package kvserver

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/mvcc"
)

// BenchmarkCommitSingleConcurrent compares sustained throughput of the
// server-side 1PC path with and without application-level batching. Each
// sub-benchmark runs n workers that keep committing independent transactions;
// "batched" uses the per-range commitBatcher (the production CommitSingle
// path), while "direct" is the pre-batching baseline that proposes one
// CommandPrewriteCommit per transaction.
func BenchmarkCommitSingleConcurrent(b *testing.B) {
	_, _, g := newBatchBenchServer(b)
	for _, n := range []int{10, 100} {
		b.Run(fmt.Sprintf("batched/concurrent=%d", n), func(b *testing.B) {
			var batches atomic.Uint64
			before := g.batcher.stats().Batches
			benchmarkSustained(b, n, func(id uint64) error {
				startTS, key := benchTxn(id)
				return g.batcher.submit(context.Background(), applyCommand{
					StartTS: startTS, CommitTS: startTS + 1, Primary: key,
					Mutations: []mvcc.Mutation{{Key: key, Value: []byte("v")}},
				})
			})
			batches.Store(g.batcher.stats().Batches - before)
			b.ReportMetric(float64(batches.Load())/float64(b.N), "batches/round")
		})
		b.Run(fmt.Sprintf("direct/concurrent=%d", n), func(b *testing.B) {
			benchmarkSustained(b, n, func(id uint64) error {
				startTS, key := benchTxn(id)
				payload, err := encodeCommand(applyCommand{
					StartTS: startTS, CommitTS: startTS + 1, Primary: key,
					Mutations: []mvcc.Mutation{{Key: key, Value: []byte("v")}},
				})
				if err != nil {
					return err
				}
				_, err = g.node.Propose(context.Background(),
					model.Entry{Type: model.CommandPrewriteCommit, Value: payload})
				return err
			})
		})
	}
}

// benchmarkSustained runs n workers, each submitting b.N commits, and reports
// nanoseconds per individual commit rather than per worker round.
func benchmarkSustained(b *testing.B, n int, submit func(id uint64) error) {
	b.Helper()
	b.ReportAllocs()
	var seq atomic.Uint64
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for w := 0; w < n; w++ {
		go func() {
			defer wg.Done()
			<-start
			var firstErr error
			for i := 0; i < b.N && firstErr == nil; i++ {
				firstErr = submit(seq.Add(1))
			}
			errs <- firstErr
		}()
	}
	b.ResetTimer()
	close(start)
	wg.Wait()
	b.StopTimer()
	close(errs)
	for err := range errs {
		if err != nil {
			b.Fatalf("commit failed: %v", err)
		}
	}
	elapsed := b.Elapsed()
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N*n), "ns/commit")
}

func newBatchBenchServer(b *testing.B) (*Server, string, *rangeGroup) {
	b.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("reserve addr: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	srv, err := New(Config{
		ID:                 addr,
		RaftAddr:           addr,
		DataDir:            b.TempDir(),
		ElectionTimeoutMin: 50 * time.Millisecond,
		ElectionTimeoutMax: 100 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
		CacheSize:          16 << 20,
		MemTableSize:       8 << 20,
		Logger:             testLogger(),
	})
	if err != nil {
		b.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		b.Fatalf("start server: %v", err)
	}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			b.Errorf("stop server: %v", err)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if g, err := srv.GroupForID(1); err == nil && g.Status().Role == "leader" {
			return srv, addr, srv.groups[1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.Fatal("range 1 did not elect a leader")
	return nil, "", nil
}

func benchTxn(id uint64) (uint64, []byte) {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, id)
	return uint64(1)<<40 + id*2, key
}
