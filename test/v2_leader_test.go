package test

import (
	"context"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// v2StartLeaderCluster starts a standalone meta server and count KV nodes, each
// hosting the same ranges (rangeSpecs) with every node as a replica. It returns
// the nodes, the meta server and a MetaService client for TSO.
func v2StartLeaderCluster(t *testing.T, count int, rangeSpecs []kvserver.RangeConfig) ([]*v2Node, *metaserver.Server, pbv2.MetaServiceClient) {
	t.Helper()
	ctx := context.Background()
	addrs := make([]string, count)
	for i := range addrs {
		addrs[i] = v2FreeAddr(t)
	}

	metaRanges := make([]meta.Range, 0, len(rangeSpecs))
	for _, rc := range rangeSpecs {
		metaRanges = append(metaRanges, meta.Range{
			ID:       rc.ID,
			Start:    rc.Start,
			End:      rc.End,
			Replicas: append([]string(nil), addrs...),
			Version:  1,
		})
	}
	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{
		Addr:   metaAddr,
		Ranges: metaRanges,
		Logger: v2Logger(),
	})
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

	nodes := make([]*v2Node, 0, count)
	for i, addr := range addrs {
		peers := make([]string, 0, count-1)
		for j, peer := range addrs {
			if i != j {
				peers = append(peers, peer)
			}
		}
		ranges := make([]kvserver.RangeConfig, 0, len(rangeSpecs))
		for _, rc := range rangeSpecs {
			ranges = append(ranges, kvserver.RangeConfig{
				ID:    rc.ID,
				Start: rc.Start,
				End:   rc.End,
				Peers: peers,
				Epoch: 1,
			})
		}
		cfg := kvserver.Config{
			ID:                 addr,
			RaftAddr:           addr,
			DataDir:            t.TempDir(),
			Ranges:             ranges,
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
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Stop(stopCtx)
		})
		n := &v2Node{srv: srv, addr: addr, cfg: cfg}
		n.client = v2Dial(t, addr)
		nodes = append(nodes, n)
	}
	return nodes, ms, metaClient
}

func v2RangeStatus(t *testing.T, client pbv2.KVServiceClient, rangeID uint64) *pbv2.StatusResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := client.Status(ctx, &pbv2.StatusRequest{RangeId: rangeID})
	if err != nil {
		return nil
	}
	return resp
}

func v2WaitRangeLeader(t *testing.T, nodes []*v2Node, rangeID uint64) *v2Node {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			st := v2RangeStatus(t, n.client, rangeID)
			if st != nil && strings.EqualFold(st.GetRole(), "leader") {
				return n
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no leader for range %d", rangeID)
	return nil
}

func v2WaitLeaderOn(t *testing.T, nodes []*v2Node, rangeID uint64, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.addr != addr {
				continue
			}
			st := v2RangeStatus(t, n.client, rangeID)
			if st != nil && strings.EqualFold(st.GetRole(), "leader") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not become leader of range %d", addr, rangeID)
}

func v2Put(t *testing.T, meta pbv2.MetaServiceClient, node *v2Node, key, value string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ts, err := meta.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 2})
	if err != nil {
		t.Fatalf("alloc ts: %v", err)
	}
	startTS, commitTS := ts.GetStart(), ts.GetStart()+1

	pr, err := node.client.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: startTS,
		Primary: []byte(key),
		Mutations: []*pbv2.Mutation{{
			Key:   []byte(key),
			Value: []byte(value),
		}},
	})
	if err != nil || !pr.GetSuccess() {
		t.Fatalf("prewrite %q: %v %+v", key, err, pr)
	}
	cr, err := node.client.Commit(ctx, &pbv2.CommitRequest{
		StartTs:  startTS,
		CommitTs: commitTS,
		Keys:     [][]byte{[]byte(key)},
	})
	if err != nil || !cr.GetSuccess() {
		t.Fatalf("commit %q: %v %+v", key, err, cr)
	}
	gr, err := node.client.Get(ctx, &pbv2.GetRequest{Key: []byte(key), Ts: commitTS})
	if err != nil || !gr.GetFound() || string(gr.GetValue()) != value {
		t.Fatalf("get %q = %q, found=%v, err=%q (rpc %v)", key, gr.GetValue(), gr.GetFound(), gr.GetError(), err)
	}
}

func TestV2LeaderTransfer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nodes, _, metaClient := v2StartLeaderCluster(t, 3, []kvserver.RangeConfig{{ID: 1, Start: nil, End: nil}})
	oldLeader := v2WaitRangeLeader(t, nodes, 1)

	var target *v2Node
	for _, n := range nodes {
		if n.addr != oldLeader.addr {
			target = n
			break
		}
	}
	if target == nil {
		t.Fatal("no follower to transfer to")
	}

	resp, err := oldLeader.client.TransferLeader(ctx, &pbv2.TransferLeaderRequest{
		RangeId: 1,
		Target:  target.addr,
		Epoch:   1,
	})
	if err != nil {
		t.Fatalf("TransferLeader: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("TransferLeader failed: %+v", resp)
	}
	if resp.GetLeader() != target.addr {
		t.Fatalf("TransferLeader leader hint = %q, want %q", resp.GetLeader(), target.addr)
	}

	// The target wins the forced election and the old leader stays a follower.
	v2WaitLeaderOn(t, nodes, 1, target.addr)
	if st := v2RangeStatus(t, oldLeader.client, 1); st != nil && strings.EqualFold(st.GetRole(), "leader") {
		t.Fatalf("old leader %s still reports leader after transfer", oldLeader.addr)
	}

	// Client writes continue through the new leader.
	v2Put(t, metaClient, target, "leader-transfer", "ok")
	if st := v2RangeStatus(t, target.client, 1); st == nil || !strings.EqualFold(st.GetRole(), "leader") {
		t.Fatalf("target %s lost leadership after the write: %+v", target.addr, st)
	}
}

func TestV2LeaderRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	nodes, ms, _ := v2StartLeaderCluster(t, 3, []kvserver.RangeConfig{
		{ID: 1, Start: nil, End: []byte("m")},
		{ID: 2, Start: []byte("m"), End: nil},
	})
	leader1 := v2WaitRangeLeader(t, nodes, 1)
	leader2 := v2WaitRangeLeader(t, nodes, 2)

	if leader1.addr != leader2.addr {
		// Force both leaders onto one node so the scheduler has work to do.
		resp, err := leader2.client.TransferLeader(ctx, &pbv2.TransferLeaderRequest{
			RangeId: 2,
			Target:  leader1.addr,
			Epoch:   1,
		})
		if err != nil || !resp.GetSuccess() {
			t.Fatalf("consolidate leaders: %v %+v", err, resp)
		}
		v2WaitLeaderOn(t, nodes, 2, leader1.addr)
	}

	moved, err := ms.RebalanceLeaders(ctx)
	if err != nil {
		t.Fatalf("RebalanceLeaders: %v", err)
	}
	if moved == 0 {
		t.Fatal("RebalanceLeaders moved no leader from the loaded node")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		l1 := v2FindRangeLeader(nodes, 1)
		l2 := v2FindRangeLeader(nodes, 2)
		if l1 != "" && l2 != "" && l1 != l2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	l1 := v2FindRangeLeader(nodes, 1)
	l2 := v2FindRangeLeader(nodes, 2)
	t.Fatalf("leaders still collocated after rebalance: range1=%s range2=%s", l1, l2)
}

// v2FindRangeLeader returns the address of the current leader without failing;
// used while a transfer is settling.
func v2FindRangeLeader(nodes []*v2Node, rangeID uint64) string {
	for _, n := range nodes {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		st, err := n.client.Status(ctx, &pbv2.StatusRequest{RangeId: rangeID})
		cancel()
		if err == nil && strings.EqualFold(st.GetRole(), "leader") {
			return n.addr
		}
	}
	return ""
}
