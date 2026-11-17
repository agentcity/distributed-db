package raft_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

// setupJointCluster starts a three-voter cluster, adds n4 and n5 as learners,
// then enters the joint configuration {leader,n4,n5} against {n1,n2,n3}. The
// manual EnterJoint call sets AutoFinalize=false, so the cluster stays joint
// until the test calls LeaveJoint.
func setupJointCluster(t *testing.T) (c *testCluster, leader *raft.Node, leaderID string, target []string) {
	t.Helper()
	c = newTestClusterWithTimeouts(t, 3, 150*time.Millisecond, 300*time.Millisecond, 20*time.Millisecond)
	c.waitLeader(c.ids, testWait)
	c.addNode("n4", c.ids, true)
	c.addNode("n5", c.ids, true)
	c.addLearnerWithRetry("n4")
	c.addLearnerWithRetry("n5")

	deadline := time.Now().Add(5 * time.Second)
	for {
		leader, leaderID = c.waitLeader(c.ids, testWait)
		target = []string{leaderID, "n4", "n5"}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := leader.EnterJoint(ctx, target, nil)
		cancel()
		if err == nil {
			break
		}
		if !retryableMembershipErr(err) || time.Now().After(deadline) {
			t.Fatalf("EnterJoint: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitJoint(t, c, leader.Status().ID)
	return c, leader, leaderID, target
}

func waitJoint(t *testing.T, c *testCluster, id string) {
	t.Helper()
	eventually(t, testWait, "node "+id+" is joint", func() bool {
		return c.nodes[id].Status().Joint
	})
}

// partitionGroups blocks every link between every pair of distinct groups in
// both directions.
func partitionGroups(c *testCluster, groups ...[]string) {
	for i := 0; i < len(groups); i++ {
		for j := i + 1; j < len(groups); j++ {
			// Partition replaces id's whole outgoing block set, so pass the
			// complete opposite group at once.
			for _, a := range groups[i] {
				c.net.Partition(a, groups[j]...)
			}
			for _, b := range groups[j] {
				c.net.Partition(b, groups[i]...)
			}
		}
	}
}

func healAll(c *testCluster, ids ...string) {
	for _, id := range ids {
		c.net.Heal(id)
	}
}

func requireJointStatus(t *testing.T, st raft.Status, votersOld, votersNew []string) {
	t.Helper()
	if !st.Joint {
		t.Fatal("status is not joint")
	}
	if !equalSet(st.VotersOld, votersOld) {
		t.Fatalf("VotersOld = %v, want %v", st.VotersOld, votersOld)
	}
	if !equalSet(st.VotersNew, votersNew) {
		t.Fatalf("VotersNew = %v, want %v", st.VotersNew, votersNew)
	}
}

// TestJointQuorumRequiresBothMajorities partitions the group so that one side
// holds a majority of C_old but not of C_new and the other holds a majority of
// C_new but not of C_old. Neither side may commit; after healing, leaving the
// joint configuration must restore a single working majority.
func TestJointQuorumRequiresBothMajorities(t *testing.T) {
	t.Run("old-only majority", func(t *testing.T) {
		c, leader, leaderID, target := setupJointCluster(t)
		st := leader.Status()
		requireJointStatus(t, st, c.ids, target)

		// Group 1 has the leader plus one old voter: an old majority (2/3)
		// but only the leader from C_new. Group 2 has the remaining old voter
		// and both new voters: a new majority but no old majority.
		var sideA, sideB []string
		otherOld := ""
		for _, id := range c.ids {
			if id != leaderID {
				otherOld = id
				break
			}
		}
		sideA = []string{leaderID, otherOld}
		for _, id := range c.ids {
			if id != leaderID && id != otherOld {
				sideB = append(sideB, id)
			}
		}
		sideB = append(sideB, "n4", "n5")
		partitionGroups(c, sideA, sideB)

		ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
		_, err := leader.Propose(ctx, model.Entry{Type: model.CommandPut, Key: "old-majority", Value: []byte("no")})
		cancel()
		if err == nil {
			t.Fatal("proposal committed with only an old-voter majority")
		}
		if !errors.Is(err, raft.ErrTimeout) && !errors.Is(err, raft.ErrNotLeader) {
			t.Fatalf("proposal error = %v, want ErrTimeout or ErrNotLeader", err)
		}

		// Give any accidental commit time to become visible before asserting
		// the absence of the value on every replica.
		time.Sleep(2 * c.electionMax)
		for _, id := range append(append([]string(nil), c.ids...), "n4", "n5") {
			if _, ok := c.sms[id].get("old-majority"); ok {
				t.Fatalf("node %s applied an entry committed without both majorities", id)
			}
		}

		// Heal, leave the joint configuration and verify a normal entry
		// commits under C_new.
		all := append(append([]string(nil), c.ids...), "n4", "n5")
		healAll(c, all...)
		// A node that C_old still contains (but C_new excludes) may lead the
		// joint configuration; use the full joint electorate to find it.
		leader, _ = c.waitLeader(all, 5*time.Second)
		leaveJointWithRetry(t, c, all)
		waitFinal(t, c, target)
		leader, _ = c.waitLeader(target, 5*time.Second)

		if _, err := propose(t, leader, model.Entry{Type: model.CommandPut, Key: "after-joint", Value: []byte("yes")}, 2*time.Second); err != nil {
			t.Fatalf("proposal after leaving joint: %v", err)
		}
		c.requireValue(target, "after-joint", "yes", testWait)
	})

	t.Run("new-only majority", func(t *testing.T) {
		c, leader, leaderID, target := setupJointCluster(t)
		st := leader.Status()
		requireJointStatus(t, st, c.ids, target)

		// The two new voters alone hold a majority of C_new, while the leader
		// and the other two old voters hold a majority of C_old. Neither side
		// has both.
		sideA := []string{"n4", "n5"}
		sideB := append([]string(nil), c.ids...)
		_ = leaderID
		partitionGroups(c, sideA, sideB)

		ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
		_, err := leader.Propose(ctx, model.Entry{Type: model.CommandPut, Key: "new-majority", Value: []byte("no")})
		cancel()
		if err == nil {
			t.Fatal("proposal committed while only a C_new majority was reachable")
		}
		if !errors.Is(err, raft.ErrTimeout) && !errors.Is(err, raft.ErrNotLeader) {
			t.Fatalf("proposal error = %v, want ErrTimeout or ErrNotLeader", err)
		}

		time.Sleep(2 * c.electionMax)
		for _, id := range append(append([]string(nil), c.ids...), "n4", "n5") {
			if _, ok := c.sms[id].get("new-majority"); ok {
				t.Fatalf("node %s applied an entry committed without both majorities", id)
			}
		}

		all := append(append([]string(nil), c.ids...), "n4", "n5")
		healAll(c, all...)
		leaveJointWithRetry(t, c, all)
		waitFinal(t, c, target)
		leader, _ = c.waitLeader(target, 5*time.Second)
		if _, err := propose(t, leader, model.Entry{Type: model.CommandPut, Key: "after-joint-2", Value: []byte("yes")}, 2*time.Second); err != nil {
			t.Fatalf("proposal after leaving joint: %v", err)
		}
		c.requireValue(target, "after-joint-2", "yes", testWait)
	})
}

func leaveJointWithRetry(t *testing.T, c *testCluster, ids []string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		leader, _ := c.waitLeader(ids, 5*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := leader.LeaveJoint(ctx)
		cancel()
		if err == nil || errors.Is(err, raft.ErrNoJointConfig) {
			return
		}
		if !retryableMembershipErr(err) || time.Now().After(deadline) {
			t.Fatalf("LeaveJoint: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitFinal(t *testing.T, c *testCluster, ids []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok := true
		for _, id := range ids {
			node := c.nodes[id]
			if node == nil {
				continue
			}
			st := node.Status()
			if st.Joint || !equalSet(st.Voters, ids) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, id := range ids {
		if node := c.nodes[id]; node != nil {
			st := node.Status()
			t.Logf("waitFinal status %s: role=%s term=%d joint=%v voters=%v old=%v epoch=%d commit=%d applied=%d",
				id, st.Role, st.Term, st.Joint, st.Voters, st.VotersOld, st.ConfigEpoch, st.CommitIndex, st.LastApplied)
		}
	}
	t.Fatalf("cluster did not leave the joint configuration with voters %v", ids)
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if !seen[id] {
			return false
		}
	}
	return true
}

func TestJointRejectsConcurrentConfigChange(t *testing.T) {
	c, leader, _, target := setupJointCluster(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := leader.AddLearner(ctx, "n9"); !errors.Is(err, raft.ErrConfigChangeInProgress) {
		t.Fatalf("AddLearner while joint = %v, want ErrConfigChangeInProgress", err)
	}
	if err := leader.PromoteLearner(ctx, "n4"); !errors.Is(err, raft.ErrConfigChangeInProgress) {
		t.Fatalf("PromoteLearner while joint = %v, want ErrConfigChangeInProgress", err)
	}
	if err := leader.RemoveVoter(ctx, "n1"); !errors.Is(err, raft.ErrConfigChangeInProgress) {
		t.Fatalf("RemoveVoter while joint = %v, want ErrConfigChangeInProgress", err)
	}
	if err := leader.EnterJoint(ctx, target, nil); !errors.Is(err, raft.ErrConfigChangeInProgress) {
		t.Fatalf("second EnterJoint = %v, want ErrConfigChangeInProgress", err)
	}

	// The joint configuration is still intact and unchanged.
	requireJointStatus(t, leader.Status(), c.ids, target)

	if err := leader.LeaveJoint(ctx); err != nil {
		t.Fatalf("LeaveJoint: %v", err)
	}
	waitFinal(t, c, target)

	// A duplicate LeaveJoint no longer has a joint configuration to leave.
	if err := leader.LeaveJoint(ctx); !errors.Is(err, raft.ErrNoJointConfig) {
		t.Fatalf("second LeaveJoint = %v, want ErrNoJointConfig", err)
	}
}

func TestLeaderRemovalStepsDownAfterFinalConfig(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 200*time.Millisecond, 400*time.Millisecond, 25*time.Millisecond)
	leader, leaderID := c.waitLeader(c.ids, testWait)

	// The leader removes itself. It retains leadership through C_old,new (it
	// is still a C_old voter) and must step down only after C_new, which
	// excludes it, is applied.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := leader.RemoveVoter(ctx, leaderID)
	cancel()
	if err != nil {
		t.Fatalf("RemoveVoter(leader %s): %v", leaderID, err)
	}

	st := leader.Status()
	if st.Joint {
		t.Fatal("leader is still joint after RemoveVoter returned")
	}
	if contains(st.Voters, leaderID) {
		t.Fatalf("removed leader still in voters: %v", st.Voters)
	}
	if st.Role == raft.Leader {
		t.Fatal("removed leader did not step down after C_new")
	}

	remaining := peersExcept(c.ids, leaderID)
	newLeader, newLeaderID := c.waitLeader(remaining, testWait)
	if newLeaderID == leaderID {
		t.Fatal("removed node was elected again")
	}
	eventually(t, testWait, "new leader applies the final configuration", func() bool {
		st := newLeader.Status()
		return !st.Joint && equalSet(st.Voters, remaining)
	})

	// The removed node stays a follower and never wins another election.
	for i := 0; i < 20; i++ {
		if leader.IsLeader() {
			t.Fatal("removed leader regained leadership")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := propose(t, newLeader, model.Entry{Type: model.CommandPut, Key: "after-leader-removal", Value: []byte("v")}, 2*time.Second); err != nil {
		t.Fatalf("proposal after leader removal: %v", err)
	}
	c.requireValue(remaining, "after-leader-removal", "v", testWait)
}

// TestJointConfigSurvivesSnapshotAndRestart verifies that a snapshot taken in
// the joint state carries C_old, C_new and the auto-finalize bit, that a
// restart restores the exact joint configuration, and that the same holds for
// the final configuration.
func TestJointConfigSurvivesSnapshotAndRestart(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 200*time.Millisecond, 400*time.Millisecond, 25*time.Millisecond)

	leader, leaderID := c.waitLeader(c.ids, testWait)
	if _, err := propose(t, leader, model.Entry{Type: model.CommandPut, Key: "before-joint", Value: []byte("v")}, testWait); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}

	// Remove one voter that is neither the leader nor our restart target.
	var victim, follower string
	for _, id := range c.ids {
		if id == leaderID {
			continue
		}
		if follower == "" {
			follower = id
			continue
		}
		victim = id
	}
	if victim == "" {
		t.Fatal("test needs two non-leader voters")
	}
	target := []string{leaderID, follower}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := leader.EnterJoint(ctx, target, nil); err != nil {
		cancel()
		t.Fatalf("EnterJoint: %v", err)
	}
	cancel()
	st := leader.Status()
	requireJointStatus(t, st, c.ids, target)
	jointEpoch := st.ConfigEpoch

	// Snapshot a follower that stays in C_new, so it receives the final entry
	// later even after the removed voter stops being a replication target.
	fNode := c.nodes[follower]
	eventually(t, testWait, "follower applies the joint configuration", func() bool {
		return fNode.Status().Joint
	})
	snap, err := fNode.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot while joint: %v", err)
	}
	if snap.Membership == nil {
		t.Fatal("joint snapshot has no membership")
	}
	if !equalSet(snap.Membership.VotersOld, c.ids) ||
		!equalSet(snap.Membership.Voters, target) ||
		snap.Membership.AutoFinalize ||
		snap.Membership.ConfigEpoch != jointEpoch {
		t.Fatalf("joint snapshot membership = %+v, want old %v new %v epoch %d auto=false",
			snap.Membership, c.ids, target, jointEpoch)
	}

	// Restart the follower from its snapshot and verify the joint state.
	fNode.Stop()
	restarted, err := raft.New(
		c.configFor(follower, peersExcept(c.ids, follower), c.dirs[follower]),
		c.stores[follower], c.sms[follower], c.net.Transport(follower),
	)
	if err != nil {
		t.Fatalf("restart New: %v", err)
	}
	c.nodes[follower] = restarted
	c.net.Register(follower, restarted)
	if err := restarted.Start(c.ctx); err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	st = restarted.Status()
	requireJointStatus(t, st, c.ids, target)
	if st.ConfigEpoch != jointEpoch {
		t.Fatalf("restarted joint epoch = %d, want %d", st.ConfigEpoch, jointEpoch)
	}

	// Leave the joint configuration, then verify the final state survives the
	// same snapshot/restart path.
	leaveJointWithRetry(t, c, c.ids)
	waitFinal(t, c, target)

	fNode = c.nodes[follower]
	snap, err = fNode.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot after final: %v", err)
	}
	if snap.Membership == nil || snap.Membership.Joint() {
		t.Fatalf("final snapshot membership = %+v, want non-joint", snap.Membership)
	}
	if !equalSet(snap.Membership.Voters, target) || len(snap.Membership.VotersOld) != 0 {
		t.Fatalf("final snapshot voters = %v old = %v, want %v and no old set",
			snap.Membership.Voters, snap.Membership.VotersOld, target)
	}

	finalEpoch := fNode.Status().ConfigEpoch
	fNode.Stop()
	restarted, err = raft.New(
		c.configFor(follower, peersExcept(c.ids, follower), c.dirs[follower]),
		c.stores[follower], c.sms[follower], c.net.Transport(follower),
	)
	if err != nil {
		t.Fatalf("restart after final New: %v", err)
	}
	c.nodes[follower] = restarted
	c.net.Register(follower, restarted)
	if err := restarted.Start(c.ctx); err != nil {
		t.Fatalf("restart after final Start: %v", err)
	}
	st = restarted.Status()
	if st.Joint {
		t.Fatal("final configuration restarted joint")
	}
	if !equalSet(st.Voters, target) {
		t.Fatalf("restarted final voters = %v, want %v", st.Voters, target)
	}
	if st.ConfigEpoch != finalEpoch {
		t.Fatalf("restarted final epoch = %d, want %d", st.ConfigEpoch, finalEpoch)
	}
}

// TestConfigEntriesIdempotentAndStaleRejected exercises the entry-level
// guarantees: an already-applied identical entry is a no-op, while a stale or
// malformed entry is rejected without changing the configuration.
func TestConfigEntriesIdempotentAndStaleRejected(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 200*time.Millisecond, 400*time.Millisecond, 25*time.Millisecond)
	leader, _ := c.waitLeader(c.ids, testWait)

	// A normal single change leaves the cluster in a final configuration.
	victim := otherID(c.ids, leader.Status().ID)
	c.removeVoterWithRetry(victim)
	leader, _ = c.waitLeader(peersExcept(c.ids, victim), testWait)

	st := leader.Status()
	epoch := st.ConfigEpoch
	finalData := raft.ConfigEntryData{
		Voters:   append([]string(nil), st.Voters...),
		Learners: append([]string(nil), st.Learners...),
		Epoch:    epoch,
	}
	value, err := raft.EncodeConfigEntry(finalData)
	if err != nil {
		t.Fatalf("EncodeConfigEntry: %v", err)
	}
	if _, err := propose(t, leader, model.Entry{Type: raft.CommandFinalConfig, Value: value}, testWait); err != nil {
		t.Fatalf("duplicate final entry: %v", err)
	}
	st = leader.Status()
	if st.ConfigEpoch != epoch {
		t.Fatalf("duplicate final entry advanced epoch to %d, want %d", st.ConfigEpoch, epoch)
	}

	// A joint entry at the already-applied epoch is stale and must not apply.
	staleData := raft.ConfigEntryData{
		VotersOld: append([]string(nil), st.Voters...),
		Voters:    append([]string(nil), st.Voters...),
		Epoch:     st.ConfigEpoch,
	}
	value, err = raft.EncodeConfigEntry(staleData)
	if err != nil {
		t.Fatalf("Encode stale entry: %v", err)
	}
	if _, err := propose(t, leader, model.Entry{Type: raft.CommandJointConfig, Value: value}, testWait); !errors.Is(err, raft.ErrStaleConfigChange) {
		t.Fatalf("stale joint entry = %v, want ErrStaleConfigChange", err)
	}
	if got := leader.Status(); got.Joint || got.ConfigEpoch != epoch || !reflect.DeepEqual(got.Voters, st.Voters) {
		t.Fatalf("stale joint entry changed config: %+v", got)
	}

	// Structural validation rejects malformed entries before they are
	// proposed.
	malformed := []raft.ConfigEntryData{
		{Epoch: 0, Voters: []string{"n1"}},
		{Epoch: 1, Voters: nil},
		{Epoch: 1, Voters: []string{"n1", "n1"}},
		{Epoch: 1, Voters: []string{"n1"}, Learners: []string{"n1"}},
		{Epoch: 1, Voters: []string{"n1"}, VotersOld: []string{"n1", ""}},
		{Epoch: 1, Voters: []string{"n1"}, AutoFinalize: true},
	}
	for i, data := range malformed {
		if _, err := raft.EncodeConfigEntry(data); !errors.Is(err, raft.ErrInvalidConfigEntry) {
			t.Fatalf("malformed entry %d: err = %v, want ErrInvalidConfigEntry", i, err)
		}
	}
}

// TestFileStorePersistsJointMembership covers the durable membership record for
// a joint configuration, including the auto-finalize intent.
func TestFileStorePersistsJointMembership(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.SaveJointMembership([]string{"n1", "n2"}, []string{"n2", "n3"}, []string{"n4"}, true, 7); err != nil {
		t.Fatalf("SaveJointMembership: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	votersOld, voters, learners, auto, epoch, err := reopened.LoadJointMembership()
	if err != nil {
		t.Fatalf("LoadJointMembership: %v", err)
	}
	if !reflect.DeepEqual(votersOld, []string{"n1", "n2"}) ||
		!reflect.DeepEqual(voters, []string{"n2", "n3"}) ||
		!reflect.DeepEqual(learners, []string{"n4"}) ||
		!auto || epoch != 7 {
		t.Fatalf("joint record = old %v voters %v learners %v auto %v epoch %d",
			votersOld, voters, learners, auto, epoch)
	}
}

// TestAutoFinalizeJointEntryCompletes verifies the recovery path used by the
// single-change APIs: a joint entry with AutoFinalize set is completed to C_new
// by the leader without any client call.
func TestAutoFinalizeJointEntryCompletes(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 200*time.Millisecond, 400*time.Millisecond, 25*time.Millisecond)
	leader, leaderID := c.waitLeader(c.ids, testWait)

	target := peersExcept(c.ids, leaderID)
	st := leader.Status()
	data := raft.ConfigEntryData{
		VotersOld:    append([]string(nil), st.Voters...),
		Voters:       target,
		AutoFinalize: true,
		Epoch:        st.ConfigEpoch + 1,
	}
	value, err := raft.EncodeConfigEntry(data)
	if err != nil {
		t.Fatalf("EncodeConfigEntry: %v", err)
	}
	if _, err := propose(t, leader, model.Entry{Type: raft.CommandJointConfig, Value: value}, testWait); err != nil {
		t.Fatalf("propose auto-finalize joint entry: %v", err)
	}

	eventually(t, 5*time.Second, "auto-finalize completes the joint configuration", func() bool {
		st := leader.Status()
		return !st.Joint && equalSet(st.Voters, target)
	})
}

// TestDuplicateJointEntryIsIdempotent verifies that replaying an already
// applied joint entry does not advance the configuration epoch.
func TestDuplicateJointEntryIsIdempotent(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 200*time.Millisecond, 400*time.Millisecond, 25*time.Millisecond)
	leader, _ := c.waitLeader(c.ids, testWait)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st := leader.Status()
	if err := leader.EnterJoint(ctx, st.Voters, st.Learners); err != nil {
		t.Fatalf("EnterJoint: %v", err)
	}
	st = leader.Status()
	requireJointStatus(t, st, st.VotersOld, st.VotersNew)
	epoch := st.ConfigEpoch

	duplicate := raft.ConfigEntryData{
		VotersOld: append([]string(nil), st.VotersOld...),
		Voters:    append([]string(nil), st.VotersNew...),
		Learners:  append([]string(nil), st.Learners...),
		Epoch:     epoch,
	}
	value, err := raft.EncodeConfigEntry(duplicate)
	if err != nil {
		t.Fatalf("EncodeConfigEntry: %v", err)
	}
	if _, err := propose(t, leader, model.Entry{Type: raft.CommandJointConfig, Value: value}, testWait); err != nil {
		t.Fatalf("duplicate joint entry: %v", err)
	}
	if got := leader.Status().ConfigEpoch; got != epoch {
		t.Fatalf("duplicate joint entry advanced epoch to %d, want %d", got, epoch)
	}
	if err := leader.LeaveJoint(ctx); err != nil {
		t.Fatalf("LeaveJoint: %v", err)
	}
	waitFinal(t, c, c.ids)
}

// TestJointEntryRequiresBothMajorities covers the transition itself: while the
// new voters are unreachable but the old configuration still has a majority,
// C_old,new must not commit. Entering a joint state the cluster cannot leave
// would otherwise be possible with an old-only majority.
func TestJointEntryRequiresBothMajorities(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 200*time.Millisecond, 400*time.Millisecond, 25*time.Millisecond)
	c.waitLeader(c.ids, testWait)
	c.addNode("n4", c.ids, true)
	c.addNode("n5", c.ids, true)
	c.addLearnerWithRetry("n4")
	c.addLearnerWithRetry("n5")

	leader, leaderID := c.waitLeader(c.ids, testWait)
	epochBefore := leader.Status().ConfigEpoch

	// The leader can still reach the old majority (n1,n2,n3) but not either
	// new voter, so a joint entry cannot get a C_new majority.
	c.net.Partition(leaderID, "n4", "n5")
	c.net.Partition("n4", leaderID)
	c.net.Partition("n5", leaderID)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	err := leader.EnterJoint(ctx, []string{leaderID, "n4", "n5"}, nil)
	cancel()
	if err == nil {
		t.Fatal("joint entry committed with only an old-voter majority")
	}
	if !errors.Is(err, raft.ErrTimeout) && !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("EnterJoint error = %v, want ErrTimeout or ErrNotLeader", err)
	}
	st := leader.Status()
	if st.Joint {
		t.Fatal("node entered the joint configuration without a C_new majority")
	}
	if st.ConfigEpoch != epochBefore {
		t.Fatalf("config epoch advanced to %d, want %d", st.ConfigEpoch, epochBefore)
	}

	healAll(c, append(append([]string(nil), c.ids...), "n4", "n5")...)
}
