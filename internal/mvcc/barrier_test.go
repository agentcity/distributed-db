package mvcc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"distributed-db/internal/storage"
)

// gatedEngine wraps a real engine and lets a test hold the next batch Commit
// before it becomes visible. It is used to make the window between "the commit
// has a commitTS and a barrier entry" and "the batch is applied" observable.
type gatedEngine struct {
	storage.Engine
	armed   atomic.Bool
	started chan struct{}
	release chan struct{}
}

func newGatedEngine(t *testing.T) *gatedEngine {
	t.Helper()
	return &gatedEngine{Engine: openTestEngineAt(t, t.TempDir())}
}

func (e *gatedEngine) arm() {
	e.started = make(chan struct{})
	e.release = make(chan struct{})
	e.armed.Store(true)
}

func (e *gatedEngine) NewBatch() storage.Batch {
	return &gatedBatch{Batch: e.Engine.NewBatch(), engine: e}
}

type gatedBatch struct {
	storage.Batch
	engine *gatedEngine
}

func (b *gatedBatch) Commit() error {
	if b.engine.armed.CompareAndSwap(true, false) {
		close(b.engine.started)
		<-b.engine.release
	}
	return b.Batch.Commit()
}

func requireBarrierClear(t *testing.T, store *Store) {
	t.Helper()
	if got := store.PendingCommits(); got != 0 {
		t.Fatalf("PendingCommits() = %d, want 0", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := store.WaitApplied(ctx, 1<<63); err != nil {
		t.Fatalf("WaitApplied on a clear barrier: %v", err)
	}
}

// TestApplyBarrierWaitsForInFlightCommit blocks a 1PC batch Commit and checks
// that a snapshot barrier at its commitTS cannot return until the batch is
// visible.
func TestApplyBarrierWaitsForInFlightCommit(t *testing.T) {
	gate := newGatedEngine(t)
	store := New(gate)
	gate.arm()

	commitErr := make(chan error, 1)
	go func() {
		commitErr <- store.CommitSingle(
			[]Mutation{{Key: []byte("k"), Value: []byte("v")}},
			[]byte("k"), 10, 100)
	}()

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("commit never reached the engine batch")
	}

	if got := store.PendingCommits(); got != 1 {
		t.Fatalf("PendingCommits() while the batch is staged = %d, want 1", got)
	}
	if got := store.AppliedTS(); got >= 100 {
		t.Fatalf("AppliedTS() = %d while commit 100 is in flight", got)
	}
	if _, err := store.Get([]byte("k"), 100); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get before apply = %v, want ErrKeyNotFound", err)
	}

	// A barrier below the in-flight commit must not wait for it.
	if err := store.WaitApplied(context.Background(), 99); err != nil {
		t.Fatalf("WaitApplied(99) below the in-flight commit: %v", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- store.WaitApplied(context.Background(), 100) }()

	select {
	case err := <-waitErr:
		t.Fatalf("WaitApplied(100) returned before the batch applied: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(gate.release)
	if err := <-commitErr; err != nil {
		t.Fatalf("CommitSingle: %v", err)
	}

	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("WaitApplied(100): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitApplied(100) did not return after the commit applied")
	}

	requireValue(t, store, "k", 100, "v")
	requireBarrierClear(t, store)
	if got := store.AppliedTS(); got < 100 {
		t.Fatalf("AppliedTS() = %d after commit 100 applied", got)
	}
}

// TestApplyBarrierWaitHonoursContext checks timeout and cancellation, and that
// a release wakes the waiter and advances AppliedTS.
func TestApplyBarrierWaitHonoursContext(t *testing.T) {
	store := New(newFakeEngine())
	lease := store.AcquireCommitLease(100)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := store.WaitApplied(ctx, 100)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitApplied timeout error = %v, want context.DeadlineExceeded", err)
	}

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	err = store.WaitApplied(cancelled, 100)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitApplied cancellation error = %v, want context.Canceled", err)
	}

	lease.Release()
	lease.Release() // idempotent
	if got := store.PendingCommits(); got != 0 {
		t.Fatalf("PendingCommits after double Release = %d, want 0", got)
	}
	if got := store.AppliedTS(); got != 100 {
		t.Fatalf("AppliedTS after Release = %d, want 100", got)
	}
	if err := store.WaitApplied(context.Background(), 100); err != nil {
		t.Fatalf("WaitApplied after Release: %v", err)
	}
}

// TestApplyBarrierAppliedTSWithOutOfOrderLeases checks that AppliedTS is capped
// by what has actually been applied and that older in-flight commits hold the
// frontier back.
func TestApplyBarrierAppliedTSWithOutOfOrderLeases(t *testing.T) {
	store := New(newFakeEngine())

	base := store.AcquireCommitLease(50)
	base.Release()
	if got := store.AppliedTS(); got != 50 {
		t.Fatalf("AppliedTS after applying 50 = %d, want 50", got)
	}

	newer := store.AcquireCommitLease(200)
	older := store.AcquireCommitLease(100)
	if got := store.AppliedTS(); got != 50 {
		t.Fatalf("AppliedTS with 100 and 200 in flight = %d, want 50 (capped by applied)", got)
	}
	if err := store.WaitApplied(context.Background(), 99); err != nil {
		t.Fatalf("WaitApplied(99) while 100 is in flight: %v", err)
	}

	older.Release()
	if got := store.AppliedTS(); got != 100 {
		t.Fatalf("AppliedTS after applying 100 with 200 in flight = %d, want 100", got)
	}
	if err := store.WaitApplied(context.Background(), 100); err != nil {
		t.Fatalf("WaitApplied(100) after 100 applied: %v", err)
	}

	newer.Release()
	if got := store.AppliedTS(); got != 200 {
		t.Fatalf("AppliedTS after applying 200 = %d, want 200", got)
	}
}

// TestApplyBarrierTracksCommitSinglesBatch checks that every command in a 1PC
// batch is registered and that one barrier at the highest commitTS waits for
// the shared batch.
func TestApplyBarrierTracksCommitSinglesBatch(t *testing.T) {
	gate := newGatedEngine(t)
	store := New(gate)
	gate.arm()

	commands := []SingleCommand{
		{Mutations: []Mutation{{Key: []byte("a"), Value: []byte("va")}}, StartTS: 100, CommitTS: 200},
		{Mutations: []Mutation{{Key: []byte("b"), Value: []byte("vb")}}, StartTS: 101, CommitTS: 201},
		{Mutations: []Mutation{{Key: []byte("c"), Value: []byte("vc")}}, StartTS: 102, CommitTS: 202},
		// Invalid timestamps are rejected without a lease.
		{Mutations: []Mutation{{Key: []byte("d"), Value: []byte("vd")}}, StartTS: 300, CommitTS: 300},
	}
	errsCh := make(chan []error, 1)
	go func() { errsCh <- store.CommitSingles(commands) }()

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("CommitSingles never reached the engine batch")
	}

	if got := store.PendingCommits(); got != 3 {
		t.Fatalf("PendingCommits() while the batch is staged = %d, want 3", got)
	}
	if err := store.WaitApplied(context.Background(), 199); err != nil {
		t.Fatalf("WaitApplied(199) below the batch: %v", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- store.WaitApplied(context.Background(), 202) }()
	select {
	case err := <-waitErr:
		t.Fatalf("WaitApplied(202) returned before the batch applied: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(gate.release)
	errs := <-errsCh
	for i, err := range errs {
		if i == len(commands)-1 {
			if err == nil {
				t.Fatalf("command %d with commitTS == startTS succeeded", i)
			}
			continue
		}
		if err != nil {
			t.Fatalf("command %d: %v", i, err)
		}
	}
	if err := <-waitErr; err != nil {
		t.Fatalf("WaitApplied(202): %v", err)
	}

	requireValue(t, store, "a", 200, "va")
	requireKeyNotFound(t, store, "b", 200)
	requireValue(t, store, "b", 201, "vb")
	requireValue(t, store, "c", 202, "vc")
	requireBarrierClear(t, store)
	if got := store.AppliedTS(); got < 202 {
		t.Fatalf("AppliedTS() = %d after batch through 202", got)
	}
}

// TestApplyBarrierAbortDoesNotLeak checks that every failure path and an
// explicit rollback leaves the barrier empty.
func TestApplyBarrierAbortDoesNotLeak(t *testing.T) {
	t.Run("validation failure", func(t *testing.T) {
		store := New(newFakeEngine())
		err := store.CommitSingle(
			[]Mutation{{Key: []byte("d")}, {Key: []byte("d")}},
			nil, 30, 40)
		if err == nil {
			t.Fatal("duplicate-key CommitSingle succeeded")
		}
		requireBarrierClear(t, store)
	})

	t.Run("engine batch failure", func(t *testing.T) {
		engine := newFakeEngine()
		engine.failNextBatchCommit = errors.New("injected engine failure")
		store := New(engine)
		err := store.CommitSingle(
			[]Mutation{{Key: []byte("x"), Value: []byte("v")}},
			[]byte("x"), 20, 30)
		if err == nil {
			t.Fatal("CommitSingle succeeded despite an injected engine failure")
		}
		requireBarrierClear(t, store)
		if _, err := store.Get([]byte("x"), 30); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("Get after failed commit = %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("2PC missing lock", func(t *testing.T) {
		store := New(newFakeEngine())
		if err := store.Commit([][]byte{[]byte("missing")}, 10, 20); err == nil {
			t.Fatal("Commit without a lock succeeded")
		}
		requireBarrierClear(t, store)
	})

	t.Run("explicit abort", func(t *testing.T) {
		store := New(newFakeEngine())
		lease := store.AcquireCommitLease(400)
		if lease.CommitTS() != 400 {
			t.Fatalf("lease.CommitTS() = %d, want 400", lease.CommitTS())
		}
		lease.Abort()
		lease.Abort() // idempotent
		lease.Release()
		requireBarrierClear(t, store)
		if got := store.AppliedTS(); got != 0 {
			t.Fatalf("AppliedTS after abort = %d, want 0", got)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		store := New(newFakeEngine())
		if err := store.Prewrite(
			[]Mutation{{Key: []byte("r"), Value: []byte("v")}},
			[]byte("r"), 50); err != nil {
			t.Fatalf("Prewrite: %v", err)
		}
		lease := store.AcquireCommitLease(60)
		if err := store.Rollback([][]byte{[]byte("r")}, 50); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		lease.Abort()
		requireBarrierClear(t, store)

		// A rolled-back transaction cannot be committed later, and that
		// rejected attempt must not leave a lease behind.
		err := store.CommitSingle(
			[]Mutation{{Key: []byte("r"), Value: []byte("v")}},
			[]byte("r"), 50, 60)
		if err == nil {
			t.Fatal("CommitSingle of a rolled-back transaction succeeded")
		}
		requireBarrierClear(t, store)
		requireKeyNotFound(t, store, "r", 60)
	})
}

// TestApplyBarrierConcurrentCommits exercises concurrent 1PC commits with
// externally held leases (the caller-side commit lease) and a barrier above all
// of them. Once the barrier returns, every successful commit at or below it
// must be visible.
func TestApplyBarrierConcurrentCommits(t *testing.T) {
	store := newTestStore(t)
	const n = 32
	const base = uint64(1000)

	// Acquire leases before exposing the timestamps, then let the commits run
	// concurrently. This is the protocol a snapshot caller must follow.
	leases := make([]*CommitLease, n)
	for i := 0; i < n; i++ {
		leases[i] = store.AcquireCommitLease(base + uint64(i))
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("concurrent-%02d", i)
			ts := base + uint64(i)
			errs[i] = store.CommitSingle(
				[]Mutation{{Key: []byte(key), Value: []byte(key)}},
				[]byte(key), ts-1, ts)
			leases[i].Release()
		}(i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.WaitApplied(ctx, base+uint64(n-1)); err != nil {
		t.Fatalf("WaitApplied through the whole batch: %v", err)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("commit %d: %v", i, errs[i])
		}
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("concurrent-%02d", i)
		requireValue(t, store, key, base+uint64(n-1), key)
	}
	requireBarrierClear(t, store)
}

// TestApplyBarrierCompactsStaleHeap checks that a long-lived lease plus a
// stream of finished commits does not grow the internal min-heap without bound.
func TestApplyBarrierCompactsStaleHeap(t *testing.T) {
	store := New(newFakeEngine())
	blocker := store.AcquireCommitLease(1)
	defer blocker.Release()

	for i := 0; i < 1000; i++ {
		lease := store.AcquireCommitLease(1000 + uint64(i))
		lease.Release()
	}

	store.barrier.mu.Lock()
	heapLen := len(store.barrier.minHeap)
	pending := store.barrier.total
	store.barrier.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending leases = %d, want 1", pending)
	}
	if heapLen > 2*pending+64 {
		t.Fatalf("min-heap length = %d for %d pending lease(s); stale entries were not compacted", heapLen, pending)
	}
}

// TestApplyBarrierConcurrentLeases hammers the barrier API from several
// goroutines; it is mainly valuable under the race detector.
func TestApplyBarrierConcurrentLeases(t *testing.T) {
	store := New(newFakeEngine())
	const goroutines = 8
	const iterations = 40

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				ts := uint64(g*iterations + i + 1)
				lease := store.AcquireCommitLease(ts)
				if i%3 == 0 {
					lease.Abort()
				} else {
					lease.Release()
				}
				_ = store.AppliedTS()
				_ = store.PendingCommits()

				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				_ = store.WaitApplied(ctx, ts)
				cancel()
			}
		}(g)
	}
	wg.Wait()

	requireBarrierClear(t, store)
}
