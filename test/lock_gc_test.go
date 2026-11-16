package test

import (
	"context"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
)

func TestV2BackgroundLockResolver(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr := v2FreeAddr(t)
	addr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Logger: v2Logger()})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("meta start: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	srv, err := kvserver.New(kvserver.Config{
		ID: addr, RaftAddr: addr, DataDir: t.TempDir(),
		ElectionTimeoutMin: 80 * time.Millisecond,
		ElectionTimeoutMax: 160 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		CacheSize:          8 << 20, MemTableSize: 4 << 20,
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Stop(sctx)
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.Raft().Status().Role != "leader" {
		time.Sleep(20 * time.Millisecond)
	}
	if srv.Raft().Status().Role != "leader" {
		t.Fatal("no leader")
	}
	client := v2Dial(t, addr)

	// A lock far older than the 3s TTL must be resolved by the background
	// resolver, not by a client read.
	oldTS := uint64(time.Now().Add(-10*time.Second).UnixMilli()) << 18
	pr, err := client.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: oldTS, Primary: []byte("expired"),
		Mutations: []*pbv2.Mutation{{Key: []byte("expired"), Value: []byte("v")}},
	})
	if err != nil || !pr.GetSuccess() {
		t.Fatalf("prewrite: %v %+v", err, pr)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.ResolvedLocks() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if srv.ResolvedLocks() == 0 {
		t.Fatal("background resolver did not resolve the expired lock")
	}
	gr, err := client.Get(ctx, &pbv2.GetRequest{Key: []byte("expired"), Ts: oldTS + 1})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gr.GetFound() {
		t.Fatalf("expired lock produced a value: %+v", gr)
	}
	st, err := client.CheckTxnStatus(ctx, &pbv2.CheckTxnStatusRequest{Key: []byte("expired"), StartTs: oldTS})
	if err != nil || !st.GetRolledBack() {
		t.Fatalf("status after resolver: %v %+v", err, st)
	}
}
