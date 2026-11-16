package raft_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
	"distributed-db/internal/rafttest"
)

// countingBatchStore records every AppendBatch attempt and can inject a batch
// failure. Embedding FileStore means the shared Append path (used for the
// leader no-op) stays real, so the node exercises a genuine durable store.
type countingBatchStore struct {
	*raft.FileStore

	mu       sync.Mutex
	batches  [][]model.Entry
	failWith error
}

func (s *countingBatchStore) AppendBatch(entries []model.Entry) error {
	s.mu.Lock()
	s.batches = append(s.batches, append([]model.Entry(nil), entries...))
	fail := s.failWith
	s.mu.Unlock()
	if fail != nil {
		return fail
	}
	return s.FileStore.AppendBatch(entries)
}

func (s *countingBatchStore) setFail(err error) {
	s.mu.Lock()
	s.failWith = err
	s.mu.Unlock()
}

func (s *countingBatchStore) recordedBatches() [][]model.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]model.Entry, len(s.batches))
	copy(out, s.batches)
	return out
}

// fallbackStore deliberately does not implement raft.BatchAppender (the inner
// store is a named field, so its methods are not promoted). It counts Append
// calls so a test can prove the per-entry fallback is used.
type fallbackStore struct {
	inner *raft.FileStore

	mu      sync.Mutex
	appends []int
}

func (s *fallbackStore) Load() (raft.PersistentState, error) { return s.inner.Load() }

func (s *fallbackStore) SaveMeta(term uint64, votedFor string) error {
	return s.inner.SaveMeta(term, votedFor)
}

func (s *fallbackStore) Append(entries ...model.Entry) error {
	s.mu.Lock()
	s.appends = append(s.appends, len(entries))
	s.mu.Unlock()
	return s.inner.Append(entries...)
}

func (s *fallbackStore) Truncate(fromIndex uint64) error { return s.inner.Truncate(fromIndex) }

func (s *fallbackStore) appendSizes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.appends...)
}

// startSingleNode starts a peerless node so every successful proposal commits
// as soon as it is durably appended.
func startSingleNode(t *testing.T, store raft.LogStore, sm *testSM, window time.Duration, max int) *raft.Node {
	t.Helper()
	cfg := testConfig("n1", nil, t.TempDir())
	cfg.ProposeBatchWindow = window
	cfg.ProposeBatchMax = max
	node, err := raft.New(cfg, store, sm, rafttest.NewNetwork().Transport("n1"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(node.Stop)
	eventually(t, testWait, "single node becomes leader", node.IsLeader)
	return node
}

type proposeResult struct {
	index uint64
	err   error
}

// proposeConcurrently releases count proposals at once. When cancelIndex >= 0,
// that proposal is given a context that is already canceled before Propose is
// called; every other proposal gets a generous deadline.
func proposeConcurrently(n *raft.Node, prefix string, count, cancelIndex int) []proposeResult {
	results := make([]proposeResult, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if i == cancelIndex {
				cancel()
			}
			index, err := n.Propose(ctx, model.Entry{
				Type:  model.CommandPut,
				Key:   fmt.Sprintf("%s-%d", prefix, i),
				Value: []byte(fmt.Sprintf("value-%d", i)),
			})
			results[i] = proposeResult{index: index, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	return results
}

func requireContiguousIndices(t *testing.T, results []proposeResult, first uint64) []uint64 {
	t.Helper()
	indices := make([]uint64, 0, len(results))
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("proposal %d failed: %v", i, res.err)
		}
		if res.index == 0 {
			t.Fatalf("proposal %d returned index 0", i)
		}
		indices = append(indices, res.index)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	for i, index := range indices {
		if want := first + uint64(i); index != want {
			t.Fatalf("proposal indices not contiguous: got %v, want %d..%d", indices, first, first+uint64(len(indices)-1))
		}
	}
	return indices
}

func requireAppliedKeys(t *testing.T, sm *testSM, prefix string, count, skip int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if i == skip {
			continue
		}
		key := fmt.Sprintf("%s-%d", prefix, i)
		got, ok := sm.get(key)
		if !ok {
			t.Fatalf("entry %q was not applied", key)
		}
		if want := fmt.Sprintf("value-%d", i); string(got) != want {
			t.Fatalf("applied %q = %q, want %q", key, got, want)
		}
	}
}

func TestProposeCoalescesConcurrentProposalsIntoOneBatch(t *testing.T) {
	fs, err := raft.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	store := &countingBatchStore{FileStore: fs}
	sm := newTestSM()
	// Max == count makes the batch fill deterministically as soon as every
	// proposal has enqueued, so the test cannot split it on a slow scheduler.
	// The window is only a safety net and is never reached here.
	const count = 32
	node := startSingleNode(t, store, sm, 2*time.Second, count)

	results := proposeConcurrently(node, "batch", count, -1)
	indices := requireContiguousIndices(t, results, 2) // index 1 is the leader no-op

	batches := store.recordedBatches()
	if len(batches) != 1 {
		t.Fatalf("AppendBatch calls = %d, want 1 (sizes %v)", len(batches), batchSizes(batches))
	}
	if len(batches[0]) != count {
		t.Fatalf("batch size = %d, want %d", len(batches[0]), count)
	}
	for i, entry := range batches[0] {
		if entry.Index != indices[i] {
			t.Fatalf("batch entry %d index = %d, want %d", i, entry.Index, indices[i])
		}
	}

	requireAppliedKeys(t, sm, "batch", count, -1)
	if got, want := node.Status().LastIndex, uint64(count+1); got != want {
		t.Fatalf("last index = %d, want %d", got, want)
	}
	if got, want := node.Status().LastApplied, uint64(count+1); got != want {
		t.Fatalf("last applied = %d, want %d", got, want)
	}
}

func TestProposeBatchFailureFailsAllWaiters(t *testing.T) {
	fs, err := raft.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	injected := errors.New("injected batch failure")
	store := &countingBatchStore{FileStore: fs}
	sm := newTestSM()
	const count = 16
	node := startSingleNode(t, store, sm, 2*time.Second, count)
	store.setFail(injected)

	results := proposeConcurrently(node, "fail", count, -1)
	for i, res := range results {
		if res.err == nil {
			t.Fatalf("proposal %d succeeded despite store failure (index %d)", i, res.index)
		}
		if res.index != 0 {
			t.Fatalf("proposal %d returned index %d on failure", i, res.index)
		}
		if !errors.Is(res.err, injected) {
			t.Fatalf("proposal %d error = %v, want wrapped %v", i, res.err, injected)
		}
	}

	attempted := 0
	for _, batch := range store.recordedBatches() {
		attempted += len(batch)
	}
	if attempted != count {
		t.Fatalf("attempted %d entries across batches, want %d", attempted, count)
	}
	// Nothing from the failed batches may reach the in-memory or applied log.
	if got := node.Status().LastIndex; got != 1 {
		t.Fatalf("last index after failed batch = %d, want 1 (leader no-op only)", got)
	}
	eventually(t, testWait, "only the leader no-op was applied", func() bool {
		return sm.appliedIndex() == 1
	})
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("fail-%d", i)
		if _, ok := sm.get(key); ok {
			t.Fatalf("failed proposal %q was applied", key)
		}
	}
}

func TestProposeBatchCanceledWaiterDoesNotCorruptOthers(t *testing.T) {
	fs, err := raft.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	store := &countingBatchStore{FileStore: fs}
	sm := newTestSM()
	const count = 8
	const canceled = 3
	node := startSingleNode(t, store, sm, 2*time.Second, count)

	results := proposeConcurrently(node, "cancel", count, canceled)

	if results[canceled].err == nil {
		t.Fatalf("canceled proposal unexpectedly succeeded with index %d", results[canceled].index)
	}
	if results[canceled].index != 0 {
		t.Fatalf("canceled proposal returned index %d", results[canceled].index)
	}

	successful := make([]proposeResult, 0, count-1)
	for i, res := range results {
		if i == canceled {
			continue
		}
		successful = append(successful, res)
	}
	requireContiguousIndices(t, successful, 2)

	batches := store.recordedBatches()
	attempted := 0
	for _, batch := range batches {
		attempted += len(batch)
	}
	if attempted != count-1 {
		t.Fatalf("attempted %d entries, want %d (canceled proposal must be dropped)", attempted, count-1)
	}
	requireAppliedKeys(t, sm, "cancel", count, canceled)
	if _, ok := sm.get(fmt.Sprintf("cancel-%d", canceled)); ok {
		t.Fatal("canceled proposal was applied")
	}
	if got, want := node.Status().LastIndex, uint64(count); got != want {
		t.Fatalf("last index = %d, want %d", got, want)
	}
}

func TestProposeFallsBackWithoutBatchAppender(t *testing.T) {
	inner, err := raft.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	store := &fallbackStore{inner: inner}
	sm := newTestSM()
	const count = 16
	node := startSingleNode(t, store, sm, 2*time.Second, count)

	results := proposeConcurrently(node, "fallback", count, -1)
	requireContiguousIndices(t, results, 2)

	sizes := store.appendSizes()
	if len(sizes) != count+1 { // one leader no-op plus one Append per proposal
		t.Fatalf("Append calls = %d (sizes %v), want %d", len(sizes), sizes, count+1)
	}
	for i, size := range sizes {
		if size != 1 {
			t.Fatalf("Append call %d had %d entries, want 1", i, size)
		}
	}
	requireAppliedKeys(t, sm, "fallback", count, -1)
}

func batchSizes(batches [][]model.Entry) []int {
	sizes := make([]int, len(batches))
	for i, batch := range batches {
		sizes[i] = len(batch)
	}
	return sizes
}
