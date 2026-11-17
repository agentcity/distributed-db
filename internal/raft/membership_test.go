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

// addNode starts one extra node in the existing in-memory network. peers is the
// voter set the node is created with (its own id is not included by the
// caller); learner starts it as a non-voting member.
func (c *testCluster) addNode(id string, peers []string, learner bool) *raft.Node {
	c.t.Helper()
	dir := c.t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		c.t.Fatalf("NewFileStore(%s): %v", id, err)
	}
	sm := newTestSM()
	tr := &testTransferTransport{Transport: c.net.Transport(id), cluster: c, id: id}
	cfg := c.configFor(id, peers, dir)
	cfg.Learner = learner
	node, err := raft.New(cfg, store, sm, tr)
	if err != nil {
		c.t.Fatalf("raft.New(%s): %v", id, err)
	}
	c.dirs[id] = dir
	c.stores[id] = store
	c.sms[id] = sm
	c.setNode(id, node)
	c.net.Register(id, node)
	if err := node.Start(c.ctx); err != nil {
		c.t.Fatalf("Start(%s): %v", id, err)
	}
	return node
}

// retryableMembershipErr reports errors that a new leader can resolve.
func retryableMembershipErr(err error) bool {
	return errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrTimeout) ||
		errors.Is(err, raft.ErrConfigChangeInProgress)
}

// addLearnerWithRetry waits out a leadership change and treats an already
// applied learner change as success (a timeout can hide a committed entry).
func (c *testCluster) addLearnerWithRetry(learnerID string) {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		leader, _ := c.waitLeader(c.ids, testWait)
		ctx, cancel := context.WithTimeout(context.Background(), testWait)
		err := leader.AddLearner(ctx, learnerID)
		cancel()
		if err == nil || errors.Is(err, raft.ErrAlreadyLearner) {
			return
		}
		if !retryableMembershipErr(err) || time.Now().After(deadline) {
			c.t.Fatalf("AddLearner(%s): %v", learnerID, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// removeVoterWithRetry removes a voter from whichever node currently leads.
func (c *testCluster) removeVoterWithRetry(victim string) {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		leader, _ := c.waitLeader(c.ids, testWait)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := leader.RemoveVoter(ctx, victim)
		cancel()
		if err == nil {
			return
		}
		if !retryableMembershipErr(err) || time.Now().After(deadline) {
			c.t.Fatalf("RemoveVoter(%s): %v", victim, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// requireUnsafeSelfRemoval waits for a stable leader and asserts that removing
// it is rejected as unsafe.
func (c *testCluster) requireUnsafeSelfRemoval() {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		leader, _ := c.waitLeader(c.ids, testWait)
		id := leader.Status().ID
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := leader.RemoveVoter(ctx, id)
		cancel()
		if errors.Is(err, raft.ErrUnsafeConfigChange) {
			return
		}
		if !retryableMembershipErr(err) || time.Now().After(deadline) {
			c.t.Fatalf("RemoveVoter(self) = %v, want ErrUnsafeConfigChange", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestMembershipAddLearnerReplicatesAndDoesNotVote(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 200*time.Millisecond, 400*time.Millisecond, 25*time.Millisecond)
	leader, leaderID := c.waitLeader(c.ids, testWait)

	learnerID := "n4"
	learner := c.addNode(learnerID, c.ids, true)

	c.addLearnerWithRetry(learnerID)

	leader, _ = c.waitLeader(c.ids, testWait)
	voters, learners := leader.Membership()
	if len(voters) != 3 || contains(voters, learnerID) {
		t.Fatalf("leader voters after AddLearner = %v, want the original 3", voters)
	}
	if !contains(learners, learnerID) {
		t.Fatalf("leader learners = %v, want %s", learners, learnerID)
	}
	st := learner.Status()
	if !st.Learner || len(st.Voters) != 3 || !contains(st.Learners, learnerID) {
		t.Fatalf("learner status = %+v, want Learner with 3 voters", st)
	}

	// The learner replicates the leader's entries.
	c.proposeWithRetry(c.ids, model.Entry{Type: model.CommandPut, Key: "learner-key", Value: []byte("v1")}, testWait)
	c.requireValue(append(append([]string(nil), c.ids...), learnerID), "learner-key", "v1", testWait)

	// A learner never grants a vote.
	resp, err := learner.HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
		Term:         st.Term,
		CandidateID:  "n2",
		LastLogIndex: st.LastIndex,
		LastLogTerm:  st.Term,
	})
	if err != nil {
		t.Fatalf("learner HandleRequestVote: %v", err)
	}
	if resp.VoteGranted {
		t.Fatalf("learner granted a vote: %+v", resp)
	}

	// It never campaigns: with the old leader stopped, one of the three voters
	// must win and the learner must stay a follower.
	leader.Stop()
	remaining := peersExcept(c.ids, leaderID)
	newLeader, _ := c.waitLeader(remaining, testWait)
	if newLeader == learner || learner.IsLeader() {
		t.Fatal("learner became leader")
	}
	for i := 0; i < 20; i++ {
		if learner.IsLeader() {
			t.Fatal("learner campaigned and became leader")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Restart the stopped voter from its durable store. Membership must be
	// restored from the persisted record even though its static config still
	// lists only the original three voters.
	restarted, err := raft.New(
		c.configFor(leaderID, peersExcept(c.ids, leaderID), c.dirs[leaderID]),
		c.stores[leaderID],
		c.sms[leaderID],
		c.net.Transport(leaderID),
	)
	if err != nil {
		t.Fatalf("restart New: %v", err)
	}
	c.setNode(leaderID, restarted)
	c.net.Register(leaderID, restarted)
	if err := restarted.Start(c.ctx); err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	rv, rl := restarted.Membership()
	if len(rv) != 3 || contains(rv, learnerID) || !contains(rl, learnerID) {
		t.Fatalf("restarted membership = voters %v learners %v, want 3 voters + learner %s", rv, rl, learnerID)
	}
	c.requireValue(remaining, "learner-key", "v1", testWait)
}

func TestMembershipPromoteChangesQuorum(t *testing.T) {
	// Longer timeouts keep the leader stable while the other voter is
	// partitioned, so the proposal failure is a real quorum failure.
	c := newTestClusterWithTimeouts(t, 2, 300*time.Millisecond, 600*time.Millisecond, 30*time.Millisecond)
	leader, leaderID := c.waitLeader(c.ids, testWait)
	learnerID := "n3"
	learner := c.addNode(learnerID, c.ids, true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.addLearnerWithRetry(learnerID)
	leader, leaderID = c.waitLeader(c.ids, testWait)
	c.proposeWithRetry(c.ids, model.Entry{Type: model.CommandPut, Key: "before-promote", Value: []byte("x")}, testWait)
	c.requireValue(append(c.ids, learnerID), "before-promote", "x", testWait)

	// Isolate the only other voter. The learner still acknowledges the
	// leader, but a learner must not count towards the majority, so the write
	// cannot commit in the 2-voter configuration.
	other := otherID(c.ids, leaderID)
	c.net.Partition(leaderID, other)
	c.net.Partition(other, leaderID)
	if _, err := propose(t, leader, model.Entry{Type: model.CommandPut, Key: "learner-only", Value: []byte("no")}, 250*time.Millisecond); err == nil {
		t.Fatal("proposal committed with only the leader and a learner acknowledging")
	} else if !errors.Is(err, raft.ErrTimeout) && !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("proposal error = %v, want ErrTimeout or ErrNotLeader", err)
	}

	c.net.Heal(leaderID)
	c.net.Heal(other)
	leader, leaderID = c.waitLeader(c.ids, testWait)

	promoteDeadline := time.Now().Add(5 * time.Second)
	for {
		leader, leaderID = c.waitLeader(c.ids, testWait)
		err := leader.PromoteLearner(ctx, learnerID)
		if err == nil || errors.Is(err, raft.ErrAlreadyVoter) {
			break
		}
		if !errors.Is(err, raft.ErrNotLeader) && !errors.Is(err, raft.ErrTimeout) &&
			!errors.Is(err, raft.ErrConfigChangeInProgress) {
			t.Fatalf("PromoteLearner: %v", err)
		}
		if time.Now().After(promoteDeadline) {
			t.Fatalf("PromoteLearner did not succeed after retries: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	voters, learners := leader.Membership()
	if len(voters) != 3 || !contains(voters, learnerID) {
		t.Fatalf("voters after promotion = %v, want 3 including %s", voters, learnerID)
	}
	if contains(learners, learnerID) {
		t.Fatalf("learners after promotion = %v, still contains promoted voter", learners)
	}

	// The promoted learner now counts: leader + promoted voter is a majority
	// of three even with the other voter partitioned.
	c.net.Partition(leaderID, other)
	c.net.Partition(other, leaderID)
	if _, err := propose(t, leader, model.Entry{Type: model.CommandPut, Key: "after-promote", Value: []byte("yes")}, 2*time.Second); err != nil {
		t.Fatalf("proposal after promotion with leader+promoted voter: %v", err)
	}
	c.requireValue([]string{leaderID, learnerID}, "after-promote", "yes", testWait)
	if learner.Status().Role == raft.Leader {
		t.Fatal("promoted voter unexpectedly became leader while the leader was alive")
	}
}

func TestMembershipRemoveVoterAndSafety(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 200*time.Millisecond, 400*time.Millisecond, 25*time.Millisecond)
	leader, leaderID := c.waitLeader(c.ids, testWait)

	// Non-leaders reject every membership change.
	followerID := otherID(c.ids, leaderID)
	follower := c.nodes[followerID]
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := follower.AddLearner(ctx, "n9"); !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("follower AddLearner = %v, want ErrNotLeader", err)
	}
	if err := follower.PromoteLearner(ctx, "n9"); !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("follower PromoteLearner = %v, want ErrNotLeader", err)
	}
	if err := follower.RemoveVoter(ctx, followerID); !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("follower RemoveVoter = %v, want ErrNotLeader", err)
	}

	// Removing the leader is allowed under joint consensus: it keeps leading
	// through C_old,new and steps down only after C_new excludes it. That path
	// is covered by TestLeaderRemovalStepsDownAfterFinalConfig.

	victim := otherID(c.ids, leaderID)
	c.removeVoterWithRetry(victim)
	leader, leaderID = c.waitLeader(c.ids, testWait)
	voters, learners := leader.Membership()
	if len(voters) != 2 || contains(voters, victim) || contains(learners, victim) {
		t.Fatalf("membership after removal = voters %v learners %v", voters, learners)
	}
	if err := leader.RemoveVoter(ctx, victim); !errors.Is(err, raft.ErrNotVoter) {
		t.Fatalf("second RemoveVoter(%s) = %v, want ErrNotVoter", victim, err)
	}

	// Removing the last voter is impossible because self-removal is rejected;
	// a one-voter group has no other voter to target.
	single := newTestClusterWithTimeouts(t, 1, 200*time.Millisecond, 400*time.Millisecond, 25*time.Millisecond)
	single.requireUnsafeSelfRemoval()
	singleLeader, singleID := single.waitLeader(single.ids, testWait)
	singleCtx, singleCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer singleCancel()
	if err := singleLeader.AddLearner(singleCtx, singleID); !errors.Is(err, raft.ErrAlreadyVoter) {
		t.Fatalf("AddLearner(self) = %v, want ErrAlreadyVoter", err)
	}
}

func TestMembershipLearnerStartsFromSnapshot(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 2, 500*time.Millisecond, time.Second, 50*time.Millisecond)
	leader, _ := c.waitLeader(c.ids, testWait)

	for i := 0; i < 3; i++ {
		c.proposeWithRetry(c.ids, model.Entry{
			Type:  model.CommandPut,
			Key:   fmt.Sprintf("snap-%d", i),
			Value: []byte("value"),
		}, testWait)
	}
	snap, err := leader.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Index == 0 {
		t.Fatal("leader snapshot index is zero")
	}

	learnerID := "n3"
	learner := c.addNode(learnerID, c.ids, true)
	leader, _ = c.waitLeader(c.ids, testWait)
	c.addLearnerWithRetry(learnerID)
	c.requireValue([]string{learnerID}, "snap-0", "value", testWait)
	eventually(t, testWait, "learner applies the snapshot index", func() bool {
		return learner.Status().LastApplied >= snap.Index
	})
}

func TestFileStoreMembershipPersistence(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	// A legacy store with no membership record reports "not persisted".
	if voters, learners, err := store.LoadMembership(); err != nil || voters != nil || learners != nil {
		t.Fatalf("LoadMembership on fresh store = %v %v %v, want nil nil nil", voters, learners, err)
	}
	if err := store.SaveMembership([]string{"n1", "n2"}, []string{"n3"}); err != nil {
		t.Fatalf("SaveMembership: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	voters, learners, err := reopened.LoadMembership()
	if err != nil {
		t.Fatalf("LoadMembership: %v", err)
	}
	if len(voters) != 2 || voters[0] != "n1" || voters[1] != "n2" {
		t.Fatalf("voters = %v", voters)
	}
	if len(learners) != 1 || learners[0] != "n3" {
		t.Fatalf("learners = %v", learners)
	}
}

func TestMembershipForceLearnerOverridesStaleDiskState(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	// A previous incarnation left this node as a voter of {n1,n2}.
	if err := store.SaveMembership([]string{"n1", "n2"}, nil); err != nil {
		t.Fatalf("SaveMembership: %v", err)
	}

	// Re-adding the range must make the node a learner of {n2} even though the
	// persisted state still lists it as a voter, so it cannot campaign before
	// the leader re-adds it.
	cfg := raft.Config{
		ID:                 "n1",
		Peers:              []string{"n2"},
		DataDir:            dir,
		ElectionTimeoutMin: 100 * time.Millisecond,
		ElectionTimeoutMax: 200 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		Learner:            true,
		ForceLearner:       true,
		Logger:             discardLogger(),
	}
	node, err := raft.New(cfg, store, newTestSM(), &testTransferTransport{cluster: &testCluster{nodes: map[string]*raft.Node{}}, id: "n1"})
	if err != nil {
		t.Fatalf("raft.New with ForceLearner: %v", err)
	}
	voters, learners := node.Membership()
	if len(voters) != 1 || voters[0] != "n2" {
		t.Fatalf("voters = %v, want [n2]", voters)
	}
	if len(learners) != 1 || learners[0] != "n1" {
		t.Fatalf("learners = %v, want [n1]", learners)
	}

	node.Stop()
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// The forced state is durable: reopening without ForceLearner restores it.
	reopened, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	persistedVoters, persistedLearners, err := reopened.LoadMembership()
	if err != nil {
		t.Fatalf("LoadMembership: %v", err)
	}
	if len(persistedVoters) != 1 || persistedVoters[0] != "n2" ||
		len(persistedLearners) != 1 || persistedLearners[0] != "n1" {
		t.Fatalf("persisted membership = %v %v, want [n2] [n1]", persistedVoters, persistedLearners)
	}
}
