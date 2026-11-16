package mvcc

import "testing"

func TestGCVersions(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	putValue(t, store, "a", "v1", 10, 20)
	putValue(t, store, "a", "v2", 30, 40)
	putValue(t, store, "a", "v3", 50, 60)
	if err := store.Prewrite([]Mutation{{Key: []byte("b"), Value: []byte("bv")}}, []byte("b"), 70); err != nil {
		t.Fatalf("Prewrite b: %v", err)
	}

	if err := store.GC(45); err != nil {
		t.Fatalf("GC(45): %v", err)
	}
	if engineHas(t, engine, dataKey([]byte("a"), 20)) {
		t.Fatal("data version 20 was not garbage collected")
	}
	if engineHas(t, engine, writeKey([]byte("a"), 20)) {
		t.Fatal("write version 20 was not garbage collected")
	}
	if !engineHas(t, engine, dataKey([]byte("a"), 40)) || !engineHas(t, engine, dataKey([]byte("a"), 60)) {
		t.Fatal("GC removed a version at or above the base/safe point")
	}
	if !engineHas(t, engine, writeKey([]byte("a"), 40)) || !engineHas(t, engine, writeKey([]byte("a"), 60)) {
		t.Fatal("GC removed a write record at or above the base/safe point")
	}
	if !engineHas(t, engine, lockKey([]byte("b"))) {
		t.Fatal("GC removed a lock above the safe point")
	}
	requireValue(t, store, "a", 45, "v2")
	requireValue(t, store, "a", 60, "v3")

	if err := store.GC(45); err != nil {
		t.Fatalf("idempotent GC(45): %v", err)
	}

	if err := store.GC(65); err != nil {
		t.Fatalf("GC(65): %v", err)
	}
	if engineHas(t, engine, dataKey([]byte("a"), 40)) || engineHas(t, engine, writeKey([]byte("a"), 40)) {
		t.Fatal("GC(65) kept version 40")
	}
	requireValue(t, store, "a", 65, "v3")

	if err := store.GC(75); err != nil {
		t.Fatalf("GC(75): %v", err)
	}
	if !engineHas(t, engine, lockKey([]byte("b"))) {
		t.Fatal("GC removed a lock; locks must be resolved, not collected")
	}
	if engineHas(t, engine, dataKey([]byte("b"), 70)) {
		t.Fatal("GC did not remove orphan provisional data")
	}
	requireErrIs(t, mustGetErr(store, "b", 75), ErrLocked)
	// The lock is resolved through the transaction path, not deleted by GC.
	if err := store.Rollback([][]byte{[]byte("b")}, 70); err != nil {
		t.Fatalf("Rollback(b): %v", err)
	}
	requireKeyNotFound(t, store, "b", 75)
	requireValue(t, store, "a", 75, "v3")
}

func TestGCDeleteBase(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)
	putValue(t, store, "d", "v1", 10, 20)
	deleteValue(t, store, "d", 30, 40)

	if err := store.GC(50); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if engineHas(t, engine, dataKey([]byte("d"), 20)) {
		t.Fatal("data version hidden by a delete was not collected")
	}
	if !engineHas(t, engine, writeKey([]byte("d"), 40)) {
		t.Fatal("GC removed the base delete tombstone")
	}
	requireKeyNotFound(t, store, "d", 50)
	requireKeys(t, scanKeys(t, store, nil, nil, 50, 0), nil)
}

func TestGCRollbackDoesNotHideBase(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)
	putValue(t, store, "e", "v1", 10, 20)
	if err := store.Prewrite([]Mutation{{Key: []byte("e"), Value: []byte("v2")}}, []byte("e"), 30); err != nil {
		t.Fatalf("Prewrite e: %v", err)
	}
	if err := store.Rollback([][]byte{[]byte("e")}, 30); err != nil {
		t.Fatalf("Rollback e: %v", err)
	}

	if err := store.GC(40); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if engineHas(t, engine, writeKey([]byte("e"), 30)) {
		t.Fatal("old rollback record was not collected")
	}
	if !engineHas(t, engine, dataKey([]byte("e"), 20)) || !engineHas(t, engine, writeKey([]byte("e"), 20)) {
		t.Fatal("GC treated a rollback as the base version")
	}
	requireValue(t, store, "e", 40, "v1")
}

func TestGCSafePointBoundary(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)
	putValue(t, store, "g", "v1", 10, 20)
	if err := store.Prewrite([]Mutation{{Key: []byte("h"), Value: []byte("hv")}}, []byte("h"), 20); err != nil {
		t.Fatalf("Prewrite h: %v", err)
	}

	if err := store.GC(20); err != nil {
		t.Fatalf("GC(20): %v", err)
	}
	if !engineHas(t, engine, dataKey([]byte("g"), 20)) || !engineHas(t, engine, writeKey([]byte("g"), 20)) {
		t.Fatal("GC removed the version exactly at safePoint")
	}
	if !engineHas(t, engine, lockKey([]byte("h"))) {
		t.Fatal("GC removed a lock exactly at safePoint")
	}
	requireValue(t, store, "g", 20, "v1")
	requireErrIs(t, mustGetErr(store, "h", 20), ErrLocked)

	if err := store.GC(21); err != nil {
		t.Fatalf("GC(21): %v", err)
	}
	requireValue(t, store, "g", 21, "v1")
	if !engineHas(t, engine, lockKey([]byte("h"))) {
		t.Fatal("GC(21) deleted a lock below the safe point; locks are left for the resolver")
	}
	requireErrIs(t, mustGetErr(store, "h", 21), ErrLocked)
	// Resolution, not GC, removes the lock.
	if err := store.Rollback([][]byte{[]byte("h")}, 20); err != nil {
		t.Fatalf("Rollback(h): %v", err)
	}
	requireKeyNotFound(t, store, "h", 21)
}

func TestGCEmptyStore(t *testing.T) {
	if err := newTestStore(t).GC(100); err != nil {
		t.Fatalf("GC on empty store: %v", err)
	}
}
