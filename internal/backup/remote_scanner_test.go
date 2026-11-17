package backup

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	"distributed-db/internal/mvcc"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/v2client"
)

// storeBackupScanServer is a real gRPC KV server backed by an in-process MVCC
// store. It implements the lock-aware ScanForBackup contract so Backup can be
// tested through RemoteScanner over the actual generated RPC, not a mock
// scanner. The handler mirrors kvserver's: a lock at or below ts is an in-band
// locked response with no values.
type storeBackupScanServer struct {
	pbv2.UnimplementedKVServiceServer

	store *mvcc.Store
	addr  string

	mu    sync.Mutex
	calls int
}

func (f *storeBackupScanServer) Status(_ context.Context, req *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &pbv2.StatusResponse{Id: f.addr, Role: "leader", Leader: f.addr, RangeId: req.GetRangeId()}, nil
}

func (f *storeBackupScanServer) ScanForBackup(_ context.Context, req *pbv2.ScanForBackupRequest) (*pbv2.ScanForBackupResponse, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	rows, err := f.store.ScanForBackup(req.GetStart(), req.GetEnd(), req.GetTs(), int(req.GetLimit()))
	if err != nil {
		var lockErr *mvcc.SnapshotLockError
		if errors.As(err, &lockErr) {
			return &pbv2.ScanForBackupResponse{
				Leader: f.addr, Error: "locked", Locked: true,
				LockedKey: append([]byte(nil), lockErr.Key...), LockTs: lockErr.StartTS,
			}, nil
		}
		return &pbv2.ScanForBackupResponse{Leader: f.addr, Error: err.Error()}, nil
	}
	out := make([]*pbv2.KV, 0, len(rows))
	for _, kv := range rows {
		out = append(out, &pbv2.KV{Key: kv.Key, Value: kv.Value})
	}
	return &pbv2.ScanForBackupResponse{Leader: f.addr, Kvs: out}, nil
}

func (f *storeBackupScanServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// startRemoteBackupScanner starts a fake KV node plus a real meta service and
// returns a client routed to the fake node. The client is independent of the
// TestCluster's data client so the RPC path can be exercised while Backup still
// gets its TSO and range map from the normal test cluster.
func startRemoteBackupScanner(t *testing.T, store *mvcc.Store) *v2client.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake backup scan node: %v", err)
	}
	node := &storeBackupScanServer{store: store, addr: lis.Addr().String()}
	gs := grpc.NewServer()
	pbv2.RegisterKVServiceServer(gs, node)
	go func() {
		if err := gs.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Logf("fake backup scan node stopped: %v", err)
		}
	}()
	t.Cleanup(gs.Stop)

	metaAddr := freeAddr(t)
	ms, err := metaserver.New(metaserver.Config{
		Addr: metaAddr,
		Ranges: []meta.Range{{
			ID: 1, Replicas: []string{node.addr}, Leader: node.addr, Version: 1,
		}},
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("new meta for fake backup scan node: %v", err)
	}
	if err := ms.Start(context.Background()); err != nil {
		t.Fatalf("start meta for fake backup scan node: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	c, err := v2client.New(metaAddr, []string{node.addr})
	if err != nil {
		t.Fatalf("new client for fake backup scan node: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRemoteScannerIncludesCommittedValues(t *testing.T) {
	ctx := context.Background()
	cluster := startTestCluster(t, nil, nil)
	store := newLocalMVCC(t)
	storePut(t, store, "alpha", "one", 10, 20)
	storePut(t, store, "beta", "two", 30, 40)
	scannerClient := startRemoteBackupScanner(t, store)

	dir := filepath.Join(t.TempDir(), "remote-ok")
	m, err := Backup(ctx, BackupConfig{
		Client: cluster.client, Meta: cluster.meta, OutDir: dir,
		Scanner:           RemoteScanner{Client: scannerClient},
		LockRetryAttempts: 1,
	})
	if err != nil {
		t.Fatalf("Backup through RemoteScanner: %v", err)
	}
	values := backupValues(t, dir, m)
	if values["alpha"] != "one" || values["beta"] != "two" {
		t.Fatalf("backup values = %v, want alpha/beta committed values", values)
	}
}

func TestRemoteScannerAbortsOnNeverCommittedLock(t *testing.T) {
	ctx := context.Background()
	cluster := startTestCluster(t, nil, nil)
	store := newLocalMVCC(t)
	storePut(t, store, "alpha", "committed", 10, 20)
	// gamma has no committed value at all. Only the lock column knows it
	// exists; a plain Scan would silently omit it.
	if err := store.Prewrite([]mvcc.Mutation{{Key: []byte("gamma"), Value: []byte("pending")}}, []byte("gamma"), 50); err != nil {
		t.Fatalf("prewrite gamma: %v", err)
	}
	scannerClient := startRemoteBackupScanner(t, store)

	dir := filepath.Join(t.TempDir(), "remote-locked")
	_, err := Backup(ctx, BackupConfig{
		Client: cluster.client, Meta: cluster.meta, OutDir: dir,
		Scanner:           RemoteScanner{Client: scannerClient},
		LockRetryAttempts: 1,
	})
	if !errors.Is(err, ErrSnapshotLocked) {
		t.Fatalf("Backup error = %v, want ErrSnapshotLocked", err)
	}
	var lockErr *SnapshotLockedError
	if !errors.As(err, &lockErr) {
		t.Fatalf("Backup error = %v, want *SnapshotLockedError", err)
	}
	if string(lockErr.Key) != "gamma" || lockErr.LockTS != 50 {
		t.Fatalf("lock error = %+v, want gamma/50", lockErr)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("aborted backup published %s (stat error %v)", dir, statErr)
	}
}

func TestRemoteScannerStrictPathAvailable(t *testing.T) {
	ctx := context.Background()
	cluster := startTestCluster(t, nil, nil)
	putKV(t, cluster.client, "alpha", "one")
	ts, err := cluster.client.AllocTS(ctx)
	if err != nil {
		t.Fatalf("AllocTS: %v", err)
	}

	strict := RemoteScanner{Client: cluster.client}
	rows, err := strict.ScanForBackup(ctx, nil, nil, ts, 10)
	if err != nil {
		t.Fatalf("strict RemoteScanner: %v", err)
	}

	if len(rows) != 1 || string(rows[0].Key) != "alpha" || string(rows[0].Value) != "one" {
		t.Fatalf("fallback rows = %+v, want alpha=one", rows)
	}
}

func TestRemoteScannerFallbackProbesCommittedKeyLock(t *testing.T) {
	ctx := context.Background()
	cluster := startTestCluster(t, nil, nil)
	putKV(t, cluster.client, "alpha", "old")

	lockTS, err := cluster.client.AllocTS(ctx)
	if err != nil {
		t.Fatalf("AllocTS for lock: %v", err)
	}
	raw := rawKVClient(t, cluster.nodeAddr)
	rawPrewrite(t, raw, "alpha", "new", lockTS)
	defer rawRollback(t, raw, "alpha", lockTS)
	ts, err := cluster.client.AllocTS(ctx)
	if err != nil {
		t.Fatalf("AllocTS for snapshot: %v", err)
	}

	scanner := RemoteScanner{Client: cluster.client, AllowBestEffortFallback: true}
	_, err = scanner.ScanForBackup(ctx, nil, nil, ts, 10)
	if !errors.Is(err, mvcc.ErrLocked) {
		t.Fatalf("scan error = %v, want mvcc.ErrLocked", err)
	}
	var lockErr *mvcc.SnapshotLockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("scan error = %v, want *mvcc.SnapshotLockError", err)
	}
	if string(lockErr.Key) != "alpha" || lockErr.StartTS != lockTS {
		t.Fatalf("lock error = %+v, want alpha/%d", lockErr, lockTS)
	}
}

func TestBackupRequireLockAwareScanSucceedsWithRemoteScanner(t *testing.T) {
	ctx := context.Background()
	cluster := startTestCluster(t, nil, nil)
	putKV(t, cluster.client, "alpha", "one")

	dir := filepath.Join(t.TempDir(), "strict-remote")
	_, err := Backup(ctx, BackupConfig{
		Client: cluster.client, Meta: cluster.meta, OutDir: dir,
		Scanner:              RemoteScanner{Client: cluster.client},
		RequireLockAwareScan: true,
		LockRetryAttempts:    1,
	})
	if err != nil {
		t.Fatalf("strict backup with remote scanner: %v", err)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("strict backup did not publish %s: %v", dir, statErr)
	}
}
