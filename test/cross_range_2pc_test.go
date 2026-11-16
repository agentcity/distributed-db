package test

import (
	"context"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	"distributed-db/internal/v2client"
)

func TestV2CrossRange2PC(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr, addr := v2FreeAddr(t), v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{
		Addr: metaAddr,
		Ranges: []meta.Range{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{addr}, Version: 1},
			{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{addr}, Version: 1},
		},
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	srv, err := kvserver.New(kvserver.Config{
		ID: addr, RaftAddr: addr, DataDir: t.TempDir(),
		Ranges: []kvserver.RangeConfig{
			{ID: 1, Start: nil, End: []byte("m")},
			{ID: 2, Start: []byte("m"), End: nil},
		},
		ElectionTimeoutMin: 80 * time.Millisecond, ElectionTimeoutMax: 160 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond, CacheSize: 16 << 20, MemTableSize: 8 << 20,
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	if !waitGroupLeader(t, srv, 1) || !waitGroupLeader(t, srv, 2) {
		t.Fatal("ranges did not elect leaders")
	}
	client, err := v2client.New(metaAddr, []string{addr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	txn, err := client.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	txn.Set([]byte("a"), []byte("left"))
	txn.Set([]byte("z"), []byte("right"))
	if err := txn.Commit(ctx); err != nil {
		t.Fatalf("cross-range commit: %v", err)
	}
	for _, tc := range []struct{ key, want string }{{"a", "left"}, {"z", "right"}} {
		txn, err := client.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, err := txn.Get(ctx, []byte(tc.key))
		_ = txn.Rollback(ctx)
		if err != nil || string(got) != tc.want {
			t.Fatalf("get %s = %q, %v; want %q", tc.key, got, err, tc.want)
		}
	}
}
