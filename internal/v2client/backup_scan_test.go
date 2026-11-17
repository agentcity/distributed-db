package v2client

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"distributed-db/internal/mvcc"
	pbv2 "distributed-db/internal/pb/v2"
)

// backupScanKV is an in-process KV node used to exercise the real generated
// gRPC client/server path for ScanForBackup.
type backupScanKV struct {
	pbv2.UnimplementedKVServiceServer

	mu        sync.Mutex
	addr      string
	rows      []*pbv2.KV
	scanRows  []*pbv2.KV
	locked    bool
	lockKey   []byte
	lockTS    uint64
	notLeader int
	requests  []*pbv2.ScanForBackupRequest
	scans     []*pbv2.ScanRequest
	epochs    []string
}

func (f *backupScanKV) Status(_ context.Context, req *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &pbv2.StatusResponse{Id: f.addr, Role: "leader", Leader: f.addr, RangeId: req.GetRangeId()}, nil
}

func (f *backupScanKV) ScanForBackup(ctx context.Context, req *pbv2.ScanForBackupRequest) (*pbv2.ScanForBackupResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	epoch := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("x-range-epoch"); len(v) > 0 {
			epoch = v[0]
		}
	}
	f.epochs = append(f.epochs, epoch)
	if f.notLeader > 0 {
		f.notLeader--
		return &pbv2.ScanForBackupResponse{Leader: f.addr, Error: "not leader"}, nil
	}
	if f.locked {
		return &pbv2.ScanForBackupResponse{
			Leader: f.addr, Error: "locked",
			Locked: true, LockedKey: append([]byte(nil), f.lockKey...), LockTs: f.lockTS,
		}, nil
	}
	return &pbv2.ScanForBackupResponse{Leader: f.addr, Kvs: cloneTestKVs(f.rows)}, nil
}

func (f *backupScanKV) Scan(_ context.Context, req *pbv2.ScanRequest) (*pbv2.ScanResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans = append(f.scans, req)
	return &pbv2.ScanResponse{Leader: f.addr, Kvs: cloneTestKVs(f.scanRows)}, nil
}

func (f *backupScanKV) stats() (scanForBackup, scan int, epochs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests), len(f.scans), append([]string(nil), f.epochs...)
}

func cloneTestKVs(in []*pbv2.KV) []*pbv2.KV {
	out := make([]*pbv2.KV, len(in))
	for i, kv := range in {
		out[i] = &pbv2.KV{Key: append([]byte(nil), kv.GetKey()...), Value: append([]byte(nil), kv.GetValue()...)}
	}
	return out
}

// backupUnsupportedKV is an older node: it implements Status so leader
// discovery works, but inherits the generated Unimplemented ScanForBackup.
type backupUnsupportedKV struct {
	pbv2.UnimplementedKVServiceServer
	addr string
}

func (f *backupUnsupportedKV) Status(_ context.Context, _ *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	return &pbv2.StatusResponse{Id: f.addr, Role: "leader", Leader: f.addr}, nil
}

// startBackupScanServer starts a real gRPC listener for srv and returns a
// client routed to it through the normal fake meta/range cache.
func startBackupScanServer(t *testing.T, srv pbv2.KVServiceServer) *Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	pbv2.RegisterKVServiceServer(gs, srv)
	go func() {
		if err := gs.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Logf("backup scan test server stopped: %v", err)
		}
	}()
	t.Cleanup(gs.Stop)

	addr := lis.Addr().String()
	switch v := srv.(type) {
	case *backupScanKV:
		v.mu.Lock()
		v.addr = addr
		v.mu.Unlock()
	case *backupUnsupportedKV:
		v.addr = addr
	}

	meta := newFakeMeta("meta1")
	meta.setGetRange(func([]byte) (*pbv2.RangeInfo, error) {
		return &pbv2.RangeInfo{
			Id: 1, Version: 1, Replicas: []string{addr}, Leader: addr,
		}, nil
	})
	c := newTestClient(meta)
	c.addrs = []string{addr}
	return c
}

func TestScanForBackupRoundTrip(t *testing.T) {
	f := &backupScanKV{rows: []*pbv2.KV{
		{Key: []byte("alpha"), Value: []byte("one")},
		{Key: []byte("beta"), Value: []byte("two")},
	}}
	c := startBackupScanServer(t, f)

	rows, err := c.ScanForBackup(context.Background(), 7, []byte("a"), []byte("z"), 123, 2)
	if err != nil {
		t.Fatalf("ScanForBackup: %v", err)
	}
	if got := len(rows); got != 2 || string(rows[0].Key) != "alpha" || string(rows[1].Value) != "two" {
		t.Fatalf("rows = %+v, want alpha/beta", rows)
	}
	f.mu.Lock()
	req := f.requests[0]
	f.mu.Unlock()
	if req.GetRangeId() != 7 || string(req.GetStart()) != "a" || string(req.GetEnd()) != "z" ||
		req.GetTs() != 123 || req.GetLimit() != 2 {
		t.Fatalf("request = %+v, want range 7 [a,z) ts 123 limit 2", req)
	}
	_, _, epochs := f.stats()
	if len(epochs) != 1 || epochs[0] != "1" {
		t.Fatalf("range epochs = %v, want [1]", epochs)
	}
}

func TestScanForBackupFollowsLeaderRedirect(t *testing.T) {
	f := &backupScanKV{
		rows:      []*pbv2.KV{{Key: []byte("alpha"), Value: []byte("one")}},
		notLeader: 1,
	}
	c := startBackupScanServer(t, f)

	rows, err := c.ScanForBackup(context.Background(), 1, []byte("a"), []byte("z"), 100, 10)
	if err != nil {
		t.Fatalf("ScanForBackup after redirect: %v", err)
	}
	if len(rows) != 1 || string(rows[0].Key) != "alpha" {
		t.Fatalf("rows = %+v, want alpha", rows)
	}
	calls, _, _ := f.stats()
	if calls != 2 {
		t.Fatalf("ScanForBackup calls = %d, want 2 (one redirect + success)", calls)
	}
}

func TestScanForBackupReturnsStructuredLockError(t *testing.T) {
	f := &backupScanKV{locked: true, lockKey: []byte("gamma"), lockTS: 50}
	c := startBackupScanServer(t, f)

	rows, err := c.ScanForBackup(context.Background(), 1, []byte("a"), []byte("z"), 50, 10)
	if err == nil {
		t.Fatal("ScanForBackup returned no error for a lock at the snapshot TSO")
	}
	if rows != nil {
		t.Fatalf("ScanForBackup returned rows alongside a lock error: %+v", rows)
	}
	if !errors.Is(err, mvcc.ErrLocked) {
		t.Fatalf("error = %v, want errors.Is(mvcc.ErrLocked)", err)
	}
	var lockErr *mvcc.SnapshotLockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("error = %v, want *mvcc.SnapshotLockError", err)
	}
	if string(lockErr.Key) != "gamma" || lockErr.StartTS != 50 || lockErr.SnapshotTS != 50 {
		t.Fatalf("lock error = %+v, want gamma/50 at snapshot 50", lockErr)
	}
	if calls, _, _ := f.stats(); calls != 1 {
		t.Fatalf("ScanForBackup calls = %d, want 1 (locks are not retried)", calls)
	}
}

func TestScanForBackupReportsUnavailableServer(t *testing.T) {
	c := startBackupScanServer(t, &backupUnsupportedKV{})

	rows, err := c.ScanForBackup(context.Background(), 1, []byte("a"), []byte("z"), 100, 10)
	if err == nil {
		t.Fatal("ScanForBackup succeeded against a server without the RPC")
	}
	if !errors.Is(err, ErrBackupScanUnavailable) {
		t.Fatalf("error = %v, want ErrBackupScanUnavailable", err)
	}
	if rows != nil {
		t.Fatalf("unsupported server returned rows: %+v", rows)
	}
}

func TestScanBestEffortUsesCallerSnapshotTS(t *testing.T) {
	f := &backupScanKV{scanRows: []*pbv2.KV{{Key: []byte("alpha"), Value: []byte("old")}}}
	c := startBackupScanServer(t, f)

	rows, err := c.ScanBestEffort(context.Background(), []byte("a"), []byte("z"), 77, 5)
	if err != nil {
		t.Fatalf("ScanBestEffort: %v", err)
	}
	if len(rows) != 1 || string(rows[0].Value) != "old" {
		t.Fatalf("rows = %+v, want alpha=old", rows)
	}
	f.mu.Lock()
	req := f.scans[0]
	f.mu.Unlock()
	if req.GetTs() != 77 || req.GetLimit() != 5 {
		t.Fatalf("Scan request = %+v, want ts 77 limit 5", req)
	}
}
