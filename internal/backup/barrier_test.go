package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"distributed-db/internal/mvcc"
	"distributed-db/internal/v2client"
)

// scanOnly implements SnapshotScanner but not BackupBarrier, like a remote
// client with no server-side barrier RPC.
type scanOnly struct{}

func (scanOnly) ScanForBackup(context.Context, []byte, []byte, uint64, int) ([]v2client.KV, error) {
	return nil, nil
}

// observingScanner records whether a scan ran before the barrier completed and
// forwards to the real in-process snapshot scanner.
type observingScanner struct {
	StoreScanner
	onScan func(ts uint64)
}

func (s observingScanner) ScanForBackup(ctx context.Context, start, end []byte, ts uint64, limit int) ([]v2client.KV, error) {
	if s.onScan != nil {
		s.onScan(ts)
	}
	return s.StoreScanner.ScanForBackup(ctx, start, end, ts, limit)
}

type backupOutcome struct {
	m   *Manifest
	err error
}

// TestBackupWaitsForAppliedCommit checks the 1PC barrier wiring end to end: a
// commit timestamp that is reserved (leased) before the backup TSO is visible
// keeps Backup from scanning, and the concurrent 1PC commit is present in the
// published backup once the lease is released.
func TestBackupWaitsForAppliedCommit(t *testing.T) {
	ctx := context.Background()
	cluster := startTestCluster(t, nil, nil)
	store := newLocalMVCC(t)
	storePut(t, store, "alpha", "old", 10, 20)

	// Simulate a commitTS allocated at 40 whose apply batch has not reached the
	// engine yet. The external lease is the caller-side allocation lease.
	lease := store.AcquireCommitLease(40)
	defer lease.Release()

	barrierEntered := make(chan uint64, 1)
	barrierDone := make(chan struct{})
	scanBeforeBarrier := false
	barrierCompleted := false
	barrier := BarrierFunc(func(ctx context.Context, ts uint64) error {
		barrierEntered <- ts
		err := store.WaitApplied(ctx, ts)
		barrierCompleted = true
		close(barrierDone)
		return err
	})
	scanner := observingScanner{
		StoreScanner: StoreScanner{Store: store},
		onScan: func(uint64) {
			if !barrierCompleted {
				scanBeforeBarrier = true
			}
		},
	}

	dir := filepath.Join(t.TempDir(), "barrier")
	outcome := make(chan backupOutcome, 1)
	go func() {
		m, err := Backup(ctx, BackupConfig{
			Client: cluster.client, Meta: cluster.meta, OutDir: dir,
			Scanner: scanner, Barrier: barrier,
		})
		outcome <- backupOutcome{m: m, err: err}
	}()

	var snapshotTS uint64
	select {
	case snapshotTS = <-barrierEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("backup never reached the apply barrier")
	}
	if snapshotTS == 0 {
		t.Fatal("barrier was called with tso 0")
	}

	select {
	case <-barrierDone:
		t.Fatal("barrier returned while a commit lease at 40 was outstanding")
	case <-time.After(50 * time.Millisecond):
	}

	// A concurrent 1PC commit for the reserved timestamp. It installs no lock,
	// so only the apply barrier (not the lock scan) can protect this window.
	if err := store.CommitSingle(
		[]mvcc.Mutation{{Key: []byte("beta"), Value: []byte("new")}},
		[]byte("beta"), 30, 40); err != nil {
		t.Fatalf("CommitSingle: %v", err)
	}
	select {
	case <-barrierDone:
		t.Fatal("barrier returned while the external allocation lease was still held")
	case <-time.After(50 * time.Millisecond):
	}

	lease.Release()
	select {
	case <-barrierDone:
	case <-time.After(10 * time.Second):
		t.Fatal("barrier did not return after the commit applied")
	}

	var got backupOutcome
	select {
	case got = <-outcome:
	case <-time.After(15 * time.Second):
		t.Fatal("backup did not finish")
	}
	if got.err != nil {
		t.Fatalf("Backup: %v", got.err)
	}
	if got.m.SnapshotTS != snapshotTS {
		t.Fatalf("manifest snapshot_ts = %d, barrier saw %d", got.m.SnapshotTS, snapshotTS)
	}
	if scanBeforeBarrier {
		t.Fatal("backup scanned before WaitApplied returned")
	}
	values := backupValues(t, dir, got.m)
	if values["alpha"] != "old" || values["beta"] != "new" {
		t.Fatalf("backup values = %v, want the committed beta", values)
	}
}

// TestBackupStrictModeRequiresBarrier checks the fail-closed default: a
// scanner that cannot provide an applied-commit barrier cannot be used with
// RequireLockAwareScan, and no output directory is created.
func TestBackupStrictModeRequiresBarrier(t *testing.T) {
	cluster := startTestCluster(t, nil, nil)
	dir := filepath.Join(t.TempDir(), "no-barrier")
	_, err := Backup(context.Background(), BackupConfig{
		Client: cluster.client, Meta: cluster.meta, OutDir: dir,
		Scanner: scanOnly{}, RequireLockAwareScan: true,
	})
	if !errors.Is(err, ErrBackupBarrierUnavailable) {
		t.Fatalf("Backup error = %v, want ErrBackupBarrierUnavailable", err)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected backup created %s (stat error %v)", dir, statErr)
	}
}

// TestStoreScannerSatisfiesStrictBarrier checks the implicit adapter: a
// StoreScanner provides both the lock-aware scan and Store.WaitApplied.
func TestStoreScannerSatisfiesStrictBarrier(t *testing.T) {
	cluster := startTestCluster(t, nil, nil)
	store := newLocalMVCC(t)
	storePut(t, store, "alpha", "one", 10, 20)
	dir := filepath.Join(t.TempDir(), "store-strict")
	m, err := Backup(context.Background(), BackupConfig{
		Client: cluster.client, Meta: cluster.meta, OutDir: dir,
		Scanner: StoreScanner{Store: store}, RequireLockAwareScan: true,
	})
	if err != nil {
		t.Fatalf("strict backup with StoreScanner: %v", err)
	}
	if got := backupValues(t, dir, m); got["alpha"] != "one" {
		t.Fatalf("backup values = %v", got)
	}
}

// TestBackupBarrierErrorFailsClosed checks that a barrier timeout/error aborts
// the attempt before publication and removes the staging directory.
func TestBackupBarrierErrorFailsClosed(t *testing.T) {
	cluster := startTestCluster(t, nil, nil)
	sentinel := errors.New("barrier timeout")
	dir := filepath.Join(t.TempDir(), "barrier-error")
	parent := filepath.Dir(dir)
	_, err := Backup(context.Background(), BackupConfig{
		Client: cluster.client, Meta: cluster.meta, OutDir: dir,
		Scanner: scanOnly{}, Barrier: BarrierFunc(func(context.Context, uint64) error {
			return sentinel
		}),
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Backup error = %v, want sentinel", err)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed backup published %s", dir)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil {
		t.Fatalf("read parent: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed backup left %d entries behind", len(entries))
	}
}
