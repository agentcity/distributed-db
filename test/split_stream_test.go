package test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/v2client"
)

func TestV2SplitStreamsLargeRange(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr, addr := v2FreeAddr(t), v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{
		Addr:   metaAddr,
		Ranges: []meta.Range{{ID: 1, Start: nil, End: nil, Replicas: []string{addr}, Version: 1}},
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
		Ranges:             []kvserver.RangeConfig{{ID: 1, Start: nil, End: nil}},
		ElectionTimeoutMin: 80 * time.Millisecond, ElectionTimeoutMax: 160 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond, CacheSize: 32 << 20, MemTableSize: 8 << 20,
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	if !waitGroupLeader(t, srv, 1) {
		t.Fatal("range 1 did not become leader")
	}
	client, err := v2client.New(metaAddr, []string{addr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	const total, batch = 2500, 100
	for base := 0; base < total; base += batch {
		txn, err := client.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := base; i < base+batch; i++ {
			txn.Set([]byte(fmt.Sprintf("k%05d", i)), []byte("v"))
		}
		if err := txn.Commit(ctx); err != nil {
			t.Fatalf("commit batch %d: %v", base, err)
		}
	}
	if _, err := srv.SplitRange(ctx, 1, []byte("k01250"), 2); err != nil {
		t.Fatalf("split: %v", err)
	}
	if !waitGroupLeader(t, srv, 2) {
		t.Fatal("new range 2 did not become leader")
	}
	// Raw client: no x-range-epoch metadata, because the host-local split
	// intentionally does not update the standalone meta map.
	raw := v2Dial(t, addr)
	scan := func(start, end []byte) int {
		resp, err := raw.Scan(ctx, &pbv2.ScanRequest{Start: start, End: end, Ts: ^uint64(0), Limit: total + 1})
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if resp.GetError() != "" {
			t.Fatalf("scan: %s", resp.GetError())
		}
		return len(resp.GetKvs())
	}
	left, right := scan(nil, []byte("k01250")), scan([]byte("k01250"), nil)
	if left != 1250 || right != 1250 {
		gr, _ := raw.Get(ctx, &pbv2.GetRequest{Key: []byte("k00000"), Ts: ^uint64(0)})
		t.Fatalf("left=%d right=%d first=%+v ranges=%+v", left, right, gr, srv.Ranges())
	}
}
