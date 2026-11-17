package mvcc

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func requireBackupLock(t *testing.T, err error, key string, lockTS uint64) {
	t.Helper()
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("error = %v, want errors.Is(_, ErrLocked)", err)
	}
	var lockErr *SnapshotLockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("error = %v, want *SnapshotLockError", err)
	}
	if string(lockErr.Key) != key || lockErr.StartTS != lockTS {
		t.Fatalf("lock error = %+v, want key %q startTS %d", lockErr, key, lockTS)
	}
}

func TestScanForBackupRejectsLockAtOrBelowSnapshot(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "committed", "old", 10, 20)
	if err := store.Prewrite([]Mutation{{Key: []byte("pending"), Value: []byte("new")}}, []byte("pending"), 50); err != nil {
		t.Fatalf("prewrite pending: %v", err)
	}

	kvs, err := store.ScanForBackup(nil, nil, 100, 0)
	if len(kvs) != 0 {
		t.Fatalf("ScanForBackup returned %v together with an error", kvs)
	}
	requireBackupLock(t, err, "pending", 50)
}

func TestScanForBackupLockTimestampBoundary(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "k", "old", 10, 20)
	if err := store.Prewrite([]Mutation{{Key: []byte("k"), Value: []byte("new")}}, []byte("k"), 50); err != nil {
		t.Fatalf("prewrite: %v", err)
	}

	// startTS > snapshot TS belongs to an invisible transaction.
	kvs, err := store.ScanForBackup(nil, nil, 49, 0)
	if err != nil {
		t.Fatalf("ScanForBackup below lock TS: %v", err)
	}
	if len(kvs) != 1 || string(kvs[0].Key) != "k" || string(kvs[0].Value) != "old" {
		t.Fatalf("ScanForBackup below lock TS = %v, want previous committed value", kvs)
	}

	// startTS == snapshot TS is visible and must fail closed.
	if _, err := store.ScanForBackup(nil, nil, 50, 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("ScanForBackup at lock TS error = %v, want ErrLocked", err)
	}
}

func TestScanForBackupIgnoresInvisibleLockOnlyKey(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "k", "v", 10, 20)
	if err := store.Prewrite([]Mutation{{Key: []byte("future"), Value: []byte("x")}}, []byte("future"), 200); err != nil {
		t.Fatalf("prewrite: %v", err)
	}
	kvs, err := store.ScanForBackup(nil, nil, 100, 0)
	if err != nil {
		t.Fatalf("ScanForBackup: %v", err)
	}
	if len(kvs) != 1 || string(kvs[0].Key) != "k" {
		t.Fatalf("ScanForBackup = %v, want only k", kvs)
	}
}

func TestScanForBackupIncludesEveryCommittedValue(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "a", "one", 10, 20)
	putValue(t, store, "b", "two", 30, 40)
	putValue(t, store, "c", "gone", 50, 60)
	deleteValue(t, store, "c", 70, 80)
	putValue(t, store, "a", "one-new", 90, 100)
	if err := store.Prewrite([]Mutation{{Key: []byte("rolled"), Value: []byte("x")}}, []byte("rolled"), 110); err != nil {
		t.Fatalf("prewrite rollback: %v", err)
	}
	if err := store.Rollback([][]byte{[]byte("rolled")}, 110); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	want, err := store.Scan(nil, nil, 120, 0)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got, err := store.ScanForBackup(nil, nil, 120, 0)
	if err != nil {
		t.Fatalf("ScanForBackup: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ScanForBackup = %v, Scan = %v", got, want)
	}
	for i := range want {
		if string(got[i].Key) != string(want[i].Key) || string(got[i].Value) != string(want[i].Value) {
			t.Fatalf("ScanForBackup[%d] = %q=%q, Scan = %q=%q",
				i, got[i].Key, got[i].Value, want[i].Key, want[i].Value)
		}
	}
	if len(got) != 2 || string(got[0].Key) != "a" || string(got[0].Value) != "one-new" || string(got[1].Key) != "b" {
		t.Fatalf("committed snapshot = %v, want a=one-new,b=two", got)
	}

	// limit and range behave like Scan.
	limited, err := store.ScanForBackup(nil, nil, 120, 1)
	if err != nil {
		t.Fatalf("ScanForBackup limit: %v", err)
	}
	if len(limited) != 1 || string(limited[0].Key) != "a" {
		t.Fatalf("limited ScanForBackup = %v", limited)
	}
	ranged, err := store.ScanForBackup([]byte("b"), nil, 120, 0)
	if err != nil {
		t.Fatalf("ScanForBackup range: %v", err)
	}
	if len(ranged) != 1 || string(ranged[0].Key) != "b" {
		t.Fatalf("ranged ScanForBackup = %v", ranged)
	}
}

func TestScanForBackupLockOutsideRangeIsIgnored(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "alpha", "one", 10, 20)
	if err := store.Prewrite([]Mutation{{Key: []byte("zulu"), Value: []byte("pending")}}, []byte("zulu"), 30); err != nil {
		t.Fatalf("prewrite: %v", err)
	}
	if _, err := store.ScanForBackup(nil, []byte("m"), 100, 0); err != nil {
		t.Fatalf("lock outside requested range must be ignored: %v", err)
	}
	kvs, err := store.ScanForBackup(nil, []byte("m"), 100, 0)
	if err != nil {
		t.Fatalf("ScanForBackup: %v", err)
	}
	if len(kvs) != 1 || string(kvs[0].Key) != "alpha" {
		t.Fatalf("ScanForBackup = %v, want alpha", kvs)
	}
	if _, err := store.ScanForBackup([]byte("alpha"), []byte("zulu"), 100, 0); err != nil {
		t.Fatalf("exclusive end lock must be ignored: %v", err)
	}
	if _, err := store.ScanForBackup(nil, []byte("zulu\x00"), 100, 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("lock inside widened range error = %v, want ErrLocked", err)
	}
}

// TestScanForBackupDoesNotChangeNormalScan is the no-regression guard: Get
// reports ErrLocked, normal Scan keeps returning the previous committed value,
// and only the backup mode fails closed.
func TestScanForBackupDoesNotChangeNormalScan(t *testing.T) {
	store := newTestStore(t)
	putValue(t, store, "k", "old", 10, 20)
	if err := store.Prewrite([]Mutation{{Key: []byte("k"), Value: []byte("new")}}, []byte("k"), 50); err != nil {
		t.Fatalf("prewrite: %v", err)
	}

	if _, err := store.Get([]byte("k"), 100); !errors.Is(err, ErrLocked) {
		t.Fatalf("Get error = %v, want ErrLocked", err)
	}
	kvs, err := store.Scan(nil, nil, 100, 0)
	if err != nil {
		t.Fatalf("normal Scan must keep ignoring locks: %v", err)
	}
	if len(kvs) != 1 || string(kvs[0].Value) != "old" {
		t.Fatalf("normal Scan = %v, want previous committed value", kvs)
	}
	if _, err := store.ScanForBackup(nil, nil, 100, 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("ScanForBackup error = %v, want ErrLocked", err)
	}
}

// TestScanForBackupConcurrentCommitNeverLosesAcknowledgedValue races a commit
// against the backup scan. Every outcome must be either "lock observed, scan
// failed" or "committed value returned"; a successful scan that omits the value
// would be an acknowledged-loss bug.
func TestScanForBackupConcurrentCommitNeverLosesAcknowledgedValue(t *testing.T) {
	const iterations = 200
	for i := 0; i < iterations; i++ {
		t.Run(fmt.Sprintf("iter-%03d", i), func(t *testing.T) {
			store := New(newFakeEngine())
			if err := store.Prewrite([]Mutation{{Key: []byte("k"), Value: []byte("v")}}, []byte("k"), 10); err != nil {
				t.Fatalf("prewrite: %v", err)
			}

			commitErr := make(chan error, 1)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				commitErr <- store.Commit([][]byte{[]byte("k")}, 10, 50)
			}()

			kvs, scanErr := store.ScanForBackup(nil, nil, 100, 0)
			wg.Wait()
			if err := <-commitErr; err != nil {
				t.Fatalf("commit: %v", err)
			}
			if scanErr != nil {
				if !errors.Is(scanErr, ErrLocked) {
					t.Fatalf("ScanForBackup: %v", scanErr)
				}
				return
			}
			if len(kvs) != 1 || string(kvs[0].Key) != "k" || string(kvs[0].Value) != "v" {
				t.Fatalf("acknowledged commit at 50 is missing from a successful scan: %v", kvs)
			}
		})
	}
}
