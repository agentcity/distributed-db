package raft_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

// testTransferTransport augments the in-memory Raft network with the optional
// TimeoutNow capability by delivering the RPC directly to the peer node. The
// production path uses the same interface over gRPC in internal/kvserver.
type testTransferTransport struct {
	raft.Transport
	cluster *testCluster
	id      string
}

func (t *testTransferTransport) TimeoutNow(ctx context.Context, peer string, req *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	node := t.cluster.nodes[peer]
	if node == nil {
		return nil, fmt.Errorf("test transport: unknown peer %s", peer)
	}
	return node.HandleTimeoutNow(ctx, req)
}

// InstallSnapshot delivers the legacy unary snapshot directly to the peer so
// membership tests can exercise learner catch-up after log compaction.
func (t *testTransferTransport) InstallSnapshot(ctx context.Context, peer string, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	node := t.cluster.nodes[peer]
	if node == nil {
		return nil, fmt.Errorf("test transport: unknown peer %s", peer)
	}
	return node.HandleInstallSnapshot(ctx, req)
}

var _ raft.TimeoutNowTransport = (*testTransferTransport)(nil)
var _ raft.SnapshotTransport = (*testTransferTransport)(nil)

func otherID(ids []string, id string) string {
	for _, other := range ids {
		if other != id {
			return other
		}
	}
	return ""
}

func TestTransferLeadershipMovesLeadership(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 500*time.Millisecond, time.Second, 50*time.Millisecond)
	leader, leaderID := c.waitLeader(c.ids, testWait)
	target := otherID(c.ids, leaderID)

	// Make sure every node has applied at least one real entry, so the target
	// is fully caught up with the leader's log.
	c.proposeWithRetry(c.ids, model.Entry{Type: model.CommandPut, Key: "before", Value: []byte("transfer")}, testWait)
	c.requireValue(c.ids, "before", "transfer", testWait)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := leader.TransferLeadership(ctx, target); err != nil {
		t.Fatalf("TransferLeadership(%s): %v", target, err)
	}
	if leader.IsLeader() {
		t.Fatalf("%s is still leader after transferring to %s", leaderID, target)
	}

	// Under scheduling pressure a third node may win the election that follows
	// the leader's step-down. Transfer again from that leader until the intended
	// target wins; the API guarantee is "this target becomes leader", not
	// "no other node ever campaigns".
	var newLeader *raft.Node
	var newID string
	for attempt := 0; attempt < 3; attempt++ {
		newLeader, newID = c.waitLeader(c.ids, 3*time.Second)
		if newID == target {
			break
		}
		if err := newLeader.TransferLeadership(ctx, target); err != nil {
			t.Fatalf("retry TransferLeadership(%s) from %s: %v", target, newID, err)
		}
	}
	if newID != target {
		t.Fatalf("new leader = %s, want transfer target %s", newID, target)
	}
	if !newLeader.IsLeader() {
		t.Fatalf("%s reported leader but IsLeader is false", newID)
	}

	// The cluster keeps accepting writes after the handover.
	c.proposeWithRetry(c.ids, model.Entry{Type: model.CommandPut, Key: "after", Value: []byte("transfer")}, testWait)
	c.requireValue(c.ids, "after", "transfer", testWait)
}

func TestTransferLeadershipRejectsUnknownTarget(t *testing.T) {
	c := newTestCluster(t, 3)
	leader, leaderID := c.waitLeader(c.ids, testWait)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := leader.TransferLeadership(ctx, "not-a-peer")
	if !errors.Is(err, raft.ErrUnknownTarget) {
		t.Fatalf("TransferLeadership(unknown) = %v, want ErrUnknownTarget", err)
	}
	if !leader.IsLeader() {
		t.Fatalf("%s stepped down after a rejected transfer", leaderID)
	}

	if err := leader.TransferLeadership(ctx, leaderID); !errors.Is(err, raft.ErrUnknownTarget) {
		t.Fatalf("TransferLeadership(self) = %v, want ErrUnknownTarget", err)
	}
}

func TestTransferLeadershipRejectsUncaughtUpTarget(t *testing.T) {
	// Longer election timeouts keep the leader stable while the target is
	// partitioned; otherwise a loaded CI machine can schedule an election
	// during the 150ms catch-up wait and the call returns ErrNotLeader.
	c := newTestClusterWithTimeouts(t, 3, 500*time.Millisecond, time.Second, 50*time.Millisecond)
	leader, leaderID := c.waitLeader(c.ids, testWait)
	target := otherID(c.ids, leaderID)

	// Isolate one follower in both directions. The remaining majority still
	// commits entries while the target's match index stays behind.
	c.net.Partition(target, peersExcept(c.ids, target)...)
	for _, id := range c.ids {
		if id != target {
			c.net.Partition(id, target)
		}
	}
	c.proposeWithRetry(c.ids, model.Entry{Type: model.CommandPut, Key: "behind-1", Value: []byte("x")}, testWait)
	c.proposeWithRetry(c.ids, model.Entry{Type: model.CommandPut, Key: "behind-2", Value: []byte("y")}, testWait)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := leader.TransferLeadership(ctx, target)
	if !errors.Is(err, raft.ErrTargetNotCaughtUp) {
		t.Fatalf("TransferLeadership(lagging %s) = %v, want ErrTargetNotCaughtUp", target, err)
	}
	if !leader.IsLeader() {
		t.Fatalf("%s stepped down after a not-caught-up transfer", leaderID)
	}
}

func TestHandleTimeoutNowRejectsStaleTerm(t *testing.T) {
	c := newTestCluster(t, 3)
	_, leaderID := c.waitLeader(c.ids, testWait)
	followerID := otherID(c.ids, leaderID)
	follower := c.nodes[followerID]
	st := follower.Status()
	if st.Leader != leaderID {
		t.Fatalf("follower %s leader = %q, want %q", followerID, st.Leader, leaderID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	resp, err := follower.HandleTimeoutNow(ctx, &raft.TimeoutNowRequest{Term: st.Term - 1, LeaderID: leaderID})
	if err != nil {
		t.Fatalf("HandleTimeoutNow(stale): %v", err)
	}
	if resp.Success || resp.Term != st.Term {
		t.Fatalf("stale TimeoutNow response = %+v, want success=false term=%d", resp, st.Term)
	}
	if got := follower.Status().Term; got != st.Term {
		t.Fatalf("stale TimeoutNow changed term from %d to %d", st.Term, got)
	}

	resp, err = follower.HandleTimeoutNow(ctx, &raft.TimeoutNowRequest{Term: st.Term + 1, LeaderID: leaderID})
	if err != nil {
		t.Fatalf("HandleTimeoutNow(future): %v", err)
	}
	if resp.Success || resp.Term != st.Term {
		t.Fatalf("future-term TimeoutNow response = %+v, want success=false term=%d", resp, st.Term)
	}
	if got := follower.Status().Term; got != st.Term {
		t.Fatalf("future-term TimeoutNow changed term from %d to %d", st.Term, got)
	}
	if follower.IsLeader() {
		t.Fatal("a rejected TimeoutNow must not make the follower a leader")
	}
}

func TestHandleTimeoutNowStartsElection(t *testing.T) {
	c := newTestCluster(t, 3)
	_, leaderID := c.waitLeader(c.ids, testWait)
	followerID := otherID(c.ids, leaderID)
	follower := c.nodes[followerID]
	st := follower.Status()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := follower.HandleTimeoutNow(ctx, &raft.TimeoutNowRequest{Term: st.Term, LeaderID: leaderID})
	if err != nil {
		t.Fatalf("HandleTimeoutNow: %v", err)
	}
	if !resp.Success {
		t.Fatalf("HandleTimeoutNow = %+v, want success", resp)
	}

	// The caught-up follower should win the election it was forced to start.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if follower.IsLeader() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("forced candidate %s did not become leader (status %+v)", followerID, follower.Status())
}
