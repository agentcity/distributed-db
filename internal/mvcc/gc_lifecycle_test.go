package mvcc

import "testing"

// TestOldestLockStartTS checks the conservative GC bound used by the storage
// lifecycle: it reports the minimum lock startTS and stops doing so once the
// locks are resolved.
func TestOldestLockStartTS(t *testing.T) {
	store, _ := newTestStoreWithEngine(t)

	if _, ok, err := store.OldestLockStartTS(); err != nil {
		t.Fatalf("OldestLockStartTS on empty store: %v", err)
	} else if ok {
		t.Fatal("OldestLockStartTS reported a lock on an empty store")
	}

	prewriteForGC(t, store, "a", 100)
	prewriteForGC(t, store, "b", 50)
	prewriteForGC(t, store, "c", 200)

	oldest, ok, err := store.OldestLockStartTS()
	if err != nil {
		t.Fatalf("OldestLockStartTS: %v", err)
	}
	if !ok || oldest != 50 {
		t.Fatalf("OldestLockStartTS = (%d, %v), want (50, true)", oldest, ok)
	}

	if err := store.ResolveLock([][]byte{[]byte("b")}, 50, 0); err != nil {
		t.Fatalf("resolve b: %v", err)
	}
	oldest, ok, err = store.OldestLockStartTS()
	if err != nil || !ok || oldest != 100 {
		t.Fatalf("after resolving b: (%d, %v, %v), want (100, true, nil)", oldest, ok, err)
	}

	if err := store.ResolveLock([][]byte{[]byte("a")}, 100, 0); err != nil {
		t.Fatalf("resolve a: %v", err)
	}
	if err := store.ResolveLock([][]byte{[]byte("c")}, 200, 0); err != nil {
		t.Fatalf("resolve c: %v", err)
	}
	if _, ok, err := store.OldestLockStartTS(); err != nil {
		t.Fatalf("after resolving all: %v", err)
	} else if ok {
		t.Fatal("OldestLockStartTS still reports a lock after all were resolved")
	}
}
