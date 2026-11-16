package raft_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
	"distributed-db/internal/rafttest"
)

// BenchmarkProposeParallel measures throughput of concurrent client proposals
// on a single-node leader backed by a real FileStore. "group-commit" uses the
// AppendBatch capability; "per-entry" uses a store without it, reproducing the
// old one-durable-append-per-proposal path.
func BenchmarkProposeParallel(b *testing.B) {
	b.Run("group-commit", func(b *testing.B) {
		benchmarkProposeParallel(b, true)
	})
	b.Run("per-entry", func(b *testing.B) {
		benchmarkProposeParallel(b, false)
	})
}

func benchmarkProposeParallel(b *testing.B, batch bool) {
	b.Helper()
	dir := b.TempDir()
	fs, err := raft.NewFileStore(dir)
	if err != nil {
		b.Fatalf("NewFileStore: %v", err)
	}
	defer fs.Close()

	var store raft.LogStore = fs
	if !batch {
		store = &fallbackStore{inner: fs}
	}

	cfg := testConfig("bench", nil, dir)
	cfg.ProposeBatchWindow = time.Millisecond
	cfg.ProposeBatchMax = 128
	node, err := raft.New(cfg, store, newTestSM(), rafttest.NewNetwork().Transport("bench"))
	if err != nil {
		b.Fatalf("raft.New: %v", err)
	}
	if err := node.Start(context.Background()); err != nil {
		b.Fatalf("Start: %v", err)
	}
	defer node.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for !node.IsLeader() {
		if time.Now().After(deadline) {
			b.Fatal("single node did not become leader")
		}
		time.Sleep(time.Millisecond)
	}

	var seq atomic.Uint64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := seq.Add(1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := node.Propose(ctx, model.Entry{
				Type:  model.CommandPut,
				Key:   fmt.Sprintf("bench-%d", id),
				Value: []byte("v"),
			})
			cancel()
			if err != nil {
				b.Errorf("Propose: %v", err)
				return
			}
		}
	})
}
