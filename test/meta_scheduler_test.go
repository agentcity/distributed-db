package test

import (
	"context"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/v2client"
)

func TestV2MetaSchedulerSplitMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	kvAddrs := []string{v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t)}
	metaAddrs := []string{v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t)}

	metas := make([]*metaserver.Server, 0, 3)
	for i, addr := range metaAddrs {
		peers := make([]string, 0, 2)
		for j, p := range metaAddrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		ms, err := metaserver.New(metaserver.Config{
			Addr: addr, ID: addr, Peers: peers, DataDir: t.TempDir(), Replicated: true,
			Ranges:             []meta.Range{{ID: 1, Start: nil, End: nil, Replicas: kvAddrs, Version: 1}},
			ElectionTimeoutMin: 120 * time.Millisecond,
			ElectionTimeoutMax: 240 * time.Millisecond,
			HeartbeatInterval:  25 * time.Millisecond,
			Logger:             v2Logger(),
		})
		if err != nil {
			t.Fatalf("meta %s: %v", addr, err)
		}
		if err := ms.Start(ctx); err != nil {
			t.Fatalf("start meta %s: %v", addr, err)
		}
		metas = append(metas, ms)
		t.Cleanup(func() { _ = ms.Stop() })
	}
	metaLeader := waitMetaLeader(t, metas)
	sched := metaClient(t, metaLeader.Addr())

	nodes := make([]*v2Node, 0, 3)
	for i, addr := range kvAddrs {
		peers := make([]string, 0, 2)
		for j, p := range kvAddrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		cfg := kvserver.Config{
			ID: addr, RaftAddr: addr, DataDir: t.TempDir(), Peers: peers, RangeID: 1,
			ElectionTimeoutMin: 120 * time.Millisecond,
			ElectionTimeoutMax: 240 * time.Millisecond,
			HeartbeatInterval:  25 * time.Millisecond,
			CacheSize:          8 << 20, MemTableSize: 4 << 20,
			Logger: v2Logger(),
		}
		srv, err := kvserver.New(cfg)
		if err != nil {
			t.Fatalf("new kv %s: %v", addr, err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatalf("start kv %s: %v", addr, err)
		}
		n := &v2Node{srv: srv, addr: addr, cfg: cfg}
		n.client = v2Dial(t, addr)
		nodes = append(nodes, n)
		t.Cleanup(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Stop(sctx)
		})
	}
	_ = v2WaitLeader(t, nodes)

	client, err := v2client.New(metaLeader.Addr(), kvAddrs)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	put := func(key, value string) {
		t.Helper()
		txn, err := client.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		txn.Set([]byte(key), []byte(value))
		if err := txn.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", key, err)
		}
	}
	put("a", "left")
	put("m", "middle")
	put("z", "right")

	split, err := sched.SplitRange(ctx, &pbv2.MetaSplitRangeRequest{RangeId: 1, SplitKey: []byte("m")})
	if err != nil || !split.GetSuccess() {
		t.Fatalf("meta split: %v %+v", err, split)
	}
	newID := split.GetNewRangeId()
	if newID == 0 {
		t.Fatal("meta split returned no new range id")
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		leaderFound := false
		for _, n := range nodes {
			if node, err := n.srv.GroupForID(newID); err == nil && node.Status().Role == raft.Leader {
				leaderFound = true
				break
			}
		}
		if leaderFound {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	client2, err := v2client.New(metaLeader.Addr(), kvAddrs)
	if err != nil {
		t.Fatalf("client2: %v", err)
	}
	t.Cleanup(func() { _ = client2.Close() })
	get := func(c *v2client.Client, key, want string) {
		t.Helper()
		txn, err := c.Begin(ctx)
		if err != nil {
			t.Fatalf("read begin: %v", err)
		}
		got, err := txn.Get(ctx, []byte(key))
		if err != nil || string(got) != want {
			t.Fatalf("get %s = %q, %v; want %q", key, got, err, want)
		}
	}
	get(client2, "a", "left")
	get(client2, "m", "middle")
	get(client2, "z", "right")

	merge, err := sched.MergeRanges(ctx, &pbv2.MetaMergeRangesRequest{LeftRangeId: 1, RightRangeId: newID})
	if err != nil || !merge.GetSuccess() {
		t.Fatalf("meta merge: %v %+v", err, merge)
	}
	client3, err := v2client.New(metaLeader.Addr(), kvAddrs)
	if err != nil {
		t.Fatalf("client3: %v", err)
	}
	t.Cleanup(func() { _ = client3.Close() })
	get(client3, "a", "left")
	get(client3, "z", "right")

	rows, err := sched.ListRanges(ctx, &pbv2.ListRangesRequest{})
	if err != nil || len(rows.GetRanges()) != 1 {
		t.Fatalf("ranges after merge: %v %+v", err, rows)
	}
}
