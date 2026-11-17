package kvserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestJointE2EMembershipAndSnapshot is the end-to-end counterpart of the Raft
// joint-consensus tests: it drives a real three-process KV cluster over gRPC
// through AddReplica -> AddLearner -> PromoteReplica -> RemoveVoter, and forces
// the learner to catch up from a compacted leader so the snapshot travels
// through the real RaftService InstallSnapshot/InstallSnapshotChunk path.
//
// The cluster starts with two voters (A, B). C is started without ranges and
// is added as a learner. C's initial log is empty and the leader compacts its
// log before registering C, so C cannot catch up from the log suffix: the only
// way it can join is by installing the leader's snapshot. The test verifies the
// installed snapshot's membership (no removed voter is resurrected), that C
// applies the configuration entries appended after the snapshot, that a write
// after promotion is visible on every replica, and that removing a voter leaves
// a working two-voter cluster whose writes still replicate.
func TestJointE2EMembershipAndSnapshot(t *testing.T) {
	addrs := []string{testFreeAddr(t), testFreeAddr(t), testFreeAddr(t)}
	a, b, c := addrs[0], addrs[1], addrs[2]

	servers := make(map[string]*Server, 3)
	clients := make(map[string]pbv2.KVServiceClient, 3)

	start := func(id string, cfg Config) {
		t.Helper()
		srv, err := New(cfg)
		if err != nil {
			t.Fatalf("New(%s): %v", id, err)
		}
		if err := srv.Start(context.Background()); err != nil {
			t.Fatalf("Start(%s): %v", id, err)
		}
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Stop(stopCtx)
		})
		servers[id] = srv
		cc, err := grpc.NewClient(id, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("dial %s: %v", id, err)
		}
		t.Cleanup(func() { _ = cc.Close() })
		clients[id] = pbv2.NewKVServiceClient(cc)
	}

	base := func(id string) Config {
		return Config{
			ID:                 id,
			RaftAddr:           id,
			DataDir:            t.TempDir(),
			ElectionTimeoutMin: 150 * time.Millisecond,
			ElectionTimeoutMax: 300 * time.Millisecond,
			HeartbeatInterval:  25 * time.Millisecond,
			CacheSize:          8 << 20,
			MemTableSize:       4 << 20,
			Logger:             testLogger(),
			// The test drives snapshots explicitly; background lifecycle loops
			// would make the snapshot index nondeterministic.
			DisableBackgroundLifecycle: true,
		}
	}

	cfgA := base(a)
	cfgA.Ranges = []RangeConfig{{ID: 1, Peers: []string{b}, Epoch: 1}}
	start(a, cfgA)

	cfgB := base(b)
	cfgB.Ranges = []RangeConfig{{ID: 1, Peers: []string{a}, Epoch: 1}}
	start(b, cfgB)

	cfgC := base(c)
	cfgC.NoRanges = true
	start(c, cfgC)

	leaderAddr := e2eWaitLeader(t, servers, []string{a, b}, 10*time.Second)
	t.Logf("initial leader: %s", leaderAddr)

	// Seed data, then compact the leader below the point where C will start.
	for i := 0; i < 8; i++ {
		key := e2eKey("seed", i)
		e2eCommitSingle(t, servers, clients, []string{a, b}, key, "seed-value", uint64(1000+i*10))
	}
	leaderNode, err := servers[leaderAddr].GroupForID(1)
	if err != nil {
		t.Fatalf("leader GroupForID: %v", err)
	}
	appliedBefore := leaderNode.Status().LastApplied
	e2eEventually(t, 10*time.Second, "leader applies seed writes", func() bool {
		return leaderNode.Status().LastApplied >= appliedBefore
	})
	snap, err := leaderNode.Snapshot()
	if err != nil {
		t.Fatalf("leader Snapshot: %v", err)
	}
	if snap.Index == 0 || snap.Membership == nil {
		t.Fatalf("leader snapshot = {index %d, membership %+v}, want positive index and membership", snap.Index, snap.Membership)
	}
	if !e2eSetEqual(snap.Membership.Voters, []string{a, b}) || len(snap.Membership.Learners) != 0 {
		t.Fatalf("snapshot membership = voters %v learners %v, want voters [%s %s] and no learners",
			snap.Membership.Voters, snap.Membership.Learners, a, b)
	}

	// AddReplica creates C's range as a learner with an empty log.
	addResp, err := clients[c].AddReplica(context.Background(), &pbv2.AddReplicaRequest{
		RangeId: 1, Target: c, Peers: []string{a, b}, Epoch: 1,
	})
	if err != nil || !addResp.GetSuccess() {
		t.Fatalf("AddReplica(%s): %v %+v", c, err, addResp)
	}

	// Register C on the leader through the gRPC membership path.
	resp := e2eMembershipCall(t, servers, clients, []string{a, b}, "AddLearner",
		func(client pbv2.KVServiceClient, ctx context.Context) (*pbv2.MembershipResponse, error) {
			return client.AddLearner(ctx, &pbv2.AddLearnerRequest{RangeId: 1, Target: c})
		})
	if !containsString(resp.GetLearners(), c) {
		t.Fatalf("AddLearner response learners = %v, want %s", resp.GetLearners(), c)
	}

	// C must install the snapshot. Its log store is the authoritative local
	// evidence: an empty-log learner cannot reach an index below the leader's
	// compacted prefix without persisting that snapshot.
	cg, err := servers[c].groupForID(1)
	if err != nil {
		t.Fatalf("C groupForID: %v", err)
	}
	e2eEventually(t, 15*time.Second, "C installs the leader snapshot", func() bool {
		meta, rc, err := cg.logStore.OpenSnapshot()
		if rc != nil {
			_ = rc.Close()
		}
		return err == nil && meta.Index >= snap.Index
	})
	meta, rc, err := cg.logStore.OpenSnapshot()
	if err != nil {
		t.Fatalf("C OpenSnapshot: %v", err)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if meta.Membership == nil {
		t.Fatal("C installed a snapshot without membership")
	}
	if !e2eSetEqual(meta.Membership.Voters, []string{a, b}) {
		t.Fatalf("installed snapshot voters = %v, want %v", meta.Membership.Voters, []string{a, b})
	}

	// C applies the AddLearner entries that follow the snapshot, so its final
	// membership includes itself as a learner.
	e2eEventually(t, 15*time.Second, "C applies its learner membership", func() bool {
		voters, learners := cg.node.Membership()
		return e2eSetEqual(voters, []string{a, b}) && containsString(learners, c)
	})

	// Promote C. All three nodes must converge on {A,B,C}.
	resp = e2eMembershipCall(t, servers, clients, []string{a, b, c}, "PromoteReplica",
		func(client pbv2.KVServiceClient, ctx context.Context) (*pbv2.MembershipResponse, error) {
			return client.PromoteReplica(ctx, &pbv2.PromoteReplicaRequest{RangeId: 1, Target: c})
		})
	if !containsString(resp.GetVoters(), c) {
		t.Fatalf("PromoteReplica response voters = %v, want %s", resp.GetVoters(), c)
	}
	e2eWaitMembership(t, servers, []string{a, b, c}, []string{a, b, c}, nil, 15*time.Second)

	// A write after promotion must be visible on every replica. The local MVCC
	// read bypasses the leader-only ReadBarrier so it proves replication, not
	// just a successful RPC.
	e2eCommitSingle(t, servers, clients, []string{a, b, c}, "after-promote", "promoted", 5000)
	e2eWaitReplicated(t, servers, []string{a, b, c}, "after-promote", "promoted", 5001, 15*time.Second)

	// Remove one original voter, preferring a non-leader so the remaining
	// cluster keeps a live leader. Removing the leader is covered by the Raft
	// tests; here the goal is the gRPC path and the post-removal write.
	leaderAddr = e2eWaitLeader(t, servers, []string{a, b, c}, 10*time.Second)
	victim := a
	if leaderAddr == a {
		victim = b
	}
	t.Logf("removing voter %s (leader %s)", victim, leaderAddr)
	resp = e2eMembershipCall(t, servers, clients, []string{a, b, c}, "RemoveVoter",
		func(client pbv2.KVServiceClient, ctx context.Context) (*pbv2.MembershipResponse, error) {
			return client.RemoveVoter(ctx, &pbv2.RemoveVoterRequest{RangeId: 1, Target: victim})
		})
	remaining := e2eWithout(addrs, victim)
	if !e2eSetEqual(resp.GetVoters(), remaining) {
		t.Fatalf("RemoveVoter response voters = %v, want %v", resp.GetVoters(), remaining)
	}
	e2eWaitMembership(t, servers, remaining, remaining, nil, 15*time.Second)

	// Writes continue after the removal.
	e2eCommitSingle(t, servers, clients, remaining, "after-remove", "still-writes", 6000)
	e2eWaitReplicated(t, servers, remaining, "after-remove", "still-writes", 6001, 15*time.Second)

	// The read path over gRPC sees the final value through the current leader.
	leaderAddr = e2eWaitLeader(t, servers, remaining, 10*time.Second)
	getResp := e2eGet(t, clients[leaderAddr], "after-remove", 6001)
	if !getResp.GetFound() || string(getResp.GetValue()) != "still-writes" {
		t.Fatalf("Get(after-remove) = %q found=%v error=%q", getResp.GetValue(), getResp.GetFound(), getResp.GetError())
	}

	// Membership RPC on the remaining replicas reports the post-removal set.
	for _, id := range remaining {
		m, err := clients[id].Membership(context.Background(), &pbv2.MembershipRequest{RangeId: 1})
		if err != nil {
			t.Fatalf("Membership(%s): %v", id, err)
		}
		if !e2eSetEqual(m.GetVoters(), remaining) {
			t.Fatalf("Membership(%s) voters = %v, want %v", id, m.GetVoters(), remaining)
		}
		if containsString(m.GetVoters(), victim) || containsString(m.GetLearners(), victim) {
			t.Fatalf("Membership(%s) still contains removed voter %s: voters=%v learners=%v",
				id, victim, m.GetVoters(), m.GetLearners())
		}
	}
}

// ---------------------------------------------------------------------------
// gRPC helpers
// ---------------------------------------------------------------------------

func e2eKey(prefix string, i int) string {
	return prefix + "-" + string(rune('a'+i))
}

func e2eWithout(in []string, drop string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}

func e2eSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, v := range a {
		seen[v] = true
	}
	for _, v := range b {
		if !seen[v] {
			return false
		}
	}
	return true
}

func e2eEventually(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !fn() {
		t.Fatalf("condition not met within %v: %s", timeout, what)
	}
}

func e2eWaitLeader(t *testing.T, servers map[string]*Server, ids []string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var leader string
		count := 0
		for _, id := range ids {
			node, err := servers[id].GroupForID(1)
			if err != nil {
				continue
			}
			if node.Status().Role == raft.Leader {
				count++
				leader = id
			}
		}
		if count == 1 {
			return leader
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("range 1 did not elect a single leader among %v within %v", ids, timeout)
	return ""
}

func e2eMembershipCall(
	t *testing.T,
	servers map[string]*Server,
	clients map[string]pbv2.KVServiceClient,
	ids []string,
	name string,
	fn func(pbv2.KVServiceClient, context.Context) (*pbv2.MembershipResponse, error),
) *pbv2.MembershipResponse {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		leader := e2eWaitLeader(t, servers, ids, 5*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := fn(clients[leader], ctx)
		cancel()
		if err != nil {
			last = err.Error()
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if resp.GetSuccess() {
			return resp
		}
		last = resp.GetError()
		if !strings.Contains(last, "not leader") && !strings.Contains(last, "timeout") &&
			!strings.Contains(last, "membership change in progress") {
			t.Fatalf("%s on %s: %s", name, leader, last)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not succeed within 15s: %s", name, last)
	return nil
}

func e2eCommitSingle(
	t *testing.T,
	servers map[string]*Server,
	clients map[string]pbv2.KVServiceClient,
	ids []string,
	key, value string,
	startTS uint64,
) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		leader := e2eWaitLeader(t, servers, ids, 5*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := clients[leader].CommitSingle(ctx, commitSingleMutation(key, value, startTS, startTS+1))
		cancel()
		if err != nil {
			last = err.Error()
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if resp.GetSuccess() {
			return
		}
		last = resp.GetError()
		if !strings.Contains(last, "not leader") && !strings.Contains(last, "timeout") {
			t.Fatalf("CommitSingle(%s) on %s: %s", key, leader, last)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("CommitSingle(%s) did not succeed within 15s: %s", key, last)
}

func e2eWaitMembership(
	t *testing.T,
	servers map[string]*Server,
	ids []string,
	wantVoters, wantLearners []string,
	timeout time.Duration,
) {
	t.Helper()
	e2eEventually(t, timeout, "membership converges", func() bool {
		for _, id := range ids {
			node, err := servers[id].GroupForID(1)
			if err != nil {
				return false
			}
			st := node.Status()
			if st.Joint || !e2eSetEqual(st.Voters, wantVoters) || !e2eSetEqual(st.Learners, wantLearners) {
				return false
			}
		}
		return true
	})
}

func e2eWaitReplicated(
	t *testing.T,
	servers map[string]*Server,
	ids []string,
	key, value string,
	ts uint64,
	timeout time.Duration,
) {
	t.Helper()
	e2eEventually(t, timeout, "all replicas apply "+key, func() bool {
		for _, id := range ids {
			g, err := servers[id].groupForID(1)
			if err != nil {
				return false
			}
			got, err := g.store.Get([]byte(key), ts)
			if err != nil || string(got) != value {
				return false
			}
		}
		return true
	})
}

func e2eGet(t *testing.T, client pbv2.KVServiceClient, key string, ts uint64) *pbv2.GetResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last *pbv2.GetResponse
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := client.Get(ctx, &pbv2.GetRequest{Key: []byte(key), Ts: ts})
		cancel()
		if err == nil && !strings.Contains(resp.GetError(), "timeout") &&
			!strings.Contains(resp.GetError(), "not leader") {
			return resp
		}
		if resp != nil {
			last = resp
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last != nil {
		return last
	}
	t.Fatalf("Get(%s) did not complete", key)
	return nil
}
