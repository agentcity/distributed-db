package raft_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
	"distributed-db/internal/rafttest"
)

const (
	testElectionMin = 40 * time.Millisecond
	testElectionMax = 80 * time.Millisecond
	testHeartbeat   = 10 * time.Millisecond
	testWait        = 2 * time.Second
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testSM applies entries strictly in index order and records them by index.
type testSM struct {
	mu         sync.Mutex
	lastIndex  uint64
	byIndex    map[uint64]model.Entry
	values     map[string][]byte
	applyCalls int
	gate       chan struct{}
}

func newTestSM() *testSM {
	return &testSM{
		byIndex: make(map[uint64]model.Entry),
		values:  make(map[string][]byte),
	}
}

func (s *testSM) Apply(entry model.Entry) error {
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyCalls++
	if entry.Index <= s.lastIndex {
		// A restarted node replays its log from index 1. A real state machine
		// (internal/kv) is idempotent; the test model simply ignores replays.
		return nil
	}
	if entry.Index != s.lastIndex+1 {
		return fmt.Errorf("state machine apply out of order: got %d after %d", entry.Index, s.lastIndex)
	}
	s.lastIndex = entry.Index
	s.byIndex[entry.Index] = entry
	switch entry.Type {
	case model.CommandPut:
		s.values[entry.Key] = append([]byte(nil), entry.Value...)
	case model.CommandDelete:
		delete(s.values, entry.Key)
	case model.CommandNoop:
	default:
		return fmt.Errorf("unknown command type %q", entry.Type)
	}
	return nil
}

func (s *testSM) get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

func (s *testSM) appliedIndex() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastIndex
}

func (s *testSM) entryAt(index uint64) (model.Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byIndex[index]
	return e, ok
}

// gateApplies blocks all future Apply calls until the returned release func is
// called. It is used to hold a new leader's apply loop behind its commit index
// while testing ReadBarrier.
func (s *testSM) gateApplies() func() {
	s.mu.Lock()
	gate := make(chan struct{})
	s.gate = gate
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.gate == gate {
				s.gate = nil
			}
			s.mu.Unlock()
			close(gate)
		})
	}
}

type testCluster struct {
	t           *testing.T
	net         *rafttest.Network
	ids         []string
	ctx         context.Context
	cancel      context.CancelFunc
	nodes       map[string]*raft.Node
	sms         map[string]*testSM
	stores      map[string]*raft.FileStore
	dirs        map[string]string
	electionMin time.Duration
	electionMax time.Duration
	heartbeat   time.Duration
}

func testConfig(id string, peers []string, dir string) raft.Config {
	return raft.Config{
		ID:                 id,
		Peers:              append([]string(nil), peers...),
		DataDir:            dir,
		ElectionTimeoutMin: testElectionMin,
		ElectionTimeoutMax: testElectionMax,
		HeartbeatInterval:  testHeartbeat,
		Logger:             discardLogger(),
	}
}

// configFor builds a config matching this cluster's timers, so a node replaced
// after a stop/restart keeps the same timing characteristics.
func (c *testCluster) configFor(id string, peers []string, dir string) raft.Config {
	return raft.Config{
		ID:                 id,
		Peers:              append([]string(nil), peers...),
		DataDir:            dir,
		ElectionTimeoutMin: c.electionMin,
		ElectionTimeoutMax: c.electionMax,
		HeartbeatInterval:  c.heartbeat,
		Logger:             discardLogger(),
	}
}

func peersExcept(ids []string, id string) []string {
	out := make([]string, 0, len(ids)-1)
	for _, other := range ids {
		if other != id {
			out = append(out, other)
		}
	}
	return out
}

func newTestCluster(t *testing.T, size int) *testCluster {
	t.Helper()
	return newTestClusterWithTimeouts(t, size, testElectionMin, testElectionMax, testHeartbeat)
}

func newTestClusterWithTimeouts(t *testing.T, size int, electionMin, electionMax, heartbeat time.Duration) *testCluster {
	t.Helper()
	ids := make([]string, size)
	for i := range ids {
		ids[i] = fmt.Sprintf("n%d", i+1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &testCluster{
		t:           t,
		net:         rafttest.NewNetwork(),
		ids:         ids,
		ctx:         ctx,
		cancel:      cancel,
		nodes:       make(map[string]*raft.Node),
		sms:         make(map[string]*testSM),
		stores:      make(map[string]*raft.FileStore),
		dirs:        make(map[string]string),
		electionMin: electionMin,
		electionMax: electionMax,
		heartbeat:   heartbeat,
	}
	t.Cleanup(func() {
		c.cancel()
		for _, n := range c.nodes {
			n.Stop()
		}
		for _, store := range c.stores {
			_ = store.Close()
		}
	})

	for _, id := range ids {
		dir := t.TempDir()
		store, err := raft.NewFileStore(dir)
		if err != nil {
			t.Fatalf("NewFileStore(%s): %v", id, err)
		}
		sm := newTestSM()
		tr := &testTransferTransport{Transport: c.net.Transport(id), cluster: c, id: id}
		node, err := raft.New(c.configFor(id, peersExcept(ids, id), dir), store, sm, tr)
		if err != nil {
			t.Fatalf("raft.New(%s): %v", id, err)
		}
		c.dirs[id] = dir
		c.stores[id] = store
		c.sms[id] = sm
		c.nodes[id] = node
		c.net.Register(id, node)
		if err := node.Start(ctx); err != nil {
			t.Fatalf("Start(%s): %v", id, err)
		}
	}
	return c
}

// waitLeader waits for exactly one leader among ids and requires it to remain
// leader for a short settling period, so callers do not race a fresh election.
func (c *testCluster) waitLeader(ids []string, timeout time.Duration) (*raft.Node, string) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var candidate string
	var since time.Time
	for time.Now().Before(deadline) {
		var leaders []string
		for _, id := range ids {
			node := c.nodes[id]
			if node != nil && node.IsLeader() {
				leaders = append(leaders, id)
			}
		}
		switch {
		case len(leaders) == 1:
			if leaders[0] != candidate {
				candidate = leaders[0]
				since = time.Now()
			} else if time.Since(since) >= 20*time.Millisecond {
				return c.nodes[candidate], candidate
			}
		default:
			candidate = ""
			since = time.Time{}
		}
		time.Sleep(time.Millisecond)
	}
	c.t.Fatalf("no stable leader among %v within %v", ids, timeout)
	return nil, ""
}

func eventually(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !fn() {
		t.Fatalf("condition not met within %v: %s", timeout, what)
	}
}

func propose(t *testing.T, n *raft.Node, cmd model.Entry, timeout time.Duration) (uint64, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return n.Propose(ctx, cmd)
}

// proposeWithRetry proposes to whichever node is currently leader among ids,
// re-waiting for an election if the leader steps down before committing. The
// put/delete test commands are idempotent, so an ambiguous timeout is safe to
// retry.
func (c *testCluster) proposeWithRetry(ids []string, cmd model.Entry, timeout time.Duration) (uint64, *raft.Node, string) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if time.Now().After(deadline) {
			c.t.Fatalf("Propose did not succeed after retries: %v", lastErr)
		}
		leader, leaderID := c.waitLeader(ids, time.Until(deadline))
		remaining := time.Until(deadline)
		if remaining > testWait {
			remaining = testWait
		}
		index, err := propose(c.t, leader, cmd, remaining)
		if err == nil {
			return index, leader, leaderID
		}
		lastErr = err
		if !errors.Is(err, raft.ErrNotLeader) && !errors.Is(err, raft.ErrTimeout) {
			c.t.Fatalf("Propose on %s: %v", leaderID, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *testCluster) requireValue(ids []string, key, want string, timeout time.Duration) {
	c.t.Helper()
	eventually(c.t, timeout, fmt.Sprintf("all nodes apply %s=%s", key, want), func() bool {
		for _, id := range ids {
			got, ok := c.sms[id].get(key)
			if !ok || string(got) != want {
				return false
			}
		}
		return true
	})
}

func (c *testCluster) requireAppliedThrough(ids []string, index uint64, timeout time.Duration) {
	c.t.Helper()
	eventually(c.t, timeout, fmt.Sprintf("all nodes apply through index %d", index), func() bool {
		for _, id := range ids {
			node := c.nodes[id]
			if node == nil || node.Status().LastApplied < index || c.sms[id].appliedIndex() < index {
				return false
			}
		}
		return true
	})
}

func TestLeaderElection(t *testing.T) {
	c := newTestCluster(t, 3)
	leader, id := c.waitLeader(c.ids, testWait)

	st := leader.Status()
	if st.Role != raft.Leader {
		t.Fatalf("status role = %s, want leader", st.Role)
	}
	if st.Term == 0 {
		t.Fatal("leader term is 0")
	}
	if st.Leader != id || leader.Leader() != id {
		t.Fatalf("leader id mismatch: status=%q Leader()=%q want %q", st.Leader, leader.Leader(), id)
	}
	if !leader.IsLeader() {
		t.Fatal("IsLeader() = false for elected leader")
	}

	eventually(t, testWait, "all nodes agree on the leader", func() bool {
		leaders := 0
		for _, nodeID := range c.ids {
			node := c.nodes[nodeID]
			if node.IsLeader() {
				leaders++
			}
			if node.Leader() != id {
				return false
			}
		}
		return leaders == 1
	})
}

func TestReplication(t *testing.T) {
	c := newTestCluster(t, 3)
	leader, _ := c.waitLeader(c.ids, testWait)

	index, err := propose(t, leader, model.Entry{Type: model.CommandPut, Key: "key", Value: []byte("value")}, testWait)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if index == 0 {
		t.Fatal("Propose returned index 0")
	}

	c.requireValue(c.ids, "key", "value", testWait)
	c.requireAppliedThrough(c.ids, index, testWait)

	for _, id := range c.ids {
		entry, ok := c.sms[id].entryAt(index)
		if !ok {
			t.Fatalf("%s did not apply entry at index %d", id, index)
		}
		if entry.Index != index || entry.Key != "key" || string(entry.Value) != "value" {
			t.Fatalf("%s applied wrong entry at %d: %+v", id, index, entry)
		}
		if node := c.nodes[id]; node.Status().CommitIndex < index {
			t.Fatalf("%s commit index %d < %d", id, node.Status().CommitIndex, index)
		}
	}
}

func TestFailoverKeepsCommittedValue(t *testing.T) {
	// Realistic timers: this test stops/restarts a leader and fsyncs under
	// -race, so a 40ms lease can expire while a follower is descheduled.
	c := newTestClusterWithTimeouts(t, 3, 120*time.Millisecond, 250*time.Millisecond, 25*time.Millisecond)
	leader, oldLeader := c.waitLeader(c.ids, testWait)

	beforeIndex, err := propose(t, leader, model.Entry{Type: model.CommandPut, Key: "before", Value: []byte("committed")}, testWait)
	if err != nil {
		t.Fatalf("Propose before failover: %v", err)
	}
	c.requireValue(c.ids, "before", "committed", testWait)

	leader.Stop()
	remaining := peersExcept(c.ids, oldLeader)
	c.requireValue(remaining, "before", "committed", testWait)

	afterIndex, newLeader, newLeaderID := c.proposeWithRetry(remaining, model.Entry{
		Type: model.CommandPut, Key: "after", Value: []byte("failover"),
	}, testWait)
	if newLeaderID == oldLeader {
		t.Fatalf("stopped node %s still reports itself leader", oldLeader)
	}
	if afterIndex <= beforeIndex {
		t.Fatalf("new proposal index %d not after %d", afterIndex, beforeIndex)
	}
	c.requireValue(remaining, "after", "failover", testWait)
	if newLeader.Status().CommitIndex < afterIndex {
		t.Fatalf("new leader commit index %d < %d", newLeader.Status().CommitIndex, afterIndex)
	}

	// Restart the old node from the same durable store and let it catch up.
	restarted, err := raft.New(
		c.configFor(oldLeader, remaining, c.dirs[oldLeader]),
		c.stores[oldLeader],
		c.sms[oldLeader],
		c.net.Transport(oldLeader),
	)
	if err != nil {
		t.Fatalf("restart New: %v", err)
	}
	c.nodes[oldLeader] = restarted
	c.net.Register(oldLeader, restarted)
	if err := restarted.Start(c.ctx); err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	c.requireValue(c.ids, "before", "committed", testWait)
	c.requireValue(c.ids, "after", "failover", testWait)
	c.requireAppliedThrough(c.ids, afterIndex, testWait)
}

func TestReadBarrierWaitsForApplyAfterFailover(t *testing.T) {
	// Realistic timers: this test stops/restarts a leader and fsyncs under
	// -race, so a 40ms lease can expire while a follower is descheduled.
	c := newTestClusterWithTimeouts(t, 3, 120*time.Millisecond, 250*time.Millisecond, 25*time.Millisecond)
	leader, oldLeaderID := c.waitLeader(c.ids, testWait)

	if _, err := propose(t, leader, model.Entry{Type: model.CommandPut, Key: "barrier", Value: []byte("visible")}, testWait); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got, ok := c.sms[oldLeaderID].get("barrier"); !ok || string(got) != "visible" {
		t.Fatalf("old leader did not apply its committed value: %q, %v", got, ok)
	}

	remaining := peersExcept(c.ids, oldLeaderID)
	leader.Stop()

	// Hold the survivors' apply loops so the new leader is guaranteed to have
	// committed its no-op but not applied it. ReadBarrier must wait.
	var releases []func()
	for _, id := range remaining {
		releases = append(releases, c.sms[id].gateApplies())
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	newLeader, newLeaderID := c.waitLeader(remaining, testWait)
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	barrier := make(chan error, 1)
	go func() { barrier <- newLeader.ReadBarrier(ctx) }()

	select {
	case err := <-barrier:
		t.Fatalf("ReadBarrier returned before apply caught up: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	for _, release := range releases {
		release()
	}
	select {
	case err := <-barrier:
		if err != nil {
			t.Fatalf("ReadBarrier after apply: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("ReadBarrier did not return after apply caught up")
	}

	// No sleep or retry: once the barrier returns, a local read must observe
	// the value committed by the previous leader.
	got, ok := c.sms[newLeaderID].get("barrier")
	if !ok || string(got) != "visible" {
		t.Fatalf("read after barrier = %q, %v; want visible", got, ok)
	}
}

func TestReadBarrierOnLeader(t *testing.T) {
	c := newTestCluster(t, 3)
	leader, leaderID := c.waitLeader(c.ids, testWait)

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := leader.ReadBarrier(ctx); err != nil {
		t.Fatalf("ReadBarrier on healthy leader: %v", err)
	}
	for _, id := range c.ids {
		if id == leaderID {
			continue
		}
		if err := c.nodes[id].ReadBarrier(ctx); !errors.Is(err, raft.ErrNotLeader) {
			t.Fatalf("ReadBarrier on follower %s = %v, want ErrNotLeader", id, err)
		}
	}
}

func TestNoQuorumProposeTimesOut(t *testing.T) {
	c := newTestCluster(t, 3)
	leader, leaderID := c.waitLeader(c.ids, testWait)
	others := peersExcept(c.ids, leaderID)

	// Fully isolate the leader: both outgoing and incoming links.
	c.net.Partition(leaderID, others...)
	for _, other := range others {
		c.net.Partition(other, leaderID)
	}

	index, err := propose(t, leader, model.Entry{Type: model.CommandPut, Key: "no-quorum", Value: []byte("x")}, 250*time.Millisecond)
	if err == nil {
		t.Fatalf("Propose succeeded without quorum, index=%d", index)
	}
	if !errors.Is(err, raft.ErrTimeout) && !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("Propose error = %v, want ErrTimeout or ErrNotLeader", err)
	}
	if index != 0 {
		t.Fatalf("Propose returned index %d on failure", index)
	}

	eventually(t, testWait, "isolated leader steps down", func() bool {
		return leader.Status().Role != raft.Leader
	})
	for _, other := range others {
		if _, ok := c.sms[other].get("no-quorum"); ok {
			t.Fatalf("node %s applied an entry committed without quorum", other)
		}
	}
	barrierCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := leader.ReadBarrier(barrierCtx); !errors.Is(err, raft.ErrNotLeader) && !errors.Is(err, raft.ErrTimeout) {
		t.Fatalf("ReadBarrier on isolated leader = %v, want ErrNotLeader", err)
	}
}

func TestProposeFailsWhenLeadershipLost(t *testing.T) {
	c := newTestCluster(t, 3)
	leader, leaderID := c.waitLeader(c.ids, testWait)
	others := peersExcept(c.ids, leaderID)
	c.net.Partition(leaderID, others...)
	for _, other := range others {
		c.net.Partition(other, leaderID)
	}

	done := make(chan error, 1)
	go func() {
		// A background context never expires: the proposal must still be
		// released when this node loses leadership, not hang forever.
		_, err := leader.Propose(context.Background(), model.Entry{
			Type: model.CommandPut, Key: "hung", Value: []byte("x"),
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Propose succeeded after quorum loss")
		}
		if !errors.Is(err, raft.ErrNotLeader) && !errors.Is(err, raft.ErrTimeout) && !errors.Is(err, raft.ErrStopped) {
			t.Fatalf("Propose error = %v, want ErrNotLeader/ErrTimeout/ErrStopped", err)
		}
	case <-time.After(testWait):
		t.Fatal("Propose with a background context hung after leadership loss")
	}
}

// spyStore records Truncate calls so the conflict test can prove the divergent
// suffix was actually removed from the durable log.
type spyStore struct {
	*raft.FileStore
	mu    sync.Mutex
	trunc []uint64
}

func (s *spyStore) Truncate(fromIndex uint64) error {
	s.mu.Lock()
	s.trunc = append(s.trunc, fromIndex)
	s.mu.Unlock()
	return s.FileStore.Truncate(fromIndex)
}

func (s *spyStore) truncations() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.trunc...)
}

func TestLogConflictTruncatedAfterHeal(t *testing.T) {
	// Realistic timers: this test stops/restarts a leader and fsyncs under
	// -race, so a 40ms lease can expire while a follower is descheduled.
	c := newTestClusterWithTimeouts(t, 3, 120*time.Millisecond, 250*time.Millisecond, 25*time.Millisecond)
	leader, oldLeaderID := c.waitLeader(c.ids, testWait)
	others := peersExcept(c.ids, oldLeaderID)

	if _, err := propose(t, leader, model.Entry{Type: model.CommandPut, Key: "prefix", Value: []byte("p")}, testWait); err != nil {
		t.Fatalf("Propose prefix: %v", err)
	}
	c.requireValue(c.ids, "prefix", "p", testWait)

	// Isolate the leader and append an entry it can never commit.
	c.net.Partition(oldLeaderID, others...)
	for _, other := range others {
		c.net.Partition(other, oldLeaderID)
	}
	if _, err := propose(t, leader, model.Entry{Type: model.CommandPut, Key: "divergent", Value: []byte("x")}, 200*time.Millisecond); err == nil {
		t.Fatal("divergent Propose unexpectedly succeeded while partitioned")
	}
	divergentIndex := leader.Status().LastIndex
	if divergentIndex <= 1 {
		t.Fatalf("divergent entry not appended, last index = %d", divergentIndex)
	}

	// Stop the old leader so it cannot inflate its term while isolated; its
	// log on disk keeps the divergent suffix.
	leader.Stop()
	persisted, err := c.stores[oldLeaderID].Load()
	if err != nil {
		t.Fatalf("load old log: %v", err)
	}
	if divergentIndex > uint64(len(persisted.Entries)) {
		t.Fatalf("divergent index %d > persisted entries %d", divergentIndex, len(persisted.Entries))
	}
	divergentTerm := persisted.Entries[divergentIndex-1].Term

	// A majority elects a new leader and commits entries at the same indexes.
	newLeader, _ := c.waitLeader(others, testWait)
	eventually(t, testWait, "new leader commits at the divergent index", func() bool {
		return newLeader.Status().CommitIndex >= divergentIndex
	})
	winnerIndex, _, _ := c.proposeWithRetry(others, model.Entry{
		Type: model.CommandPut, Key: "winner", Value: []byte("w"),
	}, testWait)
	c.requireValue(others, "winner", "w", testWait)

	// Reopen the old log and restart the node after healing its links.
	if err := c.stores[oldLeaderID].Close(); err != nil {
		t.Fatalf("close old store: %v", err)
	}
	reopened, err := raft.NewFileStore(c.dirs[oldLeaderID])
	if err != nil {
		t.Fatalf("reopen old store: %v", err)
	}
	spy := &spyStore{FileStore: reopened}
	c.stores[oldLeaderID] = reopened

	c.net.Heal(oldLeaderID)
	restarted, err := raft.New(
		c.configFor(oldLeaderID, others, c.dirs[oldLeaderID]),
		spy,
		c.sms[oldLeaderID],
		c.net.Transport(oldLeaderID),
	)
	if err != nil {
		t.Fatalf("New restarted node: %v", err)
	}
	c.nodes[oldLeaderID] = restarted
	c.net.Register(oldLeaderID, restarted)
	if err := restarted.Start(c.ctx); err != nil {
		t.Fatalf("Start restarted node: %v", err)
	}

	c.requireValue(c.ids, "winner", "w", testWait)
	c.requireAppliedThrough(c.ids, winnerIndex, testWait)
	// Let the healed cluster settle again; the restarted node may have a
	// higher term and trigger one more election.
	c.waitLeader(c.ids, testWait)
	eventually(t, testWait, "restarted node truncates divergent suffix", func() bool {
		for _, from := range spy.truncations() {
			if from == divergentIndex {
				return true
			}
		}
		return false
	})

	finalState, err := reopened.Load()
	if err != nil {
		t.Fatalf("load converged log: %v", err)
	}
	if divergentIndex > uint64(len(finalState.Entries)) {
		t.Fatalf("entry %d missing after convergence", divergentIndex)
	}
	converged := finalState.Entries[divergentIndex-1]
	if converged.Term == divergentTerm {
		t.Fatalf("divergent entry (term %d) was not replaced", divergentTerm)
	}
	// The winner entry is committed and must be present verbatim.
	if winnerIndex > uint64(len(finalState.Entries)) {
		t.Fatalf("winner entry %d missing after convergence", winnerIndex)
	}
	winner := finalState.Entries[winnerIndex-1]
	if winner.Key != "winner" || string(winner.Value) != "w" || winner.Type != model.CommandPut {
		t.Fatalf("winner entry after convergence = %+v", winner)
	}
	if _, ok := c.sms[oldLeaderID].get("divergent"); ok {
		t.Fatal("restarted node applied the divergent entry")
	}
}

func TestRestartPreservesTermVoteAndLog(t *testing.T) {
	t.Run("vote persisted before granting", func(t *testing.T) {
		dir := t.TempDir()
		store, err := raft.NewFileStore(dir)
		if err != nil {
			t.Fatalf("NewFileStore: %v", err)
		}
		net := rafttest.NewNetwork()
		node, err := raft.New(testConfig("n1", []string{"n2", "n3"}, dir), store, newTestSM(), net.Transport("n1"))
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		resp, err := node.HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
			Term: 7, CandidateID: "n2",
		})
		if err != nil {
			t.Fatalf("HandleRequestVote: %v", err)
		}
		if !resp.VoteGranted {
			t.Fatal("vote was not granted")
		}
		node.Stop()
		if err := store.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		reopened, err := raft.NewFileStore(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer reopened.Close()
		state, err := reopened.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if state.Term != 7 || state.VotedFor != "n2" {
			t.Fatalf("persisted state = term %d vote %q, want 7/n2", state.Term, state.VotedFor)
		}

		restarted, err := raft.New(testConfig("n1", []string{"n2", "n3"}, dir), reopened, newTestSM(), net.Transport("n1"))
		if err != nil {
			t.Fatalf("restart New: %v", err)
		}
		resp, err = restarted.HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
			Term: 7, CandidateID: "n3",
		})
		if err != nil {
			t.Fatalf("HandleRequestVote after restart: %v", err)
		}
		if resp.VoteGranted {
			t.Fatal("node granted a second vote in the same term after restart")
		}
	})

	t.Run("node restarts with term vote and entries", func(t *testing.T) {
		dir := t.TempDir()
		store, err := raft.NewFileStore(dir)
		if err != nil {
			t.Fatalf("NewFileStore: %v", err)
		}
		sm := newTestSM()
		net := rafttest.NewNetwork()
		cfg := testConfig("n1", nil, dir)
		node, err := raft.New(cfg, store, sm, net.Transport("n1"))
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		if err := node.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		eventually(t, testWait, "single node becomes leader", node.IsLeader)
		index, err := propose(t, node, model.Entry{Type: model.CommandPut, Key: "durable", Value: []byte("yes")}, testWait)
		if err != nil {
			t.Fatalf("Propose: %v", err)
		}
		node.Stop()
		if err := store.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		reopened, err := raft.NewFileStore(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		state, err := reopened.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if state.Term == 0 {
			t.Fatal("term was not persisted")
		}
		if state.VotedFor != "n1" {
			t.Fatalf("votedFor = %q, want n1", state.VotedFor)
		}
		if uint64(len(state.Entries)) < index {
			t.Fatalf("log has %d entries, want at least %d", len(state.Entries), index)
		}
		entry := state.Entries[index-1]
		if entry.Index != index || entry.Key != "durable" || string(entry.Value) != "yes" {
			t.Fatalf("persisted entry = %+v", entry)
		}

		// A fresh state machine replays the durable log after restart.
		sm2 := newTestSM()
		restarted, err := raft.New(cfg, reopened, sm2, net.Transport("n1"))
		if err != nil {
			t.Fatalf("restart New: %v", err)
		}
		if err := restarted.Start(context.Background()); err != nil {
			t.Fatalf("restart Start: %v", err)
		}
		t.Cleanup(restarted.Stop)
		eventually(t, testWait, "restarted single node re-applies durable value", func() bool {
			got, ok := sm2.get("durable")
			return ok && string(got) == "yes"
		})
		if restarted.Status().Term < state.Term {
			t.Fatalf("restarted term %d < persisted %d", restarted.Status().Term, state.Term)
		}
	})
}
