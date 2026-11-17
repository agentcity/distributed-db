package raft_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"hash/crc32"
	"reflect"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

// snapshotWithMembership builds a snapshot payload for the test state machine.
// It is used where only the envelope matters; the payload itself is a valid
// snapshot image at index.
func snapshotWithMembership(t *testing.T, index, term uint64, membership *raft.SnapshotMembership) raft.Snapshot {
	t.Helper()
	sm := newTestSM()
	data, _, err := sm.Snapshot()
	if err != nil {
		t.Fatalf("build snapshot payload: %v", err)
	}
	return raft.Snapshot{Index: index, Term: term, Data: data, Membership: membership}
}

func sameMembership(t *testing.T, got *raft.SnapshotMembership, voters, learners []string, epoch uint64) {
	t.Helper()
	if got == nil {
		t.Fatal("snapshot has no membership")
	}
	if !reflect.DeepEqual(got.Voters, voters) {
		t.Fatalf("snapshot voters = %v, want %v", got.Voters, voters)
	}
	if !reflect.DeepEqual(got.Learners, learners) {
		t.Fatalf("snapshot learners = %v, want %v", got.Learners, learners)
	}
	if got.ConfigEpoch != epoch {
		t.Fatalf("snapshot config epoch = %d, want %d", got.ConfigEpoch, epoch)
	}
}

// TestSnapshotMembershipConfigChangesRoundTrip exercises the full lifecycle:
// add a learner, promote it, remove a voter, take a snapshot, install that
// snapshot into a node with a stale static configuration, and restart it from
// disk. The installed membership must win everywhere.
func TestSnapshotMembershipConfigChangesRoundTrip(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 300*time.Millisecond, 600*time.Millisecond, 50*time.Millisecond)

	leader, leaderID := c.waitLeader(c.ids, testWait)
	initialEpoch := leader.Status().ConfigEpoch
	if initialEpoch == 0 {
		t.Fatal("initial config epoch is 0")
	}

	learnerID := "n4"
	c.addNode(learnerID, c.ids, true)
	c.addLearnerWithRetry(learnerID)
	leader, _ = c.waitLeader(c.ids, testWait)
	if epoch := leader.Status().ConfigEpoch; epoch <= initialEpoch {
		t.Fatalf("config epoch after AddLearner = %d, want > %d", epoch, initialEpoch)
	}

	// Promote the learner. The cluster has enough voters that the promotion
	// commits even if this node loses leadership; retry through an election.
	deadline := time.Now().Add(5 * time.Second)
	for {
		leader, _ = c.waitLeader(c.ids, testWait)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := leader.PromoteLearner(ctx, learnerID)
		cancel()
		if err == nil || errors.Is(err, raft.ErrAlreadyVoter) {
			break
		}
		if !errors.Is(err, raft.ErrNotLeader) && !errors.Is(err, raft.ErrTimeout) &&
			!errors.Is(err, raft.ErrConfigChangeInProgress) {
			t.Fatalf("PromoteLearner(%s): %v", learnerID, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("PromoteLearner(%s) did not succeed: %v", learnerID, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	leader, leaderID = c.waitLeader(append(append([]string(nil), c.ids...), learnerID), testWait)
	victim := ""
	for _, id := range c.ids {
		if id != leaderID {
			victim = id
			break
		}
	}
	if victim == "" {
		t.Fatal("no non-leader voter to remove")
	}
	c.removeVoterWithRetry(victim)

	all := append(append([]string(nil), c.ids...), learnerID)
	remaining := make([]string, 0, len(all))
	for _, id := range all {
		if id != victim {
			remaining = append(remaining, id)
		}
	}
	leader, leaderID = c.waitLeader(remaining, testWait)

	// An application entry after the config entries guarantees the state
	// machine's snapshot index covers them.
	c.proposeWithRetry(remaining, model.Entry{Type: model.CommandPut, Key: "after-config", Value: []byte("v")}, testWait)

	wantVoters, wantLearners := leader.Membership()
	wantEpoch := leader.Status().ConfigEpoch
	snap, err := leader.Snapshot()
	if err != nil {
		t.Fatalf("leader Snapshot: %v", err)
	}
	if snap.Index == 0 || snap.Membership == nil {
		t.Fatalf("snapshot = {index %d, membership %+v}, want positive index and membership", snap.Index, snap.Membership)
	}
	sameMembership(t, snap.Membership, wantVoters, wantLearners, wantEpoch)
	if epoch := leader.Status().ConfigEpoch; epoch != wantEpoch {
		t.Fatalf("config epoch changed during Snapshot: %d, want %d", epoch, wantEpoch)
	}

	// Install the snapshot into a fresh node whose static configuration is
	// deliberately stale (the original voter set, including the removed node).
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	stalePeers := append(append([]string(nil), c.ids...), learnerID)
	cfg := snapshotTestConfig("n9", stalePeers, dir, 300*time.Millisecond, 600*time.Millisecond, 50*time.Millisecond)
	node, err := raft.New(cfg, store, newTestSM(), &testTransferTransport{cluster: &testCluster{nodes: map[string]*raft.Node{}}, id: "n9"})
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}

	if err := node.RestoreSnapshot(snap); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	gotVoters, gotLearners := node.Membership()
	if !reflect.DeepEqual(gotVoters, wantVoters) || !reflect.DeepEqual(gotLearners, wantLearners) {
		t.Fatalf("membership after install = %v %v, want %v %v", gotVoters, gotLearners, wantVoters, wantLearners)
	}
	if contains(gotVoters, victim) {
		t.Fatalf("removed voter %s survived snapshot install: %v", victim, gotVoters)
	}
	if st := node.Status(); st.ConfigEpoch != wantEpoch {
		t.Fatalf("config epoch after install = %d, want %d", st.ConfigEpoch, wantEpoch)
	}
	persistedVoters, persistedLearners, persistedEpoch, err := store.LoadMembershipEpoch()
	if err != nil {
		t.Fatalf("LoadMembershipEpoch: %v", err)
	}
	if !reflect.DeepEqual(persistedVoters, wantVoters) || !reflect.DeepEqual(persistedLearners, wantLearners) || persistedEpoch != wantEpoch {
		t.Fatalf("persisted membership = %v %v epoch %d, want %v %v epoch %d",
			persistedVoters, persistedLearners, persistedEpoch, wantVoters, wantLearners, wantEpoch)
	}
	persistedSnap, err := store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	sameMembership(t, persistedSnap.Membership, wantVoters, wantLearners, wantEpoch)

	// Restart from disk with the same stale static configuration: the durable
	// snapshot and membership record must win again.
	node.Stop()
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	restarted, err := raft.New(cfg, reopened, newTestSM(), &testTransferTransport{cluster: &testCluster{nodes: map[string]*raft.Node{}}, id: "n9"})
	if err != nil {
		t.Fatalf("restart raft.New: %v", err)
	}
	defer restarted.Stop()
	restartedVoters, restartedLearners := restarted.Membership()
	if !reflect.DeepEqual(restartedVoters, wantVoters) || !reflect.DeepEqual(restartedLearners, wantLearners) {
		t.Fatalf("membership after restart = %v %v, want %v %v", restartedVoters, restartedLearners, wantVoters, wantLearners)
	}
	if contains(restartedVoters, victim) {
		t.Fatalf("removed voter %s returned after restart: %v", victim, restartedVoters)
	}
	if st := restarted.Status(); st.ConfigEpoch != wantEpoch {
		t.Fatalf("config epoch after restart = %d, want %d", st.ConfigEpoch, wantEpoch)
	}
}

// TestSnapshotCoversConfigEntriesWhenStateMachineLags verifies a snapshot taken
// immediately after a membership change still records the new configuration:
// config entries never reach the state machine, so its index lags lastApplied.
func TestSnapshotCoversConfigEntriesWhenStateMachineLags(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	net := newSnapshotNetwork()
	sm := newSnapshotSM()
	cfg := snapshotTestConfig("n1", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond)
	node, err := raft.New(cfg, store, sm, net.transport("n1"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.register("n1", node)
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer node.Stop()
	eventually(t, testWait, "single node becomes leader", node.IsLeader)

	if _, err := propose(t, node, model.Entry{Type: model.CommandPut, Key: "k", Value: []byte("v")}, testWait); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := node.AddLearner(ctx, "n2"); err != nil {
		t.Fatalf("AddLearner: %v", err)
	}
	// The state machine applied only the Put; the AddLearner config entry is
	// Raft state and does not advance the state machine's own index.
	before := node.Status()
	if sm.appliedIndex() >= before.LastApplied {
		t.Fatalf("state machine applied index = %d, want < last applied %d", sm.appliedIndex(), before.LastApplied)
	}

	wantVoters, wantLearners := node.Membership()
	snap, err := node.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Index != before.LastApplied {
		t.Fatalf("snapshot index = %d, want last applied %d so the config entry is covered", snap.Index, before.LastApplied)
	}
	sameMembership(t, snap.Membership, wantVoters, wantLearners, before.ConfigEpoch)
	if !contains(snap.Membership.Learners, "n2") {
		t.Fatalf("snapshot learners = %v, want n2", snap.Membership.Learners)
	}
}

// TestSnapshotToStorePersistsMembership verifies the streaming creation path
// records the same membership envelope as the []byte Snapshot path.
func TestSnapshotToStorePersistsMembership(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	net := newSnapshotNetwork()
	sm := newStreamSnapshotSM()
	cfg := snapshotTestConfig("n1", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond)
	node, err := raft.New(cfg, store, sm, net.transport("n1"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.register("n1", node)
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer node.Stop()
	eventually(t, testWait, "single node becomes leader", node.IsLeader)

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := node.AddLearner(ctx, "n2"); err != nil {
		t.Fatalf("AddLearner: %v", err)
	}
	if _, err := propose(t, node, model.Entry{Type: model.CommandPut, Key: "k", Value: []byte("v")}, testWait); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	wantVoters, wantLearners := node.Membership()
	wantEpoch := node.Status().ConfigEpoch
	meta, err := node.SnapshotToStore()
	if err != nil {
		t.Fatalf("SnapshotToStore: %v", err)
	}
	sameMembership(t, meta.Membership, wantVoters, wantLearners, wantEpoch)
	opened, rc, err := store.OpenSnapshot()
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	if rc == nil {
		t.Fatal("OpenSnapshot returned no reader")
	}
	_ = rc.Close()
	sameMembership(t, opened.Membership, wantVoters, wantLearners, wantEpoch)
	stored, err := store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	sameMembership(t, stored.Membership, wantVoters, wantLearners, wantEpoch)
	if voters, learners, epoch, err := store.LoadMembershipEpoch(); err != nil ||
		!reflect.DeepEqual(voters, wantVoters) || !reflect.DeepEqual(learners, wantLearners) || epoch != wantEpoch {
		t.Fatalf("persisted membership = %v %v epoch %d (%v), want %v %v epoch %d",
			voters, learners, epoch, err, wantVoters, wantLearners, wantEpoch)
	}
}

// TestLegacySnapshotWithoutMembershipFallsBack ensures a snapshot written by an
// older version (no membership envelope) still loads, using the persisted
// MembershipStore record or the static configuration.
func TestLegacySnapshotWithoutMembershipFallsBack(t *testing.T) {
	t.Run("persisted membership", func(t *testing.T) {
		dir := t.TempDir()
		store, err := raft.NewFileStore(dir)
		if err != nil {
			t.Fatalf("NewFileStore: %v", err)
		}
		if err := store.SaveMembershipEpoch([]string{"n2", "n3"}, []string{"n1"}, 4); err != nil {
			t.Fatalf("SaveMembershipEpoch: %v", err)
		}
		// No Membership field: exactly the legacy on-disk format.
		if err := store.SaveSnapshot(snapshotWithMembership(t, 5, 2, nil)); err != nil {
			t.Fatalf("SaveSnapshot legacy: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		reopened, err := raft.NewFileStore(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer reopened.Close()
		cfg := snapshotTestConfig("n1", []string{"n9"}, dir, 100*time.Millisecond, 200*time.Millisecond, 20*time.Millisecond)
		node, err := raft.New(cfg, reopened, newTestSM(), &testTransferTransport{cluster: &testCluster{nodes: map[string]*raft.Node{}}, id: "n1"})
		if err != nil {
			t.Fatalf("raft.New with legacy snapshot: %v", err)
		}
		defer node.Stop()

		if voters, learners := node.Membership(); !reflect.DeepEqual(voters, []string{"n2", "n3"}) ||
			!reflect.DeepEqual(learners, []string{"n1"}) {
			t.Fatalf("membership = %v %v, want persisted [n2 n3] [n1]", voters, learners)
		}
		if got := node.Status().ConfigEpoch; got != 4 {
			t.Fatalf("config epoch = %d, want persisted 4", got)
		}
		if snap, err := reopened.LoadSnapshot(); err != nil || snap.Index != 5 || snap.Membership != nil {
			t.Fatalf("loaded legacy snapshot = %+v, %v; want index 5 and nil membership", snap, err)
		}
	})

	t.Run("static config", func(t *testing.T) {
		dir := t.TempDir()
		store, err := raft.NewFileStore(dir)
		if err != nil {
			t.Fatalf("NewFileStore: %v", err)
		}
		if err := store.SaveSnapshot(snapshotWithMembership(t, 5, 2, nil)); err != nil {
			t.Fatalf("SaveSnapshot legacy: %v", err)
		}
		defer store.Close()

		cfg := snapshotTestConfig("n1", []string{"n2", "n3"}, dir, 100*time.Millisecond, 200*time.Millisecond, 20*time.Millisecond)
		node, err := raft.New(cfg, store, newTestSM(), &testTransferTransport{cluster: &testCluster{nodes: map[string]*raft.Node{}}, id: "n1"})
		if err != nil {
			t.Fatalf("raft.New with legacy snapshot: %v", err)
		}
		defer node.Stop()
		if voters, learners := node.Membership(); !reflect.DeepEqual(voters, []string{"n1", "n2", "n3"}) ||
			len(learners) != 0 {
			t.Fatalf("membership = %v %v, want static [n1 n2 n3]", voters, learners)
		}
	})
}

// TestInstallSnapshotChunkUpdatesMembership drives the chunked receiver with a
// two-chunk upload that carries membership and verifies the installed node (and
// its restart) uses the snapshot's configuration.
func TestInstallSnapshotChunkUpdatesMembership(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	cfg := snapshotTestConfig("n1", []string{"n2", "n3"}, dir, 100*time.Millisecond, 200*time.Millisecond, 20*time.Millisecond)
	node, err := raft.New(cfg, store, newStreamSnapshotSM(), &testTransferTransport{cluster: &testCluster{nodes: map[string]*raft.Node{}}, id: "n1"})
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	defer node.Stop()

	data, err := json.Marshal(snapshotPayload{Index: 5, Values: map[string]string{"a": "1"}})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	membership := &raft.SnapshotMembership{
		Voters:      []string{"n2", "n4"},
		Learners:    []string{"n1"},
		ConfigEpoch: 7,
	}
	half := len(data) / 2
	first := &raft.InstallSnapshotChunkRequest{
		Term: 1, LeaderID: "leader", Index: 5, SnapshotTerm: 2,
		Offset: 0, Data: data[:half], Size: int64(len(data)),
		CRC32: crc32.ChecksumIEEE(data), Membership: membership,
	}
	resp, err := node.HandleInstallSnapshotChunk(context.Background(), first)
	if err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	if resp == nil || !resp.Success {
		t.Fatalf("first chunk response = %+v, want success", resp)
	}
	last := &raft.InstallSnapshotChunkRequest{
		Term: 1, LeaderID: "leader", Index: 5, SnapshotTerm: 2,
		Offset: uint64(half), Data: data[half:], Done: true, Size: int64(len(data)),
		CRC32: crc32.ChecksumIEEE(data), Membership: membership,
	}
	resp, err = node.HandleInstallSnapshotChunk(context.Background(), last)
	if err != nil {
		t.Fatalf("final chunk: %v", err)
	}
	if resp == nil || !resp.Success || resp.LastIndex != 5 {
		t.Fatalf("final chunk response = %+v, want success at index 5", resp)
	}

	if voters, learners := node.Membership(); !reflect.DeepEqual(voters, membership.Voters) ||
		!reflect.DeepEqual(learners, membership.Learners) {
		t.Fatalf("membership after chunk install = %v %v, want %v %v",
			voters, learners, membership.Voters, membership.Learners)
	}
	if got := node.Status().ConfigEpoch; got != membership.ConfigEpoch {
		t.Fatalf("config epoch after chunk install = %d, want %d", got, membership.ConfigEpoch)
	}
	if voters, learners, epoch, err := store.LoadMembershipEpoch(); err != nil ||
		!reflect.DeepEqual(voters, membership.Voters) || !reflect.DeepEqual(learners, membership.Learners) ||
		epoch != membership.ConfigEpoch {
		t.Fatalf("persisted membership = %v %v epoch %d (%v), want %v %v epoch %d",
			voters, learners, epoch, err, membership.Voters, membership.Learners, membership.ConfigEpoch)
	}
	stored, err := store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stored.Index != 5 || stored.Membership == nil ||
		!reflect.DeepEqual(stored.Membership.Voters, membership.Voters) ||
		!reflect.DeepEqual(stored.Membership.Learners, membership.Learners) ||
		stored.Membership.ConfigEpoch != membership.ConfigEpoch {
		t.Fatalf("stored snapshot membership = %+v, want %+v", stored.Membership, membership)
	}

	// Restarting from the durable store restores the installed configuration.
	node.Stop()
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	restarted, err := raft.New(cfg, reopened, newStreamSnapshotSM(), &testTransferTransport{cluster: &testCluster{nodes: map[string]*raft.Node{}}, id: "n1"})
	if err != nil {
		t.Fatalf("restart raft.New: %v", err)
	}
	defer restarted.Stop()
	if voters, learners := restarted.Membership(); !reflect.DeepEqual(voters, membership.Voters) ||
		!reflect.DeepEqual(learners, membership.Learners) {
		t.Fatalf("membership after restart = %v %v, want %v %v",
			voters, learners, membership.Voters, membership.Learners)
	}
}

// TestSnapshotMembershipRejectsStaleAndInvalid checks the epoch guard on both
// restore paths and on the file store, and that invalid envelopes are rejected
// before any state changes.
func TestSnapshotMembershipRejectsStaleAndInvalid(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	cfg := snapshotTestConfig("n1", []string{"n2", "n3"}, dir, 100*time.Millisecond, 200*time.Millisecond, 20*time.Millisecond)
	node, err := raft.New(cfg, store, newTestSM(), &testTransferTransport{cluster: &testCluster{nodes: map[string]*raft.Node{}}, id: "n1"})
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	defer node.Stop()

	current := snapshotWithMembership(t, 10, 2, &raft.SnapshotMembership{
		Voters:      []string{"n2", "n3"},
		Learners:    []string{"n1"},
		ConfigEpoch: 5,
	})
	if err := node.RestoreSnapshot(current); err != nil {
		t.Fatalf("RestoreSnapshot current: %v", err)
	}
	votersBefore, learnersBefore := node.Membership()
	epochBefore := node.Status().ConfigEpoch

	stale := snapshotWithMembership(t, 11, 3, &raft.SnapshotMembership{
		Voters:      []string{"n1"},
		ConfigEpoch: 4,
	})
	if err := node.RestoreSnapshot(stale); !errors.Is(err, raft.ErrStaleSnapshotMembership) {
		t.Fatalf("RestoreSnapshot stale = %v, want ErrStaleSnapshotMembership", err)
	}
	if voters, learners := node.Membership(); !reflect.DeepEqual(voters, votersBefore) ||
		!reflect.DeepEqual(learners, learnersBefore) {
		t.Fatalf("stale snapshot changed membership to %v %v", voters, learners)
	}
	if got := node.Status().ConfigEpoch; got != epochBefore {
		t.Fatalf("stale snapshot changed epoch to %d, want %d", got, epochBefore)
	}

	invalid := snapshotWithMembership(t, 12, 4, &raft.SnapshotMembership{
		Voters:      []string{"n2", "n2"},
		ConfigEpoch: 6,
	})
	if err := node.RestoreSnapshot(invalid); !errors.Is(err, raft.ErrInvalidSnapshotMembership) {
		t.Fatalf("RestoreSnapshot invalid = %v, want ErrInvalidSnapshotMembership", err)
	}
	if voters, _ := node.Membership(); !reflect.DeepEqual(voters, votersBefore) {
		t.Fatalf("invalid snapshot changed membership to %v, want %v", voters, votersBefore)
	}

	// The streaming path enforces the same guards before consuming the stream.
	meta := raft.SnapshotMeta{Index: 11, Term: 3, Membership: stale.Membership}
	if err := node.RestoreSnapshotStream(meta, bytes.NewReader(stale.Data)); !errors.Is(err, raft.ErrStaleSnapshotMembership) {
		t.Fatalf("RestoreSnapshotStream stale = %v, want ErrStaleSnapshotMembership", err)
	}
	badMeta := raft.SnapshotMeta{Index: 12, Term: 4, Membership: &raft.SnapshotMembership{
		Voters:      nil,
		ConfigEpoch: 6,
	}}
	if err := node.RestoreSnapshotStream(badMeta, bytes.NewReader(current.Data)); !errors.Is(err, raft.ErrInvalidSnapshotMembership) {
		t.Fatalf("RestoreSnapshotStream invalid = %v, want ErrInvalidSnapshotMembership", err)
	}

	// A newer snapshot with a higher epoch replaces the configuration and drops
	// the old voters; the snapshot wins over local state.
	newer := snapshotWithMembership(t, 13, 4, &raft.SnapshotMembership{
		Voters:      []string{"n4"},
		Learners:    []string{"n2"},
		ConfigEpoch: 6,
	})
	if err := node.RestoreSnapshot(newer); err != nil {
		t.Fatalf("RestoreSnapshot newer: %v", err)
	}
	if voters, learners := node.Membership(); !reflect.DeepEqual(voters, []string{"n4"}) ||
		!reflect.DeepEqual(learners, []string{"n2"}) {
		t.Fatalf("membership after newer snapshot = %v %v, want [n4] [n2]", voters, learners)
	}
	if got := node.Status().ConfigEpoch; got != 6 {
		t.Fatalf("config epoch after newer snapshot = %d, want 6", got)
	}

	// The file store refuses to commit a newer-index snapshot with an older
	// config epoch, and leaves the previous snapshot intact.
	staleData := snapshotWithMembership(t, 20, 5, nil).Data
	err = store.SaveSnapshotFromReaderWithMembership(20, 5, &raft.SnapshotMembership{
		Voters:      []string{"n1"},
		ConfigEpoch: 3,
	}, bytes.NewReader(staleData))
	if !errors.Is(err, raft.ErrStaleSnapshotMembership) {
		t.Fatalf("SaveSnapshotFromReaderWithMembership stale = %v, want ErrStaleSnapshotMembership", err)
	}
	stored, err := store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot after rejected save: %v", err)
	}
	if stored.Index != 13 {
		t.Fatalf("stored snapshot index = %d after rejected save, want 13", stored.Index)
	}
}
