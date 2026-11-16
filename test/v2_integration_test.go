package test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/v2client"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type v2Node struct {
	srv    *kvserver.Server
	addr   string
	cfg    kvserver.Config
	client pbv2.KVServiceClient
}

func v2Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func v2FreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve addr: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func v2Dial(t *testing.T, addr string) pbv2.KVServiceClient {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pbv2.NewKVServiceClient(cc)
}

func v2Status(t *testing.T, c pbv2.KVServiceClient) *pbv2.StatusResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	resp, err := c.Status(ctx, &pbv2.StatusRequest{})
	if err != nil {
		return nil
	}
	return resp
}

func v2WaitLeader(t *testing.T, nodes []*v2Node) *v2Node {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			st := v2Status(t, n.client)
			if st != nil && strings.EqualFold(st.Role, "leader") {
				return n
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no v2 leader")
	return nil
}

func TestV2SingleRangeCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Logger: v2Logger()})
	if err != nil {
		t.Fatalf("meta server: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("start meta: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	metaCC, err := grpc.NewClient(metaAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("meta dial: %v", err)
	}
	t.Cleanup(func() { _ = metaCC.Close() })
	metaClient := pbv2.NewMetaServiceClient(metaCC)

	addrs := []string{v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t)}
	nodes := make([]*v2Node, 0, len(addrs))
	for i, addr := range addrs {
		peers := make([]string, 0, len(addrs)-1)
		for j, p := range addrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		cfg := kvserver.Config{
			ID:                 addr,
			RaftAddr:           addr,
			DataDir:            t.TempDir(),
			Peers:              peers,
			ElectionTimeoutMin: 150 * time.Millisecond,
			ElectionTimeoutMax: 300 * time.Millisecond,
			HeartbeatInterval:  30 * time.Millisecond,
			CacheSize:          8 << 20,
			MemTableSize:       4 << 20,
			Logger:             v2Logger(),
		}
		srv, err := kvserver.New(cfg)
		if err != nil {
			t.Fatalf("new node %s: %v", addr, err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatalf("start node %s: %v", addr, err)
		}
		n := &v2Node{srv: srv, addr: addr, cfg: cfg}
		n.client = v2Dial(t, addr)
		nodes = append(nodes, n)
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Stop(stopCtx)
		})
	}

	leader := v2WaitLeader(t, nodes)

	tso := func() (uint64, uint64) {
		t.Helper()
		resp, err := metaClient.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 2})
		if err != nil {
			t.Fatalf("alloc ts: %v", err)
		}
		return resp.Start, resp.Start + 1
	}

	put := func(key, value string) {
		t.Helper()
		startTS, commitTS := tso()
		pr, err := leader.client.Prewrite(ctx, &pbv2.PrewriteRequest{
			StartTs: startTS,
			Primary: []byte(key),
			Mutations: []*pbv2.Mutation{{
				Key:   []byte(key),
				Value: []byte(value),
			}},
		})
		if err != nil {
			t.Fatalf("prewrite: %v", err)
		}
		if !pr.GetSuccess() {
			t.Fatalf("prewrite failed: %s (leader %s)", pr.GetError(), pr.GetLeader())
		}
		cr, err := leader.client.Commit(ctx, &pbv2.CommitRequest{
			StartTs:  startTS,
			CommitTs: commitTS,
			Keys:     [][]byte{[]byte(key)},
		})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		if !cr.GetSuccess() {
			t.Fatalf("commit failed: %s", cr.GetError())
		}

		gr, err := leader.client.Get(ctx, &pbv2.GetRequest{Key: []byte(key), Ts: commitTS})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !gr.GetFound() || string(gr.GetValue()) != value {
			t.Fatalf("get(%s) = %q, found=%v, err=%q", key, gr.GetValue(), gr.GetFound(), gr.GetError())
		}
	}

	put("alpha", "one")
	put("beta", "two")

	sr, err := leader.client.Scan(ctx, &pbv2.ScanRequest{Start: []byte("a"), End: []byte("z"), Ts: 1 << 62})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(sr.GetKvs()) != 2 {
		t.Fatalf("scan returned %d rows: %v", len(sr.GetKvs()), sr.GetKvs())
	}

	// Kill the leader; the committed values must survive on the new leader.
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := leader.srv.Stop(stopCtx); err != nil {
		t.Fatalf("stop leader: %v", err)
	}
	survivors := make([]*v2Node, 0, len(nodes)-1)
	for _, n := range nodes {
		if n != leader {
			survivors = append(survivors, n)
		}
	}
	newLeader := v2WaitLeader(t, survivors)

	gr, err := newLeader.client.Get(ctx, &pbv2.GetRequest{Key: []byte("alpha"), Ts: 1 << 62})
	if err != nil {
		t.Fatalf("get after failover: %v", err)
	}
	if !gr.GetFound() || string(gr.GetValue()) != "one" {
		t.Fatalf("committed value lost after failover: %q found=%v err=%q", gr.GetValue(), gr.GetFound(), gr.GetError())
	}

	// New transaction must work on the new leader.
	func() {
		startTS, commitTS := tso()
		pr, err := newLeader.client.Prewrite(ctx, &pbv2.PrewriteRequest{
			StartTs: startTS,
			Primary: []byte("gamma"),
			Mutations: []*pbv2.Mutation{{
				Key:   []byte("gamma"),
				Value: []byte("three"),
			}},
		})
		if err != nil || !pr.GetSuccess() {
			t.Fatalf("post-failover prewrite: %v %+v", err, pr)
		}
		cr, err := newLeader.client.Commit(ctx, &pbv2.CommitRequest{
			StartTs: startTS, CommitTs: commitTS, Keys: [][]byte{[]byte("gamma")},
		})
		if err != nil || !cr.GetSuccess() {
			t.Fatalf("post-failover commit: %v %+v", err, cr)
		}
		gr, err := newLeader.client.Get(ctx, &pbv2.GetRequest{Key: []byte("gamma"), Ts: commitTS})
		if err != nil || !gr.GetFound() || string(gr.GetValue()) != "three" {
			t.Fatalf("post-failover get: %v %+v", err, gr)
		}
	}()

	// A real transactional client must discover the new leader and commit.
	vc, err := v2client.New(metaAddr, addrs)
	if err != nil {
		t.Fatalf("v2 client: %v", err)
	}
	t.Cleanup(func() { _ = vc.Close() })
	txn, err := vc.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	txn.Set([]byte("delta"), []byte("four"))
	if err := txn.Commit(ctx); err != nil {
		t.Fatalf("client commit: %v", err)
	}
	readTxn, err := vc.Begin(ctx)
	if err != nil {
		t.Fatalf("read txn: %v", err)
	}
	got, err := readTxn.Get(ctx, []byte("delta"))
	if err != nil || string(got) != "four" {
		t.Fatalf("client get delta = %q, %v", got, err)
	}

	if got := newLeader.srv.Raft().Status(); got.Leader == "" {
		t.Fatalf("new leader reports empty leader: %+v", got)
	}
}

func TestV2SnapshotRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Logger: v2Logger()})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("meta start: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })
	metaCC, err := grpc.NewClient(metaAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("meta dial: %v", err)
	}
	t.Cleanup(func() { _ = metaCC.Close() })
	metaClient := pbv2.NewMetaServiceClient(metaCC)

	addr := v2FreeAddr(t)
	dir := t.TempDir()
	cfg := kvserver.Config{
		ID: addr, RaftAddr: addr, DataDir: dir,
		ElectionTimeoutMin: 100 * time.Millisecond,
		ElectionTimeoutMax: 200 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		CacheSize:          8 << 20, MemTableSize: 4 << 20,
		Logger: v2Logger(),
	}
	srv, err := kvserver.New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	client := v2Dial(t, addr)
	waitNode := func(c pbv2.KVServiceClient) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if st := v2Status(t, c); st != nil && strings.EqualFold(st.Role, "leader") {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatal("node did not become leader")
	}
	waitNode(client)

	resp, err := metaClient.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 2})
	if err != nil {
		t.Fatalf("ts: %v", err)
	}
	startTS, commitTS := resp.Start, resp.Start+1
	pr, err := client.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: startTS, Primary: []byte("snap"),
		Mutations: []*pbv2.Mutation{{Key: []byte("snap"), Value: []byte("value")}},
	})
	if err != nil || !pr.GetSuccess() {
		t.Fatalf("prewrite: %v %+v", err, pr)
	}
	cr, err := client.Commit(ctx, &pbv2.CommitRequest{
		StartTs: startTS, CommitTs: commitTS, Keys: [][]byte{[]byte("snap")},
	})
	if err != nil || !cr.GetSuccess() {
		t.Fatalf("commit: %v %+v", err, cr)
	}
	snap, err := srv.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Index == 0 {
		t.Fatal("snapshot index is zero")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Stop(stopCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	srv2, err := kvserver.New(cfg)
	if err != nil {
		t.Fatalf("restart new: %v", err)
	}
	if err := srv2.Start(ctx); err != nil {
		t.Fatalf("restart start: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv2.Stop(sctx)
	})
	client2 := v2Dial(t, addr)
	waitNode(client2)
	gr, err := client2.Get(ctx, &pbv2.GetRequest{Key: []byte("snap"), Ts: commitTS})
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if !gr.GetFound() || string(gr.GetValue()) != "value" {
		t.Fatalf("snapshot restart lost value: found=%v value=%q err=%q", gr.GetFound(), gr.GetValue(), gr.GetError())
	}
}

func TestV2SnapshotCatchUp(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Logger: v2Logger()})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("meta start: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })
	metaCC, err := grpc.NewClient(metaAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("meta dial: %v", err)
	}
	t.Cleanup(func() { _ = metaCC.Close() })
	metaClient := pbv2.NewMetaServiceClient(metaCC)

	addrs := []string{v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t)}
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
			ElectionTimeoutMin: 150 * time.Millisecond,
			ElectionTimeoutMax: 300 * time.Millisecond,
			HeartbeatInterval:  30 * time.Millisecond,
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
	}

	leader := v2WaitLeader(t, nodes)
	var follower *v2Node
	for _, n := range nodes {
		if n != leader {
			follower = n
			break
		}
	}
	if follower == nil {
		t.Fatal("no follower")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := follower.srv.Stop(stopCtx); err != nil {
		t.Fatalf("stop follower: %v", err)
	}

	put := func(key, value string) uint64 {
		t.Helper()
		r, err := metaClient.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 2})
		if err != nil {
			t.Fatalf("ts: %v", err)
		}
		pr, err := leader.client.Prewrite(ctx, &pbv2.PrewriteRequest{
			StartTs: r.Start, Primary: []byte(key),
			Mutations: []*pbv2.Mutation{{Key: []byte(key), Value: []byte(value)}},
		})
		if err != nil || !pr.GetSuccess() {
			t.Fatalf("prewrite %s: %v %+v", key, err, pr)
		}
		cr, err := leader.client.Commit(ctx, &pbv2.CommitRequest{
			StartTs: r.Start, CommitTs: r.Start + 1, Keys: [][]byte{[]byte(key)},
		})
		if err != nil || !cr.GetSuccess() {
			t.Fatalf("commit %s: %v %+v", key, err, cr)
		}
		return r.Start + 1
	}
	commitTS := put("snap-key", "snap-value")
	put("snap-key2", "snap-value2")

	snap, err := leader.srv.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Index == 0 {
		t.Fatal("snapshot index is zero")
	}

	// Restart the follower with its old log; the leader has compacted the
	// prefix, so catch-up must use InstallSnapshot.
	restarted, err := kvserver.New(follower.cfg)
	if err != nil {
		t.Fatalf("restart follower: %v", err)
	}
	if err := restarted.Start(ctx); err != nil {
		t.Fatalf("start restarted follower: %v", err)
	}
	restartedNode := &v2Node{srv: restarted, addr: follower.addr, cfg: follower.cfg}
	restartedNode.client = v2Dial(t, follower.addr)
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = restarted.Stop(sctx)
	})

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		st := v2Status(t, restartedNode.client)
		if st != nil && st.LastApplied >= snap.Index {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st := v2Status(t, restartedNode.client); st == nil || st.LastApplied < snap.Index {
		t.Fatalf("follower did not catch up through snapshot: %+v (snapshot index %d)", st, snap.Index)
	}

	// Kill the old leader and read the committed value from the new leader.
	if err := leader.srv.Stop(stopCtx); err != nil {
		t.Fatalf("stop leader: %v", err)
	}
	survivors := []*v2Node{restartedNode}
	for _, n := range nodes {
		if n != leader && n != follower {
			survivors = append(survivors, n)
		}
	}
	newLeader := v2WaitLeader(t, survivors)
	gr, err := newLeader.client.Get(ctx, &pbv2.GetRequest{Key: []byte("snap-key"), Ts: commitTS})
	if err != nil {
		t.Fatalf("get after snapshot catch-up: %v", err)
	}
	if !gr.GetFound() || string(gr.GetValue()) != "snap-value" {
		t.Fatalf("snapshot catch-up lost value: found=%v value=%q err=%q", gr.GetFound(), gr.GetValue(), gr.GetError())
	}
}

func TestV2TwoRangesRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	addrs := []string{
		v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t),
		v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t),
	}
	ranges := []meta.Range{
		{ID: 1, Start: nil, End: []byte("m"), Replicas: addrs[0:3], Version: 1},
		{ID: 2, Start: []byte("m"), End: nil, Replicas: addrs[3:6], Version: 1},
	}
	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Ranges: ranges, Logger: v2Logger()})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("meta start: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	// Two independent Raft groups, one range each, in one process.
	nodes := make([]*v2Node, 0, len(addrs))
	for i, addr := range addrs {
		groupStart := 0
		if i >= 3 {
			groupStart = 3
		}
		peers := make([]string, 0, 2)
		for j := groupStart; j < groupStart+3; j++ {
			if j != i {
				peers = append(peers, addrs[j])
			}
		}
		cfg := kvserver.Config{
			ID: addr, RaftAddr: addr, DataDir: t.TempDir(), Peers: peers,
			RangeID:            uint64(1 + i/3),
			ElectionTimeoutMin: 150 * time.Millisecond,
			ElectionTimeoutMax: 300 * time.Millisecond,
			HeartbeatInterval:  30 * time.Millisecond,
			CacheSize:          8 << 20, MemTableSize: 4 << 20,
			Logger: v2Logger(),
		}
		if i < 3 {
			cfg.RangeEnd = []byte("m")
		} else {
			cfg.RangeStart = []byte("m")
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

	waitRangeLeader := func(group []*v2Node, rangeID uint64) *v2Node {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			for _, n := range group {
				st := v2Status(t, n.client)
				if st != nil && strings.EqualFold(st.Role, "leader") && st.GetRangeId() == rangeID {
					return n
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("no leader for range %d", rangeID)
		return nil
	}
	r1 := nodes[0:3]
	r2 := nodes[3:6]
	waitRangeLeader(r1, 1)
	waitRangeLeader(r2, 2)

	vc, err := v2client.New(metaAddr, addrs)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = vc.Close() })

	put := func(key, value string) {
		t.Helper()
		txn, err := vc.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		txn.Set([]byte(key), []byte(value))
		if err := txn.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", key, err)
		}
		read, err := vc.Begin(ctx)
		if err != nil {
			t.Fatalf("read begin: %v", err)
		}
		got, err := read.Get(ctx, []byte(key))
		if err != nil || string(got) != value {
			t.Fatalf("get %s = %q, %v", key, got, err)
		}
	}

	put("alpha", "one")
	put("zulu", "two")

	// Kill range-1 leader and verify a new transaction routes to the new leader.
	old := waitRangeLeader(r1, 1)
	if err := old.srv.Stop(context.Background()); err != nil {
		t.Fatalf("stop range-1 leader: %v", err)
	}
	var survivors []*v2Node
	for _, n := range r1 {
		if n != old {
			survivors = append(survivors, n)
		}
	}
	waitRangeLeader(survivors, 1)
	put("bravo", "three")

	// Range 1 needs two replicas; the failed node is gone, so the transaction
	// above proves the client refreshed its leader cache and routing.
}

func TestV2LockResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Logger: v2Logger()})
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("meta start: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })
	metaCC, err := grpc.NewClient(metaAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("meta dial: %v", err)
	}
	t.Cleanup(func() { _ = metaCC.Close() })
	metaClient := pbv2.NewMetaServiceClient(metaCC)

	addr := v2FreeAddr(t)
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
	client := v2Dial(t, addr)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.Raft().Status().Role != "leader" {
		time.Sleep(20 * time.Millisecond)
	}
	if srv.Raft().Status().Role != "leader" {
		t.Fatal("no leader")
	}

	ts := func() (uint64, uint64) {
		r, err := metaClient.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 2})
		if err != nil {
			t.Fatalf("ts: %v", err)
		}
		return r.Start, r.Start + 1
	}

	start, commit := ts()
	pr, err := client.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: start, Primary: []byte("lk"),
		Mutations: []*pbv2.Mutation{{Key: []byte("lk"), Value: []byte("v")}},
	})
	if err != nil || !pr.GetSuccess() {
		t.Fatalf("prewrite: %v %+v", err, pr)
	}
	st, err := client.CheckTxnStatus(ctx, &pbv2.CheckTxnStatusRequest{Key: []byte("lk"), StartTs: start})
	if err != nil || st.GetCommitted() || st.GetRolledBack() {
		t.Fatalf("status after prewrite: %v %+v", err, st)
	}
	if _, err := client.ResolveLock(ctx, &pbv2.ResolveLockRequest{
		Keys: [][]byte{[]byte("lk")}, StartTs: start, CommitTs: commit,
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	gr, err := client.Get(ctx, &pbv2.GetRequest{Key: []byte("lk"), Ts: commit})
	if err != nil || !gr.GetFound() || string(gr.GetValue()) != "v" {
		t.Fatalf("get after resolve: %v %+v", err, gr)
	}
	st, err = client.CheckTxnStatus(ctx, &pbv2.CheckTxnStatusRequest{Key: []byte("lk"), StartTs: start})
	if err != nil || !st.GetCommitted() || st.GetCommitTs() != commit {
		t.Fatalf("committed status: %v %+v", err, st)
	}

	// Rollback path.
	start2, _ := ts()
	if _, err := client.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: start2, Primary: []byte("rb"),
		Mutations: []*pbv2.Mutation{{Key: []byte("rb"), Value: []byte("x")}},
	}); err != nil {
		t.Fatalf("prewrite2: %v", err)
	}
	if _, err := client.ResolveLock(ctx, &pbv2.ResolveLockRequest{
		Keys: [][]byte{[]byte("rb")}, StartTs: start2, CommitTs: 0,
	}); err != nil {
		t.Fatalf("rollback resolve: %v", err)
	}
	if st, err := client.CheckTxnStatus(ctx, &pbv2.CheckTxnStatusRequest{Key: []byte("rb"), StartTs: start2}); err != nil || !st.GetRolledBack() {
		t.Fatalf("rolled back status: %v %+v", err, st)
	}

	// The transactional client exposes the same lock API for SQL/operators.
	vc, err := v2client.New(metaAddr, []string{addr})
	if err != nil {
		t.Fatalf("v2 client: %v", err)
	}
	t.Cleanup(func() { _ = vc.Close() })
	start3, commit3 := ts()
	if _, err := client.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: start3, Primary: []byte("vc"),
		Mutations: []*pbv2.Mutation{{Key: []byte("vc"), Value: []byte("ok")}},
	}); err != nil {
		t.Fatalf("prewrite vc: %v", err)
	}
	st, err = vc.CheckTxnStatus(ctx, []byte("vc"), start3)
	if err != nil || st.GetCommitted() || st.GetRolledBack() {
		t.Fatalf("client status: %v %+v", err, st)
	}
	if err := vc.ResolveLock(ctx, [][]byte{[]byte("vc")}, start3, commit3); err != nil {
		t.Fatalf("client resolve: %v", err)
	}
	gr, err = client.Get(ctx, &pbv2.GetRequest{Key: []byte("vc"), Ts: commit3})
	if err != nil || !gr.GetFound() || string(gr.GetValue()) != "ok" {
		t.Fatalf("client resolve get: %v %+v", err, gr)
	}
}
