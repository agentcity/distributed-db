package raft_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

// snapshotSM is a testSM that can produce and install snapshots. The payload
// records the applied index alongside the visible key/value state so a restore
// can rebuild the state machine without replaying the compacted log prefix.
type snapshotSM struct {
	*testSM
}

type snapshotPayload struct {
	Index  uint64            `json:"index"`
	Values map[string]string `json:"values"`
}

func newSnapshotSM() *snapshotSM {
	return &snapshotSM{testSM: newTestSM()}
}

func (s *snapshotSM) Snapshot() ([]byte, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload := snapshotPayload{
		Index:  s.lastIndex,
		Values: make(map[string]string, len(s.values)),
	}
	for k, v := range s.values {
		payload.Values[k] = string(v)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	return data, s.lastIndex, nil
}

func (s *snapshotSM) RestoreSnapshot(data []byte) error {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = make(map[string][]byte, len(payload.Values))
	for k, v := range payload.Values {
		s.values[k] = []byte(v)
	}
	// Indexes before the snapshot are no longer represented by log entries.
	s.byIndex = make(map[uint64]model.Entry)
	s.lastIndex = payload.Index
	return nil
}

// snapshotTestConfig mirrors testConfig but is kept local so these tests do not
// depend on how the older cluster helpers are structured.
func snapshotTestConfig(id string, peers []string, dir string, electionMin, electionMax, heartbeat time.Duration) raft.Config {
	return raft.Config{
		ID:                 id,
		Peers:              append([]string(nil), peers...),
		DataDir:            dir,
		ElectionTimeoutMin: electionMin,
		ElectionTimeoutMax: electionMax,
		HeartbeatInterval:  heartbeat,
		Logger:             discardLogger(),
	}
}

// ---------------------------------------------------------------------------
// A snapshot-capable in-memory transport
//
// internal/rafttest predates InstallSnapshot, so the snapshot tests use this
// small network. Transport capability is optional in the package, which is why
// runtimes that do not support snapshots keep compiling unchanged.
// ---------------------------------------------------------------------------

type snapshotNetwork struct {
	mu       sync.RWMutex
	handlers map[string]*raft.Node
	blocked  map[string]map[string]bool
	installs map[string]int // keyed by "from->to"
}

func newSnapshotNetwork() *snapshotNetwork {
	return &snapshotNetwork{
		handlers: make(map[string]*raft.Node),
		blocked:  make(map[string]map[string]bool),
		installs: make(map[string]int),
	}
}

func (sn *snapshotNetwork) countInstall(from, to string) int {
	sn.mu.RLock()
	defer sn.mu.RUnlock()
	return sn.installs[from+"->"+to]
}

func (sn *snapshotNetwork) recordInstall(from, to string) {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	sn.installs[from+"->"+to]++
}

func (sn *snapshotNetwork) register(id string, node *raft.Node) {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	sn.handlers[id] = node
}

func (sn *snapshotNetwork) partition(id string, peers ...string) {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	m := make(map[string]bool, len(peers))
	for _, p := range peers {
		m[p] = true
	}
	sn.blocked[id] = m
}

func (sn *snapshotNetwork) heal(id string) {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	delete(sn.blocked, id)
	for other := range sn.blocked {
		delete(sn.blocked[other], id)
	}
}

func (sn *snapshotNetwork) handler(from, to string) (*raft.Node, error) {
	sn.mu.RLock()
	defer sn.mu.RUnlock()
	if sn.blocked[from][to] {
		return nil, errors.New("snapshot network: link blocked")
	}
	h, ok := sn.handlers[to]
	if !ok {
		return nil, errors.New("snapshot network: unknown node")
	}
	return h, nil
}

func (sn *snapshotNetwork) transport(from string) raft.Transport {
	return &snapshotTransport{net: sn, from: from}
}

type snapshotTransport struct {
	net  *snapshotNetwork
	from string
}

func (t *snapshotTransport) RequestVote(ctx context.Context, peer string, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	h, err := t.net.handler(t.from, peer)
	if err != nil {
		return nil, err
	}
	return h.HandleRequestVote(ctx, req)
}

func (t *snapshotTransport) AppendEntries(ctx context.Context, peer string, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	h, err := t.net.handler(t.from, peer)
	if err != nil {
		return nil, err
	}
	return h.HandleAppendEntries(ctx, req)
}

func (t *snapshotTransport) InstallSnapshot(ctx context.Context, peer string, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	h, err := t.net.handler(t.from, peer)
	if err != nil {
		return nil, err
	}
	t.net.recordInstall(t.from, peer)
	return h.HandleInstallSnapshot(ctx, req)
}

// ---------------------------------------------------------------------------
// Cluster helpers
// ---------------------------------------------------------------------------

type snapshotCluster struct {
	t       *testing.T
	net     *snapshotNetwork
	ids     []string
	ctx     context.Context
	cancel  context.CancelFunc
	nodes   map[string]*raft.Node
	sms     map[string]*snapshotSM
	stores  map[string]*raft.FileStore
	dirs    map[string]string
	timeout time.Duration
}

func newSnapshotCluster(t *testing.T, size int) *snapshotCluster {
	t.Helper()
	// Realistic timers: fsync plus -race can deschedule a follower long enough
	// for a 40ms lease to expire.
	const (
		electionMin = 120 * time.Millisecond
		electionMax = 250 * time.Millisecond
		heartbeat   = 25 * time.Millisecond
	)
	ids := make([]string, size)
	for i := range ids {
		ids[i] = fmt.Sprintf("n%d", i+1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &snapshotCluster{
		t:       t,
		net:     newSnapshotNetwork(),
		ids:     ids,
		ctx:     ctx,
		cancel:  cancel,
		nodes:   make(map[string]*raft.Node),
		sms:     make(map[string]*snapshotSM),
		stores:  make(map[string]*raft.FileStore),
		dirs:    make(map[string]string),
		timeout: 15 * time.Second,
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
		sm := newSnapshotSM()
		cfg := snapshotTestConfig(id, peersExcept(ids, id), dir, electionMin, electionMax, heartbeat)
		node, err := raft.New(cfg, store, sm, c.net.transport(id))
		if err != nil {
			t.Fatalf("raft.New(%s): %v", id, err)
		}
		c.dirs[id] = dir
		c.stores[id] = store
		c.sms[id] = sm
		c.nodes[id] = node
		c.net.register(id, node)
		if err := node.Start(ctx); err != nil {
			t.Fatalf("Start(%s): %v", id, err)
		}
	}
	return c
}

func (c *snapshotCluster) waitLeader(ids []string) (*raft.Node, string) {
	c.t.Helper()
	deadline := time.Now().Add(c.timeout)
	var candidate string
	var since time.Time
	for time.Now().Before(deadline) {
		var leaders []string
		for _, id := range ids {
			if node := c.nodes[id]; node != nil && node.IsLeader() {
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
	c.t.Fatalf("no stable leader among %v within %v", ids, c.timeout)
	return nil, ""
}

func (c *snapshotCluster) requireValue(ids []string, key, want string) {
	c.t.Helper()
	eventually(c.t, c.timeout, fmt.Sprintf("all nodes apply %s=%s", key, want), func() bool {
		for _, id := range ids {
			got, ok := c.sms[id].get(key)
			if !ok || string(got) != want {
				return false
			}
		}
		return true
	})
}

// ---------------------------------------------------------------------------
// Snapshot tests
// ---------------------------------------------------------------------------

func TestSnapshotAndRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
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
	t.Cleanup(node.Stop)
	eventually(t, testWait, "single node becomes leader", node.IsLeader)

	for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}, {"c", "3"}} {
		if _, err := propose(t, node, model.Entry{Type: model.CommandPut, Key: kv[0], Value: []byte(kv[1])}, testWait); err != nil {
			t.Fatalf("Propose %s: %v", kv[0], err)
		}
	}

	snap, err := node.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Index == 0 {
		t.Fatal("snapshot index is 0")
	}
	if snap.Term == 0 {
		t.Fatal("snapshot term is 0")
	}
	if snap.Index != node.Status().LastApplied {
		t.Fatalf("snapshot index %d != last applied %d", snap.Index, node.Status().LastApplied)
	}
	// The log prefix is compacted in memory: only entries after the snapshot
	// remain.
	if got := node.Status().LastIndex; got != snap.Index {
		t.Fatalf("last index after snapshot = %d, want %d", got, snap.Index)
	}
	var payload snapshotPayload
	if err := json.Unmarshal(snap.Data, &payload); err != nil {
		t.Fatalf("decode snapshot data: %v", err)
	}
	if payload.Index != snap.Index || payload.Values["a"] != "1" || payload.Values["b"] != "2" || payload.Values["c"] != "3" {
		t.Fatalf("snapshot payload = %+v", payload)
	}

	// A second call without new commits is a cheap no-op returning the same
	// snapshot.
	again, err := node.Snapshot()
	if err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
	if again.Index != snap.Index || !reflect.DeepEqual(again.Data, snap.Data) {
		t.Fatalf("second snapshot = {%d %d %d bytes}, want {%d %d %d bytes}",
			again.Index, again.Term, len(again.Data), snap.Index, snap.Term, len(snap.Data))
	}

	// The snapshot is durable and the durable log starts after its index.
	stored, err := store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stored.Index != snap.Index || stored.Term != snap.Term || !reflect.DeepEqual(stored.Data, snap.Data) {
		t.Fatalf("persisted snapshot = {%d %d %d bytes}", stored.Index, stored.Term, len(stored.Data))
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i, e := range state.Entries {
		if e.Index != snap.Index+1+uint64(i) {
			t.Fatalf("durable entry %d has index %d, want %d", i, e.Index, snap.Index+1+uint64(i))
		}
	}

	// Restore into a fresh node with a fresh state machine and an empty store.
	otherDir := t.TempDir()
	otherStore, err := raft.NewFileStore(otherDir)
	if err != nil {
		t.Fatalf("NewFileStore other: %v", err)
	}
	defer otherStore.Close()
	restoredSM := newSnapshotSM()
	restored, err := raft.New(snapshotTestConfig("n2", nil, otherDir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond), otherStore, restoredSM, net.transport("n2"))
	if err != nil {
		t.Fatalf("raft.New restored: %v", err)
	}
	net.register("n2", restored)
	if err := restored.Start(context.Background()); err != nil {
		t.Fatalf("Start restored: %v", err)
	}
	t.Cleanup(restored.Stop)

	if err := restored.RestoreSnapshot(snap); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	st := restored.Status()
	if st.CommitIndex != snap.Index || st.LastApplied != snap.Index {
		t.Fatalf("restored status = commit %d applied %d, want %d", st.CommitIndex, st.LastApplied, snap.Index)
	}
	for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}, {"c", "3"}} {
		got, ok := restoredSM.get(kv[0])
		if !ok || string(got) != kv[1] {
			t.Fatalf("restored state machine %s = %q, %v; want %q", kv[0], got, ok, kv[1])
		}
	}
	// Restore is idempotent and ignores older snapshots.
	if err := restored.RestoreSnapshot(snap); err != nil {
		t.Fatalf("idempotent RestoreSnapshot: %v", err)
	}
	older := raft.Snapshot{Index: snap.Index - 1, Term: snap.Term, Data: []byte("{}")}
	if err := restored.RestoreSnapshot(older); err != nil {
		t.Fatalf("older RestoreSnapshot: %v", err)
	}
	// The restored node's durable log also starts after the snapshot index.
	otherState, err := otherStore.Load()
	if err != nil {
		t.Fatalf("Load other: %v", err)
	}
	for i, e := range otherState.Entries {
		if e.Index != snap.Index+1+uint64(i) {
			t.Fatalf("restored durable entry %d has index %d", i, e.Index)
		}
	}
}

func TestSnapshotEmptyStateMachine(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	net := newSnapshotNetwork()
	node, err := raft.New(snapshotTestConfig("n1", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond), store, newSnapshotSM(), net.transport("n1"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	snap, err := node.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot on empty state machine: %v", err)
	}
	if snap.Index != 0 || snap.Data != nil {
		t.Fatalf("empty snapshot = %+v, want zero", snap)
	}
	if stored, err := store.LoadSnapshot(); err != nil || stored.Index != 0 {
		t.Fatalf("empty store snapshot = %+v, %v; want zero", stored, err)
	}
}

func TestSnapshotUnsupportedStateMachine(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	net := newSnapshotNetwork()
	// A plain testSM does not implement Snapshotter.
	node, err := raft.New(snapshotTestConfig("n1", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond), store, newTestSM(), net.transport("n1"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	if _, err := node.Snapshot(); !errors.Is(err, raft.ErrSnapshotUnsupported) {
		t.Fatalf("Snapshot = %v, want ErrSnapshotUnsupported", err)
	}
}

// legacyStore exposes only the original LogStore surface, hiding the optional
// snapshot capability. Node.Snapshot must still work (in-memory compaction)
// without breaking existing stores.
type legacyStore struct {
	raft.LogStore
}

func TestSnapshotWithLegacyLogStore(t *testing.T) {
	dir := t.TempDir()
	fs, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer fs.Close()
	net := newSnapshotNetwork()
	sm := newSnapshotSM()
	node, err := raft.New(snapshotTestConfig("n1", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond), legacyStore{LogStore: fs}, sm, net.transport("n1"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.register("n1", node)
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(node.Stop)
	eventually(t, testWait, "single node becomes leader", node.IsLeader)
	if _, err := propose(t, node, model.Entry{Type: model.CommandPut, Key: "k", Value: []byte("v")}, testWait); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	snap, err := node.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot with legacy store: %v", err)
	}
	if snap.Index < 2 {
		t.Fatalf("snapshot index = %d, want >= 2", snap.Index)
	}
	// The old store has no snapshot capability, so nothing was persisted there.
	if ss, ok := interface{}(fs).(raft.SnapshotStore); ok {
		if stored, err := ss.LoadSnapshot(); err != nil || stored.Index != 0 {
			t.Fatalf("legacy store unexpectedly persisted a snapshot: %+v, %v", stored, err)
		}
	}
}

func TestRestartAfterSnapshotRestoresStateMachine(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
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
	eventually(t, testWait, "single node becomes leader", node.IsLeader)
	for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}} {
		if _, err := propose(t, node, model.Entry{Type: model.CommandPut, Key: kv[0], Value: []byte(kv[1])}, testWait); err != nil {
			t.Fatalf("Propose %s: %v", kv[0], err)
		}
	}
	snap, err := node.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// One update lands after the snapshot and must be replayed from the log
	// suffix, not from the snapshot.
	afterIndex, err := propose(t, node, model.Entry{Type: model.CommandPut, Key: "c", Value: []byte("3")}, testWait)
	if err != nil {
		t.Fatalf("Propose after snapshot: %v", err)
	}
	node.Stop()
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	restoredSM := newSnapshotSM()
	restarted, err := raft.New(cfg, reopened, restoredSM, net.transport("n1"))
	if err != nil {
		t.Fatalf("restart New: %v", err)
	}
	net.register("n1", restarted)
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	t.Cleanup(restarted.Stop)

	eventually(t, testWait, "restarted node reapplies snapshot and suffix", func() bool {
		if got, ok := restoredSM.get("a"); !ok || string(got) != "1" {
			return false
		}
		if got, ok := restoredSM.get("b"); !ok || string(got) != "2" {
			return false
		}
		got, ok := restoredSM.get("c")
		return ok && string(got) == "3"
	})
	if st := restarted.Status(); st.LastApplied < afterIndex {
		t.Fatalf("restarted last applied %d < %d", st.LastApplied, afterIndex)
	}
	stored, err := reopened.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot after restart: %v", err)
	}
	if stored.Index != snap.Index {
		t.Fatalf("restored snapshot index %d, want %d", stored.Index, snap.Index)
	}
}

func TestFileStoreSnapshotReloadAfterPrefixTruncation(t *testing.T) {
	dir := t.TempDir()
	fs, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.SaveMeta(4, "n1"); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	entries := []model.Entry{
		testEntry(1, 1, "a", "1"),
		testEntry(2, 1, "b", "2"),
		testEntry(3, 2, "c", "3"),
		testEntry(4, 2, "d", "4"),
		testEntry(5, 2, "e", "5"),
	}
	if err := fs.Append(entries...); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := fs.LoadSnapshot(); err != nil {
		t.Fatalf("LoadSnapshot before save: %v", err)
	}
	// Truncating without a covering snapshot would lose data.
	if err := fs.TruncatePrefix(3); err == nil {
		t.Fatal("TruncatePrefix without a snapshot should fail")
	}
	want := raft.Snapshot{Index: 3, Term: 2, Data: []byte("snapshot-payload")}
	if err := fs.SaveSnapshot(want); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	got, err := fs.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if got.Index != want.Index || got.Term != want.Term || string(got.Data) != string(want.Data) {
		t.Fatalf("LoadSnapshot = %+v, want %+v", got, want)
	}
	if err := fs.TruncatePrefix(3); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	state, err := fs.Load()
	if err != nil {
		t.Fatalf("Load after truncate prefix: %v", err)
	}
	assertEntriesFrom(t, state.Entries, 4)
	if err := fs.Append(testEntry(6, 3, "f", "6")); err != nil {
		t.Fatalf("Append after prefix truncation: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reload: the snapshot survives and the log starts after its index.
	reopened, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err = reopened.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot after reopen: %v", err)
	}
	if got.Index != want.Index || got.Term != want.Term || string(got.Data) != string(want.Data) {
		t.Fatalf("reopened snapshot = %+v, want %+v", got, want)
	}
	state, err = reopened.Load()
	if err != nil {
		t.Fatalf("Load after reopen: %v", err)
	}
	assertEntriesFrom(t, state.Entries, 4)
	if len(state.Entries) != 3 || state.Entries[2].Index != 6 {
		t.Fatalf("reopened entries = %+v", state.Entries)
	}
	if state.Term != 4 || state.VotedFor != "n1" {
		t.Fatalf("reopened meta = term %d vote %q", state.Term, state.VotedFor)
	}

	// A newer snapshot supersedes the old one, and a second reload agrees.
	newer := raft.Snapshot{Index: 6, Term: 3, Data: []byte("newer")}
	if err := reopened.SaveSnapshot(newer); err != nil {
		t.Fatalf("SaveSnapshot newer: %v", err)
	}
	if err := reopened.TruncatePrefix(6); err != nil {
		t.Fatalf("TruncatePrefix newer: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close reopened: %v", err)
	}
	final, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("final reopen: %v", err)
	}
	defer final.Close()
	got, err = final.LoadSnapshot()
	if err != nil {
		t.Fatalf("final LoadSnapshot: %v", err)
	}
	if got.Index != 6 || got.Term != 3 || string(got.Data) != "newer" {
		t.Fatalf("final snapshot = %+v", got)
	}
	state, err = final.Load()
	if err != nil {
		t.Fatalf("final Load: %v", err)
	}
	if len(state.Entries) != 0 {
		t.Fatalf("final entries = %+v, want none after prefix 6", state.Entries)
	}
}

func TestFileStoreSnapshotReloadWithoutPrefixRecord(t *testing.T) {
	// Simulates a crash after SaveSnapshot but before TruncatePrefix: Load must
	// still start after the snapshot index.
	dir := t.TempDir()
	fs, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Append(
		testEntry(1, 1, "a", "1"),
		testEntry(2, 1, "b", "2"),
		testEntry(3, 1, "c", "3"),
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := fs.SaveSnapshot(raft.Snapshot{Index: 2, Term: 1, Data: []byte("s")}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := fs.Close(); err != nil {
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
	assertEntriesFrom(t, state.Entries, 3)
	if err := reopened.Append(testEntry(4, 2, "d", "4")); err != nil {
		t.Fatalf("Append after reload: %v", err)
	}
}

func assertEntriesFrom(t *testing.T, entries []model.Entry, first uint64) {
	t.Helper()
	for i, e := range entries {
		if e.Index != first+uint64(i) {
			t.Fatalf("entry %d has index %d, want %d", i, e.Index, first+uint64(i))
		}
	}
}

func TestLaggingFollowerCatchesUpViaSnapshot(t *testing.T) {
	c := newSnapshotCluster(t, 3)
	leader, leaderID := c.waitLeader(c.ids)
	lagging := ""
	for _, id := range c.ids {
		if id != leaderID {
			lagging = id
			break
		}
	}

	// Cut the lagging follower off in both directions so it misses everything.
	c.net.partition(lagging, leaderID)
	c.net.partition(leaderID, lagging)

	var lastIndex uint64
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("k%d", i)
		value := fmt.Sprintf("v%d", i)
		index, _, _ := c.proposeOnLeader(leader, model.Entry{Type: model.CommandPut, Key: key, Value: []byte(value)})
		lastIndex = index
	}
	if lastIndex == 0 {
		t.Fatal("no entries committed")
	}

	// Compact the leader's log. The follower is still missing this prefix.
	snap, err := leader.Snapshot()
	if err != nil {
		t.Fatalf("leader Snapshot: %v", err)
	}
	if snap.Index < lastIndex {
		t.Fatalf("snapshot index %d < last committed %d", snap.Index, lastIndex)
	}
	if got := leader.Status().LastIndex; got != snap.Index {
		t.Fatalf("leader last index after snapshot = %d, want %d", got, snap.Index)
	}
	leaderState, err := c.stores[leaderID].Load()
	if err != nil {
		t.Fatalf("leader store Load: %v", err)
	}
	assertEntriesFrom(t, leaderState.Entries, snap.Index+1)

	// Heal the link: the follower's nextIndex is inside the compacted prefix,
	// so the leader must install the snapshot, then resume AppendEntries.
	c.net.heal(lagging)
	c.net.heal(leaderID)

	eventually(t, c.timeout, "lagging follower restores the leader snapshot", func() bool {
		followerSnap, err := c.stores[lagging].LoadSnapshot()
		if err != nil {
			return false
		}
		return followerSnap.Index >= snap.Index
	})
	if got := c.net.countInstall(leaderID, lagging); got == 0 {
		t.Fatal("leader never sent an InstallSnapshot to the lagging follower")
	}
	c.requireValue(c.ids, "k3", "v3")
	eventually(t, c.timeout, "lagging follower applies through the snapshot index", func() bool {
		st := c.nodes[lagging].Status()
		return st.LastApplied >= snap.Index && c.sms[lagging].appliedIndex() >= snap.Index
	})

	// Replication continues normally after the install.
	index, _, _ := c.proposeOnLeader(c.nodes[leaderID], model.Entry{Type: model.CommandPut, Key: "after", Value: []byte("snapshot")})
	c.requireValue(c.ids, "after", "snapshot")
	eventually(t, c.timeout, "follower applies the post-snapshot entry", func() bool {
		return c.sms[lagging].appliedIndex() >= index
	})
}

// proposeOnLeader proposes on node and fails the test on an unexpected error,
// retrying while a leader settles.
func (c *snapshotCluster) proposeOnLeader(node *raft.Node, cmd model.Entry) (uint64, *raft.Node, string) {
	c.t.Helper()
	deadline := time.Now().Add(c.timeout)
	var lastErr error
	for {
		if time.Now().After(deadline) {
			c.t.Fatalf("Propose did not succeed: %v", lastErr)
		}
		if !node.IsLeader() {
			node, _ = c.waitLeader(c.ids)
		}
		remaining := time.Until(deadline)
		if remaining > testWait {
			remaining = testWait
		}
		index, err := propose(c.t, node, cmd, remaining)
		if err == nil {
			return index, node, node.Leader()
		}
		lastErr = err
		if !errors.Is(err, raft.ErrNotLeader) && !errors.Is(err, raft.ErrTimeout) {
			c.t.Fatalf("Propose: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestInstallSnapshotIgnoresStaleTerm(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	net := newSnapshotNetwork()
	node, err := raft.New(snapshotTestConfig("n1", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond), store, newSnapshotSM(), net.transport("n1"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	// Adopt a newer term first.
	if _, err := node.HandleRequestVote(context.Background(), &raft.RequestVoteRequest{Term: 5, CandidateID: "n2"}); err != nil {
		t.Fatalf("HandleRequestVote: %v", err)
	}
	resp, err := node.HandleInstallSnapshot(context.Background(), &raft.InstallSnapshotRequest{
		Term:     4,
		LeaderID: "n2",
		Snapshot: raft.Snapshot{Index: 9, Term: 4, Data: []byte(`{"index":9}`)},
	})
	if err != nil {
		t.Fatalf("HandleInstallSnapshot: %v", err)
	}
	if resp.Success || resp.Term != 5 {
		t.Fatalf("stale install response = %+v, want Term 5 Success false", resp)
	}
	if node.Status().LastApplied != 0 {
		t.Fatalf("stale snapshot advanced last applied to %d", node.Status().LastApplied)
	}
	if _, err := node.HandleInstallSnapshot(context.Background(), nil); err == nil {
		t.Fatal("nil InstallSnapshotRequest should fail")
	}
}
