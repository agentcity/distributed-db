package mvcc

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"distributed-db/internal/storage"
)

func TestCommitSingleVisibilityAndCleanup(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	mutations := []Mutation{{Key: []byte("a"), Value: []byte("v1")}}
	if err := store.CommitSingle(mutations, []byte("a"), 10, 20); err != nil {
		t.Fatalf("CommitSingle: %v", err)
	}

	requireKeyNotFound(t, store, "a", 19)
	requireValue(t, store, "a", 20, "v1")
	requireValue(t, store, "a", 1<<40, "v1")

	if engineHas(t, engine, lockKey([]byte("a"))) {
		t.Fatal("lock remains after CommitSingle")
	}
	// A fresh 1PC transaction writes no provisional version at startTS.
	if engineHas(t, engine, dataKey([]byte("a"), 10)) {
		t.Fatal("provisional data written at startTS")
	}
	if engineHas(t, engine, writeKey([]byte("a"), 10)) {
		t.Fatal("write record written at startTS")
	}

	rec, found, err := store.writeAt([]byte("a"), 20)
	if err != nil {
		t.Fatalf("writeAt: %v", err)
	}
	if !found || rec.kind != kindPut || rec.startTS != 10 {
		t.Fatalf("write record = %+v, found=%v; want put/startTS=10", rec, found)
	}
}

func TestCommitSingleIdempotent(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	mutations := []Mutation{{Key: []byte("a"), Value: []byte("v1")}}
	if err := store.CommitSingle(mutations, []byte("a"), 10, 20); err != nil {
		t.Fatalf("first CommitSingle: %v", err)
	}
	if err := store.CommitSingle(mutations, []byte("a"), 10, 20); err != nil {
		t.Fatalf("second CommitSingle: %v", err)
	}
	requireValue(t, store, "a", 20, "v1")
	if engineHas(t, engine, lockKey([]byte("a"))) {
		t.Fatal("lock remains after idempotent retry")
	}

	// A retry must not overwrite the already committed value: the first commit
	// is the transaction's outcome.
	retry := []Mutation{{Key: []byte("a"), Value: []byte("different")}}
	if err := store.CommitSingle(retry, []byte("a"), 10, 20); err != nil {
		t.Fatalf("retry with different value: %v", err)
	}
	requireValue(t, store, "a", 20, "v1")
}

func TestCommitSingleErrLocked(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	if err := store.Prewrite([]Mutation{{Key: []byte("b"), Value: []byte("other")}}, []byte("b"), 5); err != nil {
		t.Fatalf("Prewrite: %v", err)
	}
	err := store.CommitSingle([]Mutation{{Key: []byte("b"), Value: []byte("mine")}}, []byte("b"), 10, 20)
	requireErrIs(t, err, ErrLocked)

	requireErrIs(t, mustGetErr(store, "b", 20), ErrLocked)
	lock, locked, err := store.lockAt([]byte("b"))
	if err != nil || !locked || lock.startTS != 5 {
		t.Fatalf("lock = %+v, locked=%v, err=%v; want lock from startTS 5", lock, locked, err)
	}
	if engineHas(t, engine, dataKey([]byte("b"), 20)) || engineHas(t, engine, writeKey([]byte("b"), 20)) {
		t.Fatal("CommitSingle wrote data despite ErrLocked")
	}
}

func TestCommitSingleErrWriteConflict(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	putValue(t, store, "c", "v1", 10, 30)
	err := store.CommitSingle([]Mutation{{Key: []byte("c"), Value: []byte("v2")}}, []byte("c"), 20, 40)
	requireErrIs(t, err, ErrWriteConflict)
	requireValue(t, store, "c", 30, "v1")
	requireValue(t, store, "c", 40, "v1")
	if engineHas(t, engine, lockKey([]byte("c"))) {
		t.Fatal("lock created by failed CommitSingle")
	}
	if engineHas(t, engine, dataKey([]byte("c"), 40)) || engineHas(t, engine, writeKey([]byte("c"), 40)) {
		t.Fatal("CommitSingle wrote data despite ErrWriteConflict")
	}

	// A committed version at exactly commitTS owned by another transaction is
	// also a conflict.
	putValue(t, store, "d", "old", 10, 40)
	err = store.CommitSingle([]Mutation{{Key: []byte("d"), Value: []byte("new")}}, []byte("d"), 20, 40)
	requireErrIs(t, err, ErrWriteConflict)
	requireValue(t, store, "d", 40, "old")
}

func TestCommitSingleUpgradesExistingPrewrite(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	mutations := []Mutation{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
	}
	if err := store.Prewrite(mutations, []byte("a"), 10); err != nil {
		t.Fatalf("Prewrite: %v", err)
	}
	if !engineHas(t, engine, lockKey([]byte("a"))) || !engineHas(t, engine, dataKey([]byte("a"), 10)) {
		t.Fatal("prewrite did not create expected lock/provisional data")
	}

	if err := store.CommitSingle(mutations, []byte("a"), 10, 20); err != nil {
		t.Fatalf("CommitSingle over prewrite: %v", err)
	}
	requireKeyNotFound(t, store, "a", 19)
	requireValue(t, store, "a", 20, "1")
	requireValue(t, store, "b", 20, "2")
	for _, key := range []string{"a", "b"} {
		if engineHas(t, engine, lockKey([]byte(key))) {
			t.Fatalf("lock remains for %q", key)
		}
		if engineHas(t, engine, dataKey([]byte(key), 10)) {
			t.Fatalf("provisional data remains for %q", key)
		}
	}

	// A lock with the same startTS but a different primary belongs to an
	// inconsistent transaction and must be rejected without removing it.
	if err := store.Prewrite([]Mutation{{Key: []byte("c"), Value: []byte("x")}}, []byte("c"), 30); err != nil {
		t.Fatalf("Prewrite c: %v", err)
	}
	err := store.CommitSingle([]Mutation{{Key: []byte("c"), Value: []byte("x")}}, []byte("other"), 30, 40)
	if err == nil {
		t.Fatal("CommitSingle with a different primary succeeded")
	}
	if !engineHas(t, engine, lockKey([]byte("c"))) {
		t.Fatal("rejected CommitSingle removed the lock")
	}
	if engineHas(t, engine, writeKey([]byte("c"), 40)) {
		t.Fatal("rejected CommitSingle wrote a commit record")
	}

	// A rolled back transaction must not commit through 1PC.
	if err := store.Prewrite([]Mutation{{Key: []byte("e"), Value: []byte("x")}}, []byte("e"), 50); err != nil {
		t.Fatalf("Prewrite e: %v", err)
	}
	if err := store.Rollback([][]byte{[]byte("e")}, 50); err != nil {
		t.Fatalf("Rollback e: %v", err)
	}
	err = store.CommitSingle([]Mutation{{Key: []byte("e"), Value: []byte("x")}}, []byte("e"), 50, 60)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("CommitSingle after rollback = %v, want rolled-back error", err)
	}
	requireKeyNotFound(t, store, "e", 60)
}

func TestCommitSingleDelete(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	putValue(t, store, "d", "v1", 10, 20)
	if err := store.CommitSingle([]Mutation{{Key: []byte("d"), Delete: true}}, []byte("d"), 30, 40); err != nil {
		t.Fatalf("CommitSingle delete: %v", err)
	}

	requireValue(t, store, "d", 39, "v1")
	requireKeyNotFound(t, store, "d", 40)
	requireKeyNotFound(t, store, "d", 1<<40)
	if engineHas(t, engine, lockKey([]byte("d"))) {
		t.Fatal("lock remains after delete")
	}
	if engineHas(t, engine, dataKey([]byte("d"), 40)) {
		t.Fatal("delete wrote a data version")
	}
	rec, found, err := store.writeAt([]byte("d"), 40)
	if err != nil {
		t.Fatalf("writeAt: %v", err)
	}
	if !found || rec.kind != kindDelete || rec.startTS != 30 {
		t.Fatalf("write record = %+v, found=%v; want delete/startTS=30", rec, found)
	}
}

func TestCommitSingleGCSafe(t *testing.T) {
	store := newTestStore(t)

	if err := store.CommitSingle([]Mutation{{Key: []byte("a"), Value: []byte("v")}}, []byte("a"), 10, 20); err != nil {
		t.Fatalf("CommitSingle put: %v", err)
	}
	if err := store.GC(30); err != nil {
		t.Fatalf("GC after put: %v", err)
	}
	requireValue(t, store, "a", 30, "v")

	if err := store.CommitSingle([]Mutation{{Key: []byte("a"), Delete: true}}, []byte("a"), 40, 50); err != nil {
		t.Fatalf("CommitSingle delete: %v", err)
	}
	if err := store.GC(60); err != nil {
		t.Fatalf("GC after delete: %v", err)
	}
	requireKeyNotFound(t, store, "a", 60)
}

func TestCommitSingleValidationLeavesNoState(t *testing.T) {
	cases := []struct {
		name      string
		mutations []Mutation
		startTS   uint64
		commitTS  uint64
	}{
		{
			name:      "empty mutations",
			mutations: nil,
			startTS:   10,
			commitTS:  20,
		},
		{
			name:      "empty key",
			mutations: []Mutation{{Key: []byte("a"), Value: []byte("v")}, {Key: nil, Value: []byte("x")}},
			startTS:   10,
			commitTS:  20,
		},
		{
			name:      "duplicate key",
			mutations: []Mutation{{Key: []byte("a"), Value: []byte("v")}, {Key: []byte("a"), Value: []byte("w")}},
			startTS:   10,
			commitTS:  20,
		},
		{
			name:      "commit equal to start",
			mutations: []Mutation{{Key: []byte("a"), Value: []byte("v")}},
			startTS:   20,
			commitTS:  20,
		},
		{
			name:      "commit before start",
			mutations: []Mutation{{Key: []byte("a"), Value: []byte("v")}},
			startTS:   20,
			commitTS:  10,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, engine := newTestStoreWithEngine(t)
			if err := store.CommitSingle(tc.mutations, nil, tc.startTS, tc.commitTS); err == nil {
				t.Fatal("CommitSingle succeeded, want validation error")
			}
			for _, key := range []string{"a", ""} {
				if engineHas(t, engine, lockKey([]byte(key))) {
					t.Fatalf("lock %q created by failed validation", key)
				}
				if engineHas(t, engine, dataKey([]byte(key), tc.commitTS)) {
					t.Fatalf("data %q created by failed validation", key)
				}
				if engineHas(t, engine, writeKey([]byte(key), tc.commitTS)) {
					t.Fatalf("write record %q created by failed validation", key)
				}
				if engineHas(t, engine, dataKey([]byte(key), tc.startTS)) {
					t.Fatalf("provisional data %q created by failed validation", key)
				}
			}
		})
	}
}

func TestCommitSingleEngineFailureIsAtomic(t *testing.T) {
	engine := newFakeEngine()
	store := New(engine)
	engine.failNextBatchCommit = errors.New("injected engine failure")

	mutations := []Mutation{{Key: []byte("a"), Value: []byte("v")}}
	err := store.CommitSingle(mutations, []byte("a"), 10, 20)
	if err == nil {
		t.Fatal("CommitSingle succeeded despite injected engine failure")
	}

	for _, key := range [][]byte{
		lockKey([]byte("a")),
		dataKey([]byte("a"), 20),
		writeKey([]byte("a"), 20),
		dataKey([]byte("a"), 10),
		writeKey([]byte("a"), 10),
	} {
		if engineHas(t, engine, key) {
			t.Fatalf("engine retains key %q after failed CommitSingle", key)
		}
	}

	// The failed batch must have been closed, leaving the store usable.
	if err := store.CommitSingle(mutations, []byte("a"), 10, 20); err != nil {
		t.Fatalf("CommitSingle after failure: %v", err)
	}
	requireValue(t, store, "a", 20, "v")
}

func TestCommitSingleMultiKey(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)
	putValue(t, store, "d", "old", 10, 15)

	mutations := []Mutation{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
		{Key: []byte("c"), Delete: true},
		{Key: []byte("d"), Delete: true},
	}
	if err := store.CommitSingle(mutations, []byte("a"), 20, 30); err != nil {
		t.Fatalf("CommitSingle multi-key: %v", err)
	}

	requireKeyNotFound(t, store, "a", 19)
	requireValue(t, store, "a", 30, "1")
	requireValue(t, store, "b", 30, "2")
	requireKeyNotFound(t, store, "c", 30)
	requireKeyNotFound(t, store, "d", 30)
	if value, err := store.Get([]byte("d"), 15); err != nil || string(value) != "old" {
		t.Fatalf("Get d at 15 = %q, %v; want old version", value, err)
	}

	for _, key := range []string{"a", "b", "c", "d"} {
		if engineHas(t, engine, lockKey([]byte(key))) {
			t.Fatalf("lock remains for %q", key)
		}
		if !engineHas(t, engine, writeKey([]byte(key), 30)) {
			t.Fatalf("missing write record for %q", key)
		}
	}
	if !engineHas(t, engine, dataKey([]byte("a"), 30)) || !engineHas(t, engine, dataKey([]byte("b"), 30)) {
		t.Fatal("missing put data version")
	}
	if engineHas(t, engine, dataKey([]byte("c"), 30)) || engineHas(t, engine, dataKey([]byte("d"), 30)) {
		t.Fatal("delete wrote a data version")
	}
}

// countingEngine counts batch lifecycle calls so tests can assert that
// CommitSingle uses exactly one engine batch.
type countingEngine struct {
	storage.Engine
	batches int
	commits int
	closes  int
}

func (e *countingEngine) NewBatch() storage.Batch {
	e.batches++
	return &countingBatch{Batch: e.Engine.NewBatch(), engine: e}
}

type countingBatch struct {
	storage.Batch
	engine *countingEngine
}

func (b *countingBatch) Commit() error {
	b.engine.commits++
	return b.Batch.Commit()
}

func (b *countingBatch) Close() error {
	b.engine.closes++
	return b.Batch.Close()
}

func TestCommitSingleUsesOneBatch(t *testing.T) {
	counted := &countingEngine{Engine: newFakeEngine()}
	store := New(counted)

	mutations := []Mutation{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
		{Key: []byte("c"), Delete: true},
	}
	if err := store.CommitSingle(mutations, []byte("a"), 10, 20); err != nil {
		t.Fatalf("CommitSingle: %v", err)
	}
	if counted.batches != 1 || counted.commits != 1 || counted.closes != 1 {
		t.Fatalf("batch lifecycle = %d batches, %d commits, %d closes; want 1/1/1",
			counted.batches, counted.commits, counted.closes)
	}

	// An idempotent retry needs no batch at all.
	if err := store.CommitSingle(mutations, []byte("a"), 10, 20); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if counted.batches != 1 || counted.commits != 1 || counted.closes != 1 {
		t.Fatalf("idempotent retry created a batch: %d/%d/%d",
			counted.batches, counted.commits, counted.closes)
	}

	// Validation failures happen before any batch is opened.
	if err := store.CommitSingle([]Mutation{{Key: []byte("d")}, {Key: []byte("d")}}, nil, 30, 40); err == nil {
		t.Fatal("duplicate-key CommitSingle succeeded")
	}
	if counted.batches != 1 || counted.commits != 1 || counted.closes != 1 {
		t.Fatalf("validation failure created a batch: %d/%d/%d",
			counted.batches, counted.commits, counted.closes)
	}
}

func TestCommitSingleConcurrentTransactions(t *testing.T) {
	store := newTestStore(t)
	const n = 16

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("single-%02d", i)
			value := fmt.Sprintf("value-%02d", i)
			startTS := uint64(10 + i*10)
			err := store.CommitSingle(
				[]Mutation{{Key: []byte(key), Value: []byte(value)}},
				[]byte(key), startTS, startTS+5)
			if err != nil {
				errs <- fmt.Errorf("commit single %s: %w", key, err)
				return
			}
			errs <- nil
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < n; i++ {
		key := fmt.Sprintf("single-%02d", i)
		value := fmt.Sprintf("value-%02d", i)
		requireValue(t, store, key, 1000, value)
	}
}

func TestCommitSinglesPerCommandOutcomes(t *testing.T) {
	store, _ := newTestStoreWithEngine(t)
	if err := store.CommitSingle([]Mutation{{Key: []byte("a"), Value: []byte("old")}}, nil, 10, 20); err != nil {
		t.Fatalf("seed: %v", err)
	}

	errs := store.CommitSingles([]SingleCommand{
		{Mutations: []Mutation{{Key: []byte("b"), Value: []byte("vb")}}, StartTS: 30, CommitTS: 40},
		{Mutations: []Mutation{{Key: []byte("a"), Value: []byte("stale")}}, StartTS: 15, CommitTS: 25},
		{Mutations: []Mutation{{Key: []byte("c"), Value: []byte("vc")}}, StartTS: 30, CommitTS: 40},
	})
	if len(errs) != 3 {
		t.Fatalf("errs len = %d", len(errs))
	}
	if errs[0] != nil || errs[2] != nil {
		t.Fatalf("valid commands errors = %v, %v", errs[0], errs[2])
	}
	if !errors.Is(errs[1], ErrWriteConflict) {
		t.Fatalf("stale command error = %v, want ErrWriteConflict", errs[1])
	}
	requireValue(t, store, "b", 40, "vb")
	requireValue(t, store, "c", 40, "vc")
	requireValue(t, store, "a", 20, "old")
}
