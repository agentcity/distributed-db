package mvcc

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestSnapshotIsolation(t *testing.T) {
	store := newTestStore(t)

	putValue(t, store, "a", "v1", 10, 20)
	requireKeyNotFound(t, store, "a", 19)
	requireValue(t, store, "a", 20, "v1")
	requireValue(t, store, "a", 1<<40, "v1")

	putValue(t, store, "a", "v2", 30, 40)
	requireValue(t, store, "a", 25, "v1")
	requireValue(t, store, "a", 39, "v1")
	requireValue(t, store, "a", 40, "v2")
	requireValue(t, store, "a", 100, "v2")

	putValue(t, store, "b", "bv", 50, 60)
	requireValue(t, store, "a", 100, "v2")
	requireValue(t, store, "b", 60, "bv")
	requireKeyNotFound(t, store, "b", 59)
}

// TestReadBoundaryTimestampReuse checks the conflict boundary: a commit at
// exactly startTS is not a conflict, and the new transaction must not clobber
// the committed data version that occupies the same timestamp.
func TestReadBoundaryTimestampReuse(t *testing.T) {
	store := newTestStore(t)

	putValue(t, store, "a", "v1", 10, 20)
	if err := store.Prewrite([]Mutation{{Key: []byte("a"), Value: []byte("v2")}}, []byte("a"), 20); err != nil {
		t.Fatalf("Prewrite at already-used commitTS: %v", err)
	}
	requireKeyNotFound(t, store, "a", 19)
	requireErrIs(t, mustGetErr(store, "a", 20), ErrLocked)

	kvs, err := store.Scan(nil, nil, 20, 0)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(kvs) != 1 || string(kvs[0].Key) != "a" || string(kvs[0].Value) != "v1" {
		t.Fatalf("Scan at 20 = %+v, want a=v1", kvs)
	}

	if err := store.Commit([][]byte{[]byte("a")}, 20, 30); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	requireValue(t, store, "a", 25, "v1")
	requireValue(t, store, "a", 30, "v2")
}

func mustGetErr(store *Store, key string, ts uint64) error {
	_, err := store.Get([]byte(key), ts)
	return err
}

func TestPrewriteCommit(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	mutations := []Mutation{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
	}
	if err := store.Prewrite(mutations, []byte("a"), 10); err != nil {
		t.Fatalf("Prewrite: %v", err)
	}
	requireErrIs(t, mustGetErr(store, "a", 10), ErrLocked)
	requireErrIs(t, mustGetErr(store, "b", 10), ErrLocked)
	requireErrIs(t, mustGetErr(store, "a", 9), ErrKeyNotFound)

	if err := store.Commit([][]byte{[]byte("a"), []byte("b")}, 10, 20); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	requireValue(t, store, "a", 20, "1")
	requireValue(t, store, "b", 20, "2")
	if engineHas(t, engine, lockKey([]byte("a"))) || engineHas(t, engine, lockKey([]byte("b"))) {
		t.Fatal("locks were not removed by Commit")
	}
	if !engineHas(t, engine, writeKey([]byte("a"), 20)) {
		t.Fatal("missing write record at commitTS")
	}

	// Idempotent at the same commitTS.
	if err := store.Commit([][]byte{[]byte("a"), []byte("b")}, 10, 20); err != nil {
		t.Fatalf("idempotent Commit: %v", err)
	}
	// A different commitTS for an already committed transaction is an error.
	requireErrIs(t, store.Commit([][]byte{[]byte("a")}, 10, 30), ErrAlreadyCommitted)
}

func TestRollback(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	if err := store.Prewrite([]Mutation{{Key: []byte("a"), Value: []byte("v")}}, []byte("a"), 10); err != nil {
		t.Fatalf("Prewrite: %v", err)
	}
	if err := store.Rollback([][]byte{[]byte("a")}, 10); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	requireKeyNotFound(t, store, "a", 20)
	if engineHas(t, engine, lockKey([]byte("a"))) {
		t.Fatal("lock was not removed by Rollback")
	}
	if !engineHas(t, engine, writeKey([]byte("a"), 10)) {
		t.Fatal("missing rollback write record")
	}
	if engineHas(t, engine, dataKey([]byte("a"), 10)) {
		t.Fatal("provisional data was not removed by Rollback")
	}
	if err := store.Rollback([][]byte{[]byte("a")}, 10); err != nil {
		t.Fatalf("idempotent Rollback: %v", err)
	}
	if err := store.Commit([][]byte{[]byte("a")}, 10, 20); err == nil {
		t.Fatal("Commit after Rollback succeeded")
	}

	putValue(t, store, "b", "bv", 10, 20)
	requireErrIs(t, store.Rollback([][]byte{[]byte("b")}, 10), ErrAlreadyCommitted)

	if err := store.Prewrite([]Mutation{{Key: []byte("c"), Value: []byte("cv")}}, []byte("c"), 30); err != nil {
		t.Fatalf("Prewrite c: %v", err)
	}
	requireErrIs(t, store.Rollback([][]byte{[]byte("c")}, 31), ErrLocked)

	// Rolling back a key that was never prewritten is a harmless no-op.
	if err := store.Rollback([][]byte{[]byte("d")}, 40); err != nil {
		t.Fatalf("Rollback never-prewritten: %v", err)
	}
	requireKeyNotFound(t, store, "d", 40)
}

func TestRollbackDoesNotHideOlderVersion(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "a", "v1", 10, 20)

	if err := store.Prewrite([]Mutation{{Key: []byte("a"), Delete: true}}, []byte("a"), 30); err != nil {
		t.Fatalf("Prewrite delete: %v", err)
	}
	if err := store.Rollback([][]byte{[]byte("a")}, 30); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	requireValue(t, store, "a", 30, "v1")
	requireKeys(t, scanKeys(t, store, nil, nil, 30, 0), []string{"a"})
}

func TestWriteConflict(t *testing.T) {
	store := newTestStore(t)

	putValue(t, store, "a", "v1", 10, 20)
	if err := store.Prewrite([]Mutation{{Key: []byte("a"), Value: []byte("v2")}}, []byte("a"), 15); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("Prewrite at startTS 15 = %v, want ErrWriteConflict", err)
	}

	// Committed at exactly startTS is not "after startTS".
	if err := store.Prewrite([]Mutation{{Key: []byte("a"), Value: []byte("v2")}}, []byte("a"), 20); err != nil {
		t.Fatalf("Prewrite at boundary: %v", err)
	}
	if err := store.Commit([][]byte{[]byte("a")}, 20, 30); err != nil {
		t.Fatalf("Commit boundary: %v", err)
	}
	requireValue(t, store, "a", 25, "v1")
	requireValue(t, store, "a", 30, "v2")

	// A rollback record does not block a retry with the same startTS.
	if err := store.Prewrite([]Mutation{{Key: []byte("b"), Value: []byte("bv")}}, []byte("b"), 40); err != nil {
		t.Fatalf("Prewrite b: %v", err)
	}
	if err := store.Rollback([][]byte{[]byte("b")}, 40); err != nil {
		t.Fatalf("Rollback b: %v", err)
	}
	if err := store.Prewrite([]Mutation{{Key: []byte("b"), Value: []byte("bv2")}}, []byte("b"), 40); err != nil {
		t.Fatalf("Prewrite b after rollback: %v", err)
	}
	if err := store.Commit([][]byte{[]byte("b")}, 40, 50); err != nil {
		t.Fatalf("Commit b: %v", err)
	}
	requireValue(t, store, "b", 50, "bv2")

	// A lock on another key never conflicts.
	if err := store.Prewrite([]Mutation{{Key: []byte("c"), Value: []byte("cv")}}, []byte("c"), 15); err != nil {
		t.Fatalf("Prewrite other key: %v", err)
	}
	if err := store.Rollback([][]byte{[]byte("c")}, 15); err != nil {
		t.Fatalf("Rollback c: %v", err)
	}
}

func TestLockConflict(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "a", "old", 1, 2)

	if err := store.Prewrite([]Mutation{{Key: []byte("a"), Value: []byte("new")}}, []byte("a"), 10); err != nil {
		t.Fatalf("Prewrite: %v", err)
	}
	requireErrIs(t, mustGetErr(store, "a", 10), ErrLocked)
	requireValue(t, store, "a", 9, "old")
	requireErrIs(t, store.Prewrite([]Mutation{{Key: []byte("a"), Value: []byte("other")}}, []byte("a"), 11), ErrLocked)
	requireErrIs(t, store.Commit([][]byte{[]byte("a")}, 11, 20), ErrLocked)
	requireErrIs(t, store.Rollback([][]byte{[]byte("a")}, 11), ErrLocked)

	// Re-prewriting the same transaction is allowed.
	if err := store.Prewrite([]Mutation{{Key: []byte("a"), Value: []byte("new")}}, []byte("a"), 10); err != nil {
		t.Fatalf("re-Prewrite: %v", err)
	}

	// Scan ignores locks and returns the previous committed version.
	kvs, err := store.Scan(nil, nil, 10, 0)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(kvs) != 1 || string(kvs[0].Value) != "old" {
		t.Fatalf("Scan with lock = %+v, want old value", kvs)
	}

	if err := store.Commit([][]byte{[]byte("a")}, 10, 20); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	requireValue(t, store, "a", 20, "new")
}

func TestScan(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "c", "3", 10, 20)
	putValue(t, store, "a", "1", 10, 20)
	putValue(t, store, "b", "2", 10, 20)

	requireKeys(t, scanKeys(t, store, nil, nil, 20, 0), []string{"a", "b", "c"})
	requireKeys(t, scanKeys(t, store, nil, nil, 20, 2), []string{"a", "b"})
	requireKeys(t, scanKeys(t, store, []byte("a"), []byte("c"), 20, 0), []string{"a", "b"})
	requireKeys(t, scanKeys(t, store, []byte("b"), nil, 20, 0), []string{"b", "c"})
	requireKeys(t, scanKeys(t, store, nil, []byte("b"), 20, 0), []string{"a"})
	requireKeys(t, scanKeys(t, store, []byte("b"), []byte("b"), 20, 0), nil)
	requireKeys(t, scanKeys(t, store, []byte("b"), []byte("a"), 20, 0), nil)
	requireKeys(t, scanKeys(t, store, nil, nil, 0, 0), nil)

	deleteValue(t, store, "b", 30, 40)
	requireKeys(t, scanKeys(t, store, nil, nil, 39, 0), []string{"a", "b", "c"})
	requireKeys(t, scanKeys(t, store, nil, nil, 40, 0), []string{"a", "c"})
	requireKeys(t, scanKeys(t, store, nil, nil, 40, 1), []string{"a"})

	// An uncommitted prewrite is invisible to Scan.
	if err := store.Prewrite([]Mutation{{Key: []byte("d"), Value: []byte("4")}}, []byte("d"), 50); err != nil {
		t.Fatalf("Prewrite d: %v", err)
	}
	requireKeys(t, scanKeys(t, store, nil, nil, 50, 0), []string{"a", "c"})
	if err := store.Commit([][]byte{[]byte("d")}, 50, 60); err != nil {
		t.Fatalf("Commit d: %v", err)
	}
	requireKeys(t, scanKeys(t, store, nil, nil, 60, 0), []string{"a", "c", "d"})

	kvs, err := store.Scan(nil, nil, 60, 0)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []KV{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("c"), Value: []byte("3")},
		{Key: []byte("d"), Value: []byte("4")},
	}
	if len(kvs) != len(want) {
		t.Fatalf("Scan = %+v, want %+v", kvs, want)
	}
	for i := range want {
		if string(kvs[i].Key) != string(want[i].Key) || string(kvs[i].Value) != string(want[i].Value) {
			t.Fatalf("Scan[%d] = %+v, want %+v", i, kvs[i], want[i])
		}
	}
}

// TestScanPrefixKeys makes sure arbitrary user keys (including keys that are
// prefixes of each other) are decoded exactly, deduplicated and ordered.
func TestScanPrefixKeys(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "a", "va", 10, 20)
	putValue(t, store, "a\x00", "vnull", 11, 30)
	putValue(t, store, "a0", "v0", 12, 40)
	putValue(t, store, "b", "vb", 13, 50)

	requireKeys(t, scanKeys(t, store, nil, nil, 50, 0), []string{"a", "a\x00", "a0", "b"})
	requireValue(t, store, "a", 50, "va")
	requireValue(t, store, "a\x00", 50, "vnull")
	requireValue(t, store, "a0", 50, "v0")
	requireValue(t, store, "b", 50, "vb")
	requireKeys(t, scanKeys(t, store, []byte("a"), []byte("b"), 50, 0), []string{"a", "a\x00", "a0"})
	requireKeys(t, scanKeys(t, store, []byte("a\x00"), []byte("a0"), 50, 0), []string{"a\x00"})
}

func TestDelete(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "a", "v1", 10, 20)
	putValue(t, store, "a", "v2", 30, 40)
	deleteValue(t, store, "a", 50, 60)

	requireValue(t, store, "a", 20, "v1")
	requireValue(t, store, "a", 40, "v2")
	requireValue(t, store, "a", 59, "v2")
	requireKeyNotFound(t, store, "a", 60)
	requireKeyNotFound(t, store, "a", 100)
	requireKeys(t, scanKeys(t, store, nil, nil, 60, 0), nil)

	putValue(t, store, "a", "v3", 70, 80)
	requireValue(t, store, "a", 80, "v3")
	// The delete at 60 hides v2 for every snapshot until v3 commits at 80.
	requireKeyNotFound(t, store, "a", 79)
}

func TestDeleteMissingKey(t *testing.T) {
	store := newTestStore(t)
	deleteValue(t, store, "missing", 10, 20)
	requireKeyNotFound(t, store, "missing", 20)
}

func TestEmptyValue(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "empty", "", 10, 20)
	got, err := store.Get([]byte("empty"), 20)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Get = %q, want empty value", got)
	}
}

func TestGetAndScanReturnCopies(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "a", "value", 10, 20)

	got, err := store.Get([]byte("a"), 20)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got[0] = 'X'
	requireValue(t, store, "a", 20, "value")

	kvs, err := store.Scan(nil, nil, 20, 0)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	kvs[0].Value[0] = 'Y'
	kvs[0].Key[0] = 'Z'
	requireValue(t, store, "a", 20, "value")
}

func TestLockAndWriteEncoding(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	if err := store.Prewrite([]Mutation{{Key: []byte("a"), Value: []byte("v")}}, []byte("primary"), 10); err != nil {
		t.Fatalf("Prewrite put: %v", err)
	}
	raw, err := engine.Get(lockKey([]byte("a")))
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	lock, err := decodeLock(raw)
	if err != nil {
		t.Fatalf("decode lock: %v", err)
	}
	if lock.startTS != 10 || lock.kind != kindPut || lock.ttlMillis != defaultLockTTLMillis {
		t.Fatalf("lock = %+v", lock)
	}
	if string(lock.primary) != "primary" || string(lock.value) != "v" {
		t.Fatalf("lock primary/value = %q/%q", lock.primary, lock.value)
	}
	if err := store.Rollback([][]byte{[]byte("a")}, 10); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if err := store.Prewrite([]Mutation{{Key: []byte("b"), Delete: true}}, []byte("b"), 11); err != nil {
		t.Fatalf("Prewrite delete: %v", err)
	}
	raw, err = engine.Get(lockKey([]byte("b")))
	if err != nil {
		t.Fatalf("read delete lock: %v", err)
	}
	lock, err = decodeLock(raw)
	if err != nil {
		t.Fatalf("decode delete lock: %v", err)
	}
	if lock.kind != kindDelete || len(lock.value) != 0 {
		t.Fatalf("delete lock = %+v", lock)
	}
	if err := store.Rollback([][]byte{[]byte("b")}, 11); err != nil {
		t.Fatalf("Rollback b: %v", err)
	}

	putValue(t, store, "c", "cv", 20, 30)
	raw, err = engine.Get(writeKey([]byte("c"), 30))
	if err != nil {
		t.Fatalf("read write record: %v", err)
	}
	rec, err := decodeWrite(raw)
	if err != nil {
		t.Fatalf("decode write record: %v", err)
	}
	if rec.kind != kindPut || rec.startTS != 20 {
		t.Fatalf("write record = %+v", rec)
	}
}

func TestInvalidInput(t *testing.T) {
	store := newTestStore(t)
	if err := store.Prewrite(nil, nil, 10); err == nil {
		t.Fatal("Prewrite(nil) succeeded")
	}
	if err := store.Prewrite([]Mutation{{Value: []byte("v")}}, nil, 10); err == nil {
		t.Fatal("Prewrite with empty key succeeded")
	}
	if err := store.Prewrite([]Mutation{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("a"), Value: []byte("2")},
	}, []byte("a"), 10); err == nil {
		t.Fatal("Prewrite with duplicate keys succeeded")
	}
	if err := store.Commit(nil, 10, 20); err == nil {
		t.Fatal("Commit(nil) succeeded")
	}
	if err := store.Commit([][]byte{[]byte("a")}, 20, 20); err == nil {
		t.Fatal("Commit with commitTS == startTS succeeded")
	}
	if err := store.Commit([][]byte{[]byte("a")}, 20, 10); err == nil {
		t.Fatal("Commit with commitTS < startTS succeeded")
	}
	if err := store.Rollback(nil, 10); err == nil {
		t.Fatal("Rollback(nil) succeeded")
	}
}

// TestPrewriteAtomicValidation verifies that a conflict on one mutation does
// not leave locks behind for the others.
func TestPrewriteAtomicValidation(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)
	putValue(t, store, "c", "cv", 10, 30)

	err := store.Prewrite([]Mutation{
		{Key: []byte("b"), Value: []byte("bv")},
		{Key: []byte("c"), Value: []byte("cv2")},
	}, []byte("b"), 20)
	requireErrIs(t, err, ErrWriteConflict)
	if engineHas(t, engine, lockKey([]byte("b"))) {
		t.Fatal("partial lock left after write conflict")
	}

	if err := store.Prewrite([]Mutation{{Key: []byte("c"), Value: []byte("cv3")}}, []byte("c"), 40); err != nil {
		t.Fatalf("Prewrite c: %v", err)
	}
	err = store.Prewrite([]Mutation{
		{Key: []byte("b"), Value: []byte("bv")},
		{Key: []byte("c"), Value: []byte("cv4")},
	}, []byte("b"), 50)
	requireErrIs(t, err, ErrLocked)
	if engineHas(t, engine, lockKey([]byte("b"))) {
		t.Fatal("partial lock left after lock conflict")
	}
}

func TestPrewriteBatchFailureCleanup(t *testing.T) {
	engine := newFakeEngine()
	// The fake is required here to inject an atomic batch failure.
	store := New(engine)

	engine.failNextBatchCommit = errors.New("injected prewrite failure")
	err := store.Prewrite([]Mutation{{Key: []byte("a"), Value: []byte("v")}}, []byte("a"), 10)
	if err == nil {
		t.Fatal("Prewrite succeeded despite injected batch failure")
	}
	if engineHas(t, engine, lockKey([]byte("a"))) {
		t.Fatal("lock left after failed prewrite")
	}
	if engineHas(t, engine, dataKey([]byte("a"), 10)) {
		t.Fatal("provisional data left after failed prewrite")
	}
	if !engineHas(t, engine, writeKey([]byte("a"), 10)) {
		t.Fatal("missing rollback record after failed prewrite")
	}
	requireKeyNotFound(t, store, "a", 10)

	// The rollback record must not prevent retrying with the same startTS.
	if err := store.Prewrite([]Mutation{{Key: []byte("a"), Value: []byte("v")}}, []byte("a"), 10); err != nil {
		t.Fatalf("retry Prewrite: %v", err)
	}
	if err := store.Commit([][]byte{[]byte("a")}, 10, 20); err != nil {
		t.Fatalf("retry Commit: %v", err)
	}
	requireValue(t, store, "a", 20, "v")
}

func TestCommitBatchFailureRetry(t *testing.T) {
	engine := newFakeEngine()
	// The fake is required here to inject an atomic batch failure.
	store := New(engine)

	if err := store.Prewrite([]Mutation{{Key: []byte("a"), Value: []byte("v")}}, []byte("a"), 10); err != nil {
		t.Fatalf("Prewrite: %v", err)
	}
	engine.failNextBatchCommit = errors.New("injected commit failure")
	if err := store.Commit([][]byte{[]byte("a")}, 10, 20); err == nil {
		t.Fatal("Commit succeeded despite injected batch failure")
	}
	requireErrIs(t, mustGetErr(store, "a", 20), ErrLocked)
	if err := store.Commit([][]byte{[]byte("a")}, 10, 20); err != nil {
		t.Fatalf("retry Commit: %v", err)
	}
	requireValue(t, store, "a", 20, "v")
}

func TestReopen(t *testing.T) {
	dir := t.TempDir()
	engine := openTestEngineAt(t, dir)
	store := New(engine)
	putValue(t, store, "a", "v1", 10, 20)
	if err := store.Prewrite([]Mutation{{Key: []byte("b"), Value: []byte("v2")}}, []byte("b"), 30); err != nil {
		t.Fatalf("Prewrite b: %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Close before reopen: %v", err)
	}

	reopenedEngine := openTestEngineAt(t, dir)
	reopened := New(reopenedEngine)
	requireValue(t, reopened, "a", 20, "v1")
	requireErrIs(t, mustGetErr(reopened, "b", 30), ErrLocked)
	if err := reopened.Commit([][]byte{[]byte("b")}, 30, 40); err != nil {
		t.Fatalf("Commit after reopen: %v", err)
	}
	requireValue(t, reopened, "b", 40, "v2")
	if err := reopenedEngine.Close(); err != nil {
		t.Fatalf("Close before second reopen: %v", err)
	}

	again := New(openTestEngineAt(t, dir))
	requireValue(t, again, "a", 20, "v1")
	requireValue(t, again, "b", 40, "v2")
}

func TestConcurrentTransactions(t *testing.T) {
	store := newTestStore(t)
	const n = 16

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%02d", i)
			value := fmt.Sprintf("value-%02d", i)
			startTS := uint64(10 + i*10)
			commitTS := startTS + 5
			if err := store.Prewrite([]Mutation{{Key: []byte(key), Value: []byte(value)}}, []byte(key), startTS); err != nil {
				errs <- fmt.Errorf("prewrite %s: %w", key, err)
				return
			}
			if err := store.Commit([][]byte{[]byte(key)}, startTS, commitTS); err != nil {
				errs <- fmt.Errorf("commit %s: %w", key, err)
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
		key := fmt.Sprintf("key-%02d", i)
		value := fmt.Sprintf("value-%02d", i)
		requireValue(t, store, key, 1000, value)
	}
}
