package mvcc

import "testing"

// prewriteForGC creates a lock plus provisional data for key at startTS.
func prewriteForGC(t *testing.T, store *Store, key string, startTS uint64) {
	t.Helper()
	mutation := Mutation{Key: []byte(key), Value: []byte("v-" + key)}
	if err := store.Prewrite([]Mutation{mutation}, []byte(key), startTS); err != nil {
		t.Fatalf("Prewrite(%q, startTS=%d): %v", key, startTS, err)
	}
}

// TestGCSafeKeepsLocksAndCollectsVersions exercises the safe-point rules on a
// real Pebble engine: old versions are collected, the newest base version and
// every version at or above the safe point survive, and lock records are never
// touched regardless of their startTS.
func TestGCSafeKeepsLocksAndCollectsVersions(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	putValue(t, store, "a", "v1", 10, 20)
	putValue(t, store, "a", "v2", 30, 40)
	putValue(t, store, "a", "v3", 50, 60)
	prewriteForGC(t, store, "b", 70)  // below a later safe point
	prewriteForGC(t, store, "c", 100) // always above the safe points used here

	stats, err := store.GCWithStats(45)
	if err != nil {
		t.Fatalf("GCWithStats(45): %v", err)
	}
	want := GCStats{DataVersions: 1, WriteRecords: 1, LocksRetained: 2}
	if stats != want {
		t.Fatalf("GCWithStats(45) = %+v, want %+v", stats, want)
	}

	if engineHas(t, engine, dataKey([]byte("a"), 20)) {
		t.Fatal("data version 20 was not garbage collected")
	}
	if engineHas(t, engine, writeKey([]byte("a"), 20)) {
		t.Fatal("write version 20 was not garbage collected")
	}
	for _, ts := range []uint64{40, 60} {
		if !engineHas(t, engine, dataKey([]byte("a"), ts)) || !engineHas(t, engine, writeKey([]byte("a"), ts)) {
			t.Fatalf("GC removed data/write version %d at or above the base/safe point", ts)
		}
	}
	if !engineHas(t, engine, dataKey([]byte("b"), 70)) {
		t.Fatal("GC removed provisional data at or above the safe point")
	}
	if !engineHas(t, engine, lockKey([]byte("b"))) || !engineHas(t, engine, lockKey([]byte("c"))) {
		t.Fatal("GC deleted a lock record; locks must be left for the lock resolver")
	}
	requireValue(t, store, "a", 45, "v2")
	requireValue(t, store, "a", 60, "v3")
	// Locks above the snapshot are invisible to reads.
	requireKeyNotFound(t, store, "b", 45)
	requireKeyNotFound(t, store, "c", 45)

	// Advancing the safe point past b's startTS must collect b's orphan
	// provisional data but keep its lock, because GC may not decide that b's
	// transaction is abandoned.
	stats, err = store.GCWithStats(75)
	if err != nil {
		t.Fatalf("GCWithStats(75): %v", err)
	}
	want = GCStats{DataVersions: 2, WriteRecords: 1, LocksRetained: 2}
	if stats != want {
		t.Fatalf("GCWithStats(75) = %+v, want %+v", stats, want)
	}
	if engineHas(t, engine, dataKey([]byte("a"), 40)) || engineHas(t, engine, writeKey([]byte("a"), 40)) {
		t.Fatal("GC(75) kept the superseded version 40")
	}
	if !engineHas(t, engine, dataKey([]byte("a"), 60)) || !engineHas(t, engine, writeKey([]byte("a"), 60)) {
		t.Fatal("GC(75) removed the newest base version 60")
	}
	if engineHas(t, engine, dataKey([]byte("b"), 70)) {
		t.Fatal("GC did not collect orphan provisional data of an uncommitted transaction")
	}
	if !engineHas(t, engine, lockKey([]byte("b"))) || !engineHas(t, engine, lockKey([]byte("c"))) {
		t.Fatal("GC(75) deleted a lock record; locks must be left for the lock resolver")
	}
	if !engineHas(t, engine, dataKey([]byte("c"), 100)) {
		t.Fatal("GC(75) removed data at or above the safe point")
	}
	requireValue(t, store, "a", 75, "v3")
	requireErrIs(t, mustGetErr(store, "b", 75), ErrLocked)
	requireKeyNotFound(t, store, "c", 75)

	// A repeated pass is a no-op: everything collectible is already gone.
	stats, err = store.GCWithStats(75)
	if err != nil {
		t.Fatalf("idempotent GCWithStats(75): %v", err)
	}
	want = GCStats{LocksRetained: 2}
	if stats != want {
		t.Fatalf("idempotent GCWithStats(75) = %+v, want %+v", stats, want)
	}

	// The retained lock is still resolvable through the transaction path.
	if err := store.ResolveLock([][]byte{[]byte("b")}, 70, 0); err != nil {
		t.Fatalf("ResolveLock(b, rollback): %v", err)
	}
	requireKeyNotFound(t, store, "b", 75)
}

// TestGCSafeKeepsLocksBelowSafePoint checks that a lock whose startTS is below
// the safe point is retained and still blocks reads until it is resolved. The
// GC wrapper must not delete it either.
func TestGCSafeKeepsLocksBelowSafePoint(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)
	prewriteForGC(t, store, "lk", 10)
	prewriteForGC(t, store, "hi", 100)

	// safePoint == lock startTS: retained.
	if err := store.GC(10); err != nil {
		t.Fatalf("GC(10): %v", err)
	}
	// safePoint well past the lock startTS: still retained.
	stats, err := store.GCWithStats(50)
	if err != nil {
		t.Fatalf("GCWithStats(50): %v", err)
	}
	// lk's provisional data is below the safe point and has no committed
	// write, so it is collected; the lock itself is not.
	if want := (GCStats{DataVersions: 1, LocksRetained: 2}); stats != want {
		t.Fatalf("GCWithStats(50) = %+v, want %+v", stats, want)
	}
	if !engineHas(t, engine, lockKey([]byte("lk"))) || !engineHas(t, engine, lockKey([]byte("hi"))) {
		t.Fatal("GC deleted a lock below the safe point")
	}
	requireErrIs(t, mustGetErr(store, "lk", 10), ErrLocked)
	requireErrIs(t, mustGetErr(store, "lk", 50), ErrLocked)
	// The lock is not visible to a snapshot older than its startTS.
	requireKeyNotFound(t, store, "lk", 9)
	// A lock above the safe point is likewise invisible below its startTS.
	requireKeyNotFound(t, store, "hi", 50)

	// Resolution, not GC, removes the lock.
	if err := store.ResolveLock([][]byte{[]byte("lk")}, 10, 0); err != nil {
		t.Fatalf("ResolveLock(lk, rollback): %v", err)
	}
	if engineHas(t, engine, lockKey([]byte("lk"))) {
		t.Fatal("ResolveLock left the rolled-back lock behind")
	}
	requireKeyNotFound(t, store, "lk", 50)
}

// TestGCSafeCollectsRollbackRecords checks that rollback records below the safe
// point are collected while those at or above it are retained.
func TestGCSafeCollectsRollbackRecords(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	putValue(t, store, "a", "v1", 10, 20)
	prewriteForGC(t, store, "a", 30)
	if err := store.Rollback([][]byte{[]byte("a")}, 30); err != nil {
		t.Fatalf("Rollback(a, 30): %v", err)
	}
	prewriteForGC(t, store, "b", 100)
	if err := store.Rollback([][]byte{[]byte("b")}, 100); err != nil {
		t.Fatalf("Rollback(b, 100): %v", err)
	}

	stats, err := store.GCWithStats(50)
	if err != nil {
		t.Fatalf("GCWithStats(50): %v", err)
	}
	if want := (GCStats{RollbackRecords: 1}); stats != want {
		t.Fatalf("GCWithStats(50) = %+v, want %+v", stats, want)
	}
	if engineHas(t, engine, writeKey([]byte("a"), 30)) {
		t.Fatal("old rollback record at 30 was not collected")
	}
	if !engineHas(t, engine, writeKey([]byte("b"), 100)) {
		t.Fatal("GC collected a rollback record at or above the safe point")
	}
	if !engineHas(t, engine, dataKey([]byte("a"), 20)) || !engineHas(t, engine, writeKey([]byte("a"), 20)) {
		t.Fatal("GC removed the committed base version while collecting a rollback")
	}
	requireValue(t, store, "a", 50, "v1")

	stats, err = store.GCWithStats(101)
	if err != nil {
		t.Fatalf("GCWithStats(101): %v", err)
	}
	if want := (GCStats{RollbackRecords: 1}); stats != want {
		t.Fatalf("GCWithStats(101) = %+v, want %+v", stats, want)
	}
	if engineHas(t, engine, writeKey([]byte("b"), 100)) {
		t.Fatal("rollback record at 100 was not collected once it fell below the safe point")
	}
}

// TestGCSafeZeroSafePointIsNoOp checks that safePoint 0 does not wipe the
// store and reports zero work.
func TestGCSafeZeroSafePointIsNoOp(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)
	putValue(t, store, "a", "v1", 10, 20)
	prewriteForGC(t, store, "b", 30)

	stats, err := store.GCWithStats(0)
	if err != nil {
		t.Fatalf("GCWithStats(0): %v", err)
	}
	if stats != (GCStats{}) {
		t.Fatalf("GCWithStats(0) = %+v, want zero stats", stats)
	}
	if err := store.GC(0); err != nil {
		t.Fatalf("GC(0): %v", err)
	}
	for _, key := range [][]byte{
		dataKey([]byte("a"), 20), writeKey([]byte("a"), 20),
		dataKey([]byte("b"), 30), lockKey([]byte("b")),
	} {
		if !engineHas(t, engine, key) {
			t.Fatalf("GC(0) deleted %q; safePoint 0 must be a no-op", key)
		}
	}
	requireValue(t, store, "a", 20, "v1")
	requireErrIs(t, mustGetErr(store, "b", 30), ErrLocked)
}

// TestGCSafeStatsCounts verifies that every category of collected record is
// counted exactly once and that locks are only counted, never deleted.
func TestGCSafeStatsCounts(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)

	putValue(t, store, "a", "v1", 10, 20)
	putValue(t, store, "a", "v2", 30, 40)
	deleteValue(t, store, "a", 50, 60) // base tombstone; hides both puts
	putValue(t, store, "b", "v1", 70, 80)
	prewriteForGC(t, store, "c", 90) // lock + provisional data, never committed
	prewriteForGC(t, store, "d", 100)
	if err := store.Rollback([][]byte{[]byte("d")}, 100); err != nil {
		t.Fatalf("Rollback(d, 100): %v", err)
	}

	stats, err := store.GCWithStats(85)
	if err != nil {
		t.Fatalf("GCWithStats(85): %v", err)
	}
	want := GCStats{DataVersions: 2, WriteRecords: 2, LocksRetained: 1}
	if stats != want {
		t.Fatalf("GCWithStats(85) = %+v, want %+v", stats, want)
	}
	if got := stats.Total(); got != 4 {
		t.Fatalf("stats.Total() = %d, want 4", got)
	}

	for _, ts := range []uint64{20, 40} {
		if engineHas(t, engine, dataKey([]byte("a"), ts)) || engineHas(t, engine, writeKey([]byte("a"), ts)) {
			t.Fatalf("GC(85) kept a superseded version of a at %d", ts)
		}
	}
	if !engineHas(t, engine, writeKey([]byte("a"), 60)) {
		t.Fatal("GC(85) removed the base delete tombstone")
	}
	if !engineHas(t, engine, dataKey([]byte("b"), 80)) || !engineHas(t, engine, writeKey([]byte("b"), 80)) {
		t.Fatal("GC(85) removed the base version of b")
	}
	if !engineHas(t, engine, dataKey([]byte("c"), 90)) || !engineHas(t, engine, lockKey([]byte("c"))) {
		t.Fatal("GC removed data or lock at or above the safe point")
	}
	if !engineHas(t, engine, writeKey([]byte("d"), 100)) {
		t.Fatal("GC removed a rollback record at or above the safe point")
	}
	requireKeyNotFound(t, store, "a", 85)
	requireKeyNotFound(t, store, "c", 85)

	// The next pass collects c's orphan provisional data and d's rollback
	// record, but still retains c's lock.
	stats, err = store.GCWithStats(101)
	if err != nil {
		t.Fatalf("GCWithStats(101): %v", err)
	}
	want = GCStats{DataVersions: 1, RollbackRecords: 1, LocksRetained: 1}
	if stats != want {
		t.Fatalf("GCWithStats(101) = %+v, want %+v", stats, want)
	}
	if engineHas(t, engine, dataKey([]byte("c"), 90)) {
		t.Fatal("GC did not collect orphan provisional data of c")
	}
	if engineHas(t, engine, writeKey([]byte("d"), 100)) {
		t.Fatal("GC did not collect the old rollback record of d")
	}
	if !engineHas(t, engine, lockKey([]byte("c"))) {
		t.Fatal("GC deleted c's lock")
	}
	requireErrIs(t, mustGetErr(store, "c", 101), ErrLocked)

	if err := store.ResolveLock([][]byte{[]byte("c")}, 90, 0); err != nil {
		t.Fatalf("ResolveLock(c, rollback): %v", err)
	}
	requireKeyNotFound(t, store, "c", 101)
}
