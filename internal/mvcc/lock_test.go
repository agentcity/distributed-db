package mvcc

import (
	"errors"
	"testing"
	"time"
)

func TestCheckTxnStatusCommitted(t *testing.T) {
	store := newTestStore(t)
	start := uint64(time.Now().UnixMilli()) << 18
	commit := start + 1
	putValue(t, store, "k", "v", start, commit)

	st, err := store.CheckTxnStatus([]byte("k"), start, time.Now())
	if err != nil {
		t.Fatalf("CheckTxnStatus: %v", err)
	}
	if !st.Committed || st.CommitTS != commit {
		t.Fatalf("status = %+v, want committed at %d", st, commit)
	}
}

func TestCheckTxnStatusRolledBack(t *testing.T) {
	store := newTestStore(t)
	start := uint64(time.Now().UnixMilli()) << 18
	if err := store.Prewrite([]Mutation{{Key: []byte("k"), Value: []byte("v")}}, []byte("k"), start); err != nil {
		t.Fatalf("prewrite: %v", err)
	}
	if err := store.Rollback([][]byte{[]byte("k")}, start); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	st, err := store.CheckTxnStatus([]byte("k"), start, time.Now())
	if err != nil {
		t.Fatalf("CheckTxnStatus: %v", err)
	}
	if !st.RolledBack {
		t.Fatalf("status = %+v, want rolled back", st)
	}
}

func TestCheckTxnStatusLiveLock(t *testing.T) {
	store := newTestStore(t)
	start := uint64(time.Now().UnixMilli()) << 18
	if err := store.Prewrite([]Mutation{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
	}, []byte("a"), start); err != nil {
		t.Fatalf("prewrite: %v", err)
	}
	st, err := store.CheckTxnStatus([]byte("b"), 0, time.Now())
	if err != nil {
		t.Fatalf("CheckTxnStatus: %v", err)
	}
	if st.Committed || st.RolledBack || string(st.LockPrimary) != "a" {
		t.Fatalf("status = %+v, want live lock with primary a", st)
	}
	if len(st.Keys) != 2 {
		t.Fatalf("locked keys = %v, want 2", st.Keys)
	}
}

func TestCheckTxnStatusExpiredRollsBack(t *testing.T) {
	store := newTestStore(t)
	start := uint64(time.Now().Add(-10*time.Second).UnixMilli()) << 18
	if err := store.Prewrite([]Mutation{{Key: []byte("k"), Value: []byte("v")}}, []byte("k"), start); err != nil {
		t.Fatalf("prewrite: %v", err)
	}
	st, err := store.CheckTxnStatus([]byte("k"), 0, time.Now())
	if err != nil {
		t.Fatalf("CheckTxnStatus: %v", err)
	}
	if !st.RolledBack || !st.Expired {
		t.Fatalf("status = %+v, want expired rolled back", st)
	}
	if err := store.ResolveLock(st.Keys, st.LockTS, 0); err != nil {
		t.Fatalf("ResolveLock expired: %v", err)
	}
	if _, err := store.Get([]byte("k"), start+1); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get after expiry = %v, want ErrKeyNotFound", err)
	}
}

func TestResolveLock(t *testing.T) {
	store := newTestStore(t)
	start := uint64(time.Now().UnixMilli()) << 18
	commit := start + 1
	if err := store.Prewrite([]Mutation{{Key: []byte("k"), Value: []byte("v")}}, []byte("k"), start); err != nil {
		t.Fatalf("prewrite: %v", err)
	}
	if err := store.ResolveLock([][]byte{[]byte("k")}, start, commit); err != nil {
		t.Fatalf("ResolveLock commit: %v", err)
	}
	got, err := store.Get([]byte("k"), commit)
	if err != nil || string(got) != "v" {
		t.Fatalf("Get after resolve = %q, %v", got, err)
	}

	start2 := commit + 1
	if err := store.Prewrite([]Mutation{{Key: []byte("g"), Value: []byte("x")}}, []byte("g"), start2); err != nil {
		t.Fatalf("prewrite2: %v", err)
	}
	if err := store.ResolveLock([][]byte{[]byte("g")}, start2, 0); err != nil {
		t.Fatalf("ResolveLock rollback: %v", err)
	}
	if _, err := store.Get([]byte("g"), start2+1); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get after rollback = %v, want ErrKeyNotFound", err)
	}
}
