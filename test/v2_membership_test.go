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

// v2StartMembershipCluster starts count KV nodes where only the first
// replicas form the initial range-1 replica set. The remaining nodes start
// empty (NoRanges) and are added through metaserver.AddReplica.
func v2StartMembershipCluster(t *testing.T, count, replicas int) ([]*v2Node, *metaserver.Server, pbv2.MetaServiceClient) {
	t.Helper()
	if replicas <= 0 || replicas > count {
		t.Fatalf("invalid replicas=%d count=%d", replicas, count)
	}
	ctx := context.Background()
	addrs := make([]string, count)
	for i := range addrs {
		addrs[i] = v2FreeAddr(t)
	}
	voters := addrs[:replicas]

	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{
		Addr: metaAddr,
		Ranges: []meta.Range{{
			ID:       1,
			Replicas: append([]string(nil), voters...),
			Version:  1,
		}},
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
		cfg := kvserver.Config{
			ID:                 addr,
			RaftAddr:           addr,
			DataDir:            t.TempDir(),
			ElectionTimeoutMin: 150 * time.Millisecond,
			ElectionTimeoutMax: 300 * time.Millisecond,
			HeartbeatInterval:  30 * time.Millisecond,
			CacheSize:          8 << 20,
			MemTableSize:       4 << 20,
			Logger:             v2Logger(),
		}
		if i < replicas {
			peers := make([]string, 0, replicas-1)
			for _, voter := range voters {
				if voter != addr {
					peers = append(peers, voter)
				}
			}
			cfg.Ranges = []kvserver.RangeConfig{{ID: 1, Peers: peers, Epoch: 1}}
		} else {
			cfg.NoRanges = true
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

func v2RangeRows(t *testing.T, meta pbv2.MetaServiceClient) []*pbv2.RangeInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := meta.ListRanges(ctx, &pbv2.ListRangesRequest{})
	if err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	return resp.GetRanges()
}

func v2RangeByID(t *testing.T, meta pbv2.MetaServiceClient, id uint64) *pbv2.RangeInfo {
	t.Helper()
	for _, r := range v2RangeRows(t, meta) {
		if r.GetId() == id {
			return r
		}
	}
	t.Fatalf("range %d not found in meta", id)
	return nil
}

func v2Get(t *testing.T, meta pbv2.MetaServiceClient, node *v2Node, key, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ts, err := meta.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 1})
	if err != nil {
		t.Fatalf("alloc ts: %v", err)
	}
	resp, err := node.client.Get(ctx, &pbv2.GetRequest{Key: []byte(key), Ts: ts.GetStart()})
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	if resp.GetError() != "" {
		t.Fatalf("get %q: %s (leader=%s)", key, resp.GetError(), resp.GetLeader())
	}
	if !resp.GetFound() || string(resp.GetValue()) != want {
		t.Fatalf("get %q = %q found=%v, want %q", key, resp.GetValue(), resp.GetFound(), want)
	}
}

// v2WaitApplied waits until node has applied through index.
func v2WaitApplied(t *testing.T, node *v2Node, index uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := v2RangeStatus(t, node.client, 1)
		if st != nil && st.GetLastApplied() >= index {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not apply through index %d", node.addr, index)
}

func TestV2ReplicaMembershipE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const replicas = 3
	nodes, ms, metaClient := v2StartMembershipCluster(t, 4, replicas)
	target := nodes[3]

	leader := v2WaitRangeLeader(t, nodes[:replicas], 1)
	v2Put(t, metaClient, leader, "membership-before", "v1")

	// Add the fourth node: create group -> learner -> catch up -> promote.
	if err := ms.AddReplica(ctx, 1, target.addr); err != nil {
		t.Fatalf("AddReplica(%s): %v", target.addr, err)
	}
	r := v2RangeByID(t, metaClient, 1)
	if len(r.GetReplicas()) != replicas+1 || !containsString(r.GetReplicas(), target.addr) {
		t.Fatalf("meta replicas after add = %v, want %d including %s", r.GetReplicas(), replicas+1, target.addr)
	}
	if r.GetVersion() <= 1 {
		t.Fatalf("meta version after add = %d, want > 1", r.GetVersion())
	}

	st := v2RangeStatus(t, target.client, 1)
	if st == nil {
		t.Fatalf("target %s does not host range 1 after AddReplica", target.addr)
	}
	if st.GetLearner() || len(st.GetLearners()) != 0 {
		t.Fatalf("target status after promotion = %+v, want a voter", st)
	}
	if !containsString(st.GetVoters(), target.addr) || len(st.GetVoters()) != replicas+1 {
		t.Fatalf("target voters = %v, want %d including %s", st.GetVoters(), replicas+1, target.addr)
	}
	// Catch-up: the promoted target has applied the leader's commit index.
	leader = v2WaitRangeLeader(t, nodes[:replicas+1], 1)
	v2WaitApplied(t, target, v2RangeStatus(t, leader.client, 1).GetCommitIndex())

	// Prove the new voter holds the data: move leadership to it and read.
	resp, err := leader.client.TransferLeader(ctx, &pbv2.TransferLeaderRequest{
		RangeId: 1, Target: target.addr, Epoch: r.GetVersion(),
	})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("TransferLeader to new voter: %v %+v", err, resp)
	}
	v2WaitLeaderOn(t, nodes[:replicas+1], 1, target.addr)
	v2Get(t, metaClient, target, "membership-before", "v1")

	// Kill one of the original voters. The remaining two voters plus the new
	// voter form a healthy quorum.
	var victim *v2Node
	for _, n := range nodes[:replicas] {
		if n.addr != target.addr {
			victim = n
			break
		}
	}
	if victim == nil {
		t.Fatal("no original voter to remove")
	}
	if err := victim.srv.Stop(ctx); err != nil {
		t.Fatalf("stop victim %s: %v", victim.addr, err)
	}
	live := make([]*v2Node, 0, replicas)
	for _, n := range nodes[:replicas+1] {
		if n.addr != victim.addr {
			live = append(live, n)
		}
	}
	leader = v2WaitRangeLeader(t, live, 1)
	v2Get(t, metaClient, leader, "membership-before", "v1")

	// Remove the killed voter from the replica set and from Raft.
	if err := ms.RemoveReplica(ctx, 1, victim.addr); err != nil {
		t.Fatalf("RemoveReplica(%s): %v", victim.addr, err)
	}
	r = v2RangeByID(t, metaClient, 1)
	if len(r.GetReplicas()) != replicas || containsString(r.GetReplicas(), victim.addr) {
		t.Fatalf("meta replicas after remove = %v, want %d without %s", r.GetReplicas(), replicas, victim.addr)
	}
	if r.GetVersion() <= 2 {
		t.Fatalf("meta version after remove = %d, want > 2", r.GetVersion())
	}

	// Data is intact and the range still serves writes through the remaining
	// voters.
	v2Get(t, metaClient, leader, "membership-before", "v1")
	v2Put(t, metaClient, leader, "membership-after", "v2")
	v2Get(t, metaClient, leader, "membership-after", "v2")
}

func TestV2LearnerCatchesUpFromCompactedLog(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const replicas = 3
	nodes, ms, metaClient := v2StartMembershipCluster(t, 4, replicas)
	target := nodes[3]
	leader := v2WaitRangeLeader(t, nodes[:replicas], 1)

	for i := 0; i < 3; i++ {
		v2Put(t, metaClient, leader, "snapshot-"+string(rune('a'+i)), "value")
	}
	// Compact the leader's log so the new learner must be bootstrapped from a
	// snapshot rather than from the log prefix.
	if _, err := leader.srv.Snapshot(); err != nil {
		t.Fatalf("leader snapshot: %v", err)
	}

	if err := ms.AddReplica(ctx, 1, target.addr); err != nil {
		t.Fatalf("AddReplica after compaction: %v", err)
	}
	st := v2RangeStatus(t, target.client, 1)
	if st == nil || st.GetLearner() {
		t.Fatalf("target status after add = %+v, want installed voter", st)
	}

	// Move leadership to the target and read a pre-snapshot value from it.
	resp, err := leader.client.TransferLeader(ctx, &pbv2.TransferLeaderRequest{
		RangeId: 1, Target: target.addr,
	})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("TransferLeader to snapshot-caught-up voter: %v %+v", err, resp)
	}
	v2WaitLeaderOn(t, nodes[:replicas+1], 1, target.addr)
	v2Get(t, metaClient, target, "snapshot-a", "value")
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if strings.TrimSpace(v) == want {
			return true
		}
	}
	return false
}
