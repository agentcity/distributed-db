package test

import (
	"context"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/v2client"
)

func TestV2RangeHostSplitMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr := v2FreeAddr(t)
	addr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{
		Addr: metaAddr,
		Ranges: []meta.Range{{
			ID: 1, Start: nil, End: nil, Replicas: []string{addr}, Version: 1,
		}},
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("meta start: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	srv, err := kvserver.New(kvserver.Config{
		ID: addr, RaftAddr: addr, DataDir: t.TempDir(),
		Ranges:             []kvserver.RangeConfig{{ID: 1, Start: nil, End: nil}},
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
	if !waitGroupLeader(t, srv, 1) {
		t.Fatal("range 1 did not become leader")
	}

	client, err := v2client.New(metaAddr, []string{addr})
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
	put("a", "left")
	put("m", "middle")
	put("z", "right")

	ranges, err := srv.SplitRange(ctx, 1, []byte("m"), 2)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(ranges) != 2 {
		t.Fatalf("ranges after split = %+v", ranges)
	}
	if !waitGroupLeader(t, srv, 2) {
		t.Fatal("range 2 did not become leader")
	}
	metaRanges := make([]meta.Range, 0, len(ranges))
	for _, r := range ranges {
		metaRanges = append(metaRanges, meta.Range{
			ID: r.ID, Start: r.Start, End: r.End,
			Replicas: []string{addr}, Version: 1,
		})
	}
	if err := ms.ApplyRanges(ctx, metaRanges); err != nil {
		t.Fatalf("meta ranges after split: %v", err)
	}
	client2, err := v2client.New(metaAddr, []string{addr})
	if err != nil {
		t.Fatalf("client2: %v", err)
	}
	t.Cleanup(func() { _ = client2.Close() })
	get(client2, "a", "left")
	get(client2, "m", "middle")
	get(client2, "z", "right")

	// A write to the right side must go to the new Raft group.
	txn, err := client2.Begin(ctx)
	if err != nil {
		t.Fatalf("right begin: %v", err)
	}
	txn.Set([]byte("zz"), []byte("new"))
	if err := txn.Commit(ctx); err != nil {
		t.Fatalf("right commit: %v", err)
	}
	get(client2, "zz", "new")

	merged, err := srv.MergeRanges(ctx, 1, 2)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("ranges after merge = %+v", merged)
	}
	if err := ms.ApplyRanges(ctx, []meta.Range{{
		ID: 1, Start: nil, End: nil, Replicas: []string{addr}, Version: 2,
	}}); err != nil {
		t.Fatalf("meta range after merge: %v", err)
	}
	client3, err := v2client.New(metaAddr, []string{addr})
	if err != nil {
		t.Fatalf("client3: %v", err)
	}
	t.Cleanup(func() { _ = client3.Close() })
	get(client3, "a", "left")
	get(client3, "z", "right")
	get(client3, "zz", "new")
}

func waitGroupLeader(t *testing.T, srv *kvserver.Server, rangeID uint64) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		node, err := srv.GroupForID(rangeID)
		if err == nil && node.Status().Role == raft.Leader {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestV2DistributedSplit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	addrs := []string{v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t)}
	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{
		Addr: metaAddr,
		Ranges: []meta.Range{{
			ID: 1, Start: nil, End: nil, Replicas: addrs, Version: 1,
		}},
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("meta start: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	nodes := make([]*v2Node, 0, 3)
	for i, addr := range addrs {
		peers := make([]string, 0, 2)
		for j, p := range addrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		cfg := kvserver.Config{
			ID: addr, RaftAddr: addr, DataDir: t.TempDir(), Peers: peers,
			RangeID:            1,
			ElectionTimeoutMin: 120 * time.Millisecond,
			ElectionTimeoutMax: 240 * time.Millisecond,
			HeartbeatInterval:  25 * time.Millisecond,
			CacheSize:          8 << 20, MemTableSize: 4 << 20,
			Logger: v2Logger(),
		}
		srv, err := kvserver.New(cfg)
		if err != nil {
			t.Fatalf("new %s: %v", addr, err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatalf("start %s: %v", addr, err)
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
	leader := v2WaitLeader(t, nodes)

	client, err := v2client.New(metaAddr, addrs)
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

	// The coordinator asks every replica to split; followers wait for the
	// leader's replicated prepare command.
	split := func(n *v2Node) {
		t.Helper()
		resp, err := n.client.SplitRange(ctx, &pbv2.SplitRangeRequest{
			RangeId: 1, SplitKey: []byte("m"), NewRangeId: 2,
		})
		if err != nil || !resp.GetSuccess() {
			t.Fatalf("split on %s: %v %+v", n.addr, err, resp)
		}
	}
	split(leader)
	for _, n := range nodes {
		if n != leader {
			split(n)
		}
	}

	// Wait for the new range group's leader on at least one node.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		ok := false
		for _, n := range nodes {
			node, err := n.srv.GroupForID(2)
			if err == nil && node.Status().Role == raft.Leader {
				ok = true
				break
			}
		}
		if ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, n := range nodes {
		if _, err := n.srv.GroupForID(2); err != nil {
			t.Fatalf("range 2 group missing on %s", n.addr)
		}
	}

	ranges := leader.srv.Ranges()
	metaRanges := make([]meta.Range, 0, len(ranges))
	for _, r := range ranges {
		metaRanges = append(metaRanges, meta.Range{
			ID: r.ID, Start: r.Start, End: r.End,
			Replicas: addrs, Version: 1,
		})
	}
	if err := ms.ApplyRanges(ctx, metaRanges); err != nil {
		t.Fatalf("meta ranges: %v", err)
	}

	client2, err := v2client.New(metaAddr, addrs)
	if err != nil {
		t.Fatalf("client2: %v", err)
	}
	t.Cleanup(func() { _ = client2.Close() })
	get := func(key, want string) {
		t.Helper()
		txn, err := client2.Begin(ctx)
		if err != nil {
			t.Fatalf("read begin: %v", err)
		}
		got, err := txn.Get(ctx, []byte(key))
		if err != nil || string(got) != want {
			t.Fatalf("get %s = %q, %v; want %q", key, got, err, want)
		}
	}
	get("a", "left")
	get("m", "middle")
	get("z", "right")

	txn, err := client2.Begin(ctx)
	if err != nil {
		t.Fatalf("right begin: %v", err)
	}
	txn.Set([]byte("zz"), []byte("new"))
	if err := txn.Commit(ctx); err != nil {
		t.Fatalf("right commit: %v", err)
	}
	get("zz", "new")
}

func TestV2DistributedMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	addrs := []string{v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t)}
	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{
		Addr: metaAddr,
		Ranges: []meta.Range{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: addrs, Version: 1},
			{ID: 2, Start: []byte("m"), End: nil, Replicas: addrs, Version: 1},
		},
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("meta start: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	nodes := make([]*v2Node, 0, 3)
	for i, addr := range addrs {
		peers := make([]string, 0, 2)
		for j, p := range addrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		cfg := kvserver.Config{
			ID: addr, RaftAddr: addr, DataDir: t.TempDir(),
			Ranges: []kvserver.RangeConfig{
				{ID: 1, Start: nil, End: []byte("m"), Peers: peers},
				{ID: 2, Start: []byte("m"), End: nil, Peers: peers},
			},
			ElectionTimeoutMin: 120 * time.Millisecond,
			ElectionTimeoutMax: 240 * time.Millisecond,
			HeartbeatInterval:  25 * time.Millisecond,
			CacheSize:          8 << 20, MemTableSize: 4 << 20,
			Logger: v2Logger(),
		}
		srv, err := kvserver.New(cfg)
		if err != nil {
			t.Fatalf("new %s: %v", addr, err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatalf("start %s: %v", addr, err)
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
	for _, id := range []uint64{1, 2} {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			leader := false
			for _, n := range nodes {
				if node, err := n.srv.GroupForID(id); err == nil && node.Status().Role == raft.Leader {
					leader = true
					break
				}
			}
			if leader {
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
	}

	client, err := v2client.New(metaAddr, addrs)
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
	put("z", "right")

	// The coordinator asks all replicas concurrently: the leader of each
	// group proposes prepare while followers wait for it.
	var wg sync.WaitGroup
	errs := make(chan string, len(nodes))
	for _, n := range nodes {
		wg.Add(1)
		go func(n *v2Node) {
			defer wg.Done()
			resp, err := n.client.MergeRanges(ctx, &pbv2.MergeRangesRequest{LeftRangeId: 1, RightRangeId: 2})
			if err != nil || !resp.GetSuccess() {
				errs <- n.addr + ": " + errString(err) + " " + resp.GetError()
			}
		}(n)
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatalf("merge failed: %s", msg)
	}
	for _, n := range nodes {
		if _, err := n.srv.GroupForID(2); err == nil {
			t.Fatalf("range 2 still present on %s", n.addr)
		}
	}
	if err := ms.ApplyRanges(ctx, []meta.Range{{
		ID: 1, Start: nil, End: nil, Replicas: addrs, Version: 2,
	}}); err != nil {
		t.Fatalf("meta after merge: %v", err)
	}
	client2, err := v2client.New(metaAddr, addrs)
	if err != nil {
		t.Fatalf("client2: %v", err)
	}
	t.Cleanup(func() { _ = client2.Close() })
	get := func(key, want string) {
		t.Helper()
		txn, err := client2.Begin(ctx)
		if err != nil {
			t.Fatalf("read begin: %v", err)
		}
		got, err := txn.Get(ctx, []byte(key))
		if err != nil || string(got) != want {
			t.Fatalf("get %s = %q, %v; want %q", key, got, err, want)
		}
	}
	get("a", "left")
	get("z", "right")
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
