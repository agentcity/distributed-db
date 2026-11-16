package test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/metaserver"
	"distributed-db/internal/v2client"
)

func benchFreeAddr(b *testing.B) string {
	b.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("reserve addr: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// BenchmarkV2Transaction measures the real network path: TSO RPC, Prewrite
// Raft round, TSO RPC, Commit Raft round. Run with:
//
//	go test -run '^$' -bench BenchmarkV2Transaction -benchtime=50x ./test
func BenchmarkV2Transaction(b *testing.B) {
	ctx := context.Background()
	metaAddr := benchFreeAddr(b)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Logger: v2Logger()})
	if err != nil {
		b.Fatalf("meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		b.Fatalf("meta start: %v", err)
	}
	defer ms.Stop()

	addr := benchFreeAddr(b)
	srv, err := kvserver.New(kvserver.Config{
		ID: addr, RaftAddr: addr, DataDir: b.TempDir(),
		ElectionTimeoutMin: 100 * time.Millisecond,
		ElectionTimeoutMax: 200 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		CacheSize:          64 << 20,
		MemTableSize:       32 << 20,
		Logger:             v2Logger(),
	})
	if err != nil {
		b.Fatalf("server: %v", err)
	}
	if err := srv.Start(ctx); err != nil {
		b.Fatalf("start: %v", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Stop(sctx)
	}()

	// Wait for leadership before starting the timer.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.Raft().Status().Role != "leader" {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Raft().Status().Role != "leader" {
		b.Fatalf("no leader")
	}

	client, err := v2client.New(metaAddr, []string{addr})
	if err != nil {
		b.Fatalf("client: %v", err)
	}
	defer client.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txn, err := client.Begin(ctx)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		key := []byte(fmt.Sprintf("bench-%08d", i))
		txn.Set(key, []byte("value"))
		if err := txn.Commit(ctx); err != nil {
			b.Fatalf("commit: %v", err)
		}
	}
}
