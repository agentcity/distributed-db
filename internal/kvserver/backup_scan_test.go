package kvserver

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pbv2 "distributed-db/internal/pb/v2"
)

// newBackupScanRPC starts a second gRPC listener for the same Server, using
// the exported registration helper. The production listener from newStatsServer
// still uses the legacy registration, which is exactly the wiring gap the
// parent will close in server.go.
func newBackupScanRPC(t *testing.T, srv *Server) (pbv2.KVServiceClient, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for backup scan RPC: %v", err)
	}
	gs := grpc.NewServer()
	RegisterKVServiceWithBackupScan(gs, srv)
	go func() {
		if err := gs.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Logf("backup scan gRPC server stopped: %v", err)
		}
	}()
	t.Cleanup(gs.Stop)

	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial backup scan RPC: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pbv2.NewKVServiceClient(cc), lis.Addr().String()
}

func TestBackupScanHandlerLocksAtOrBelowSnapshot(t *testing.T) {
	srv, addr, base := newStatsServer(t, []byte("a"), []byte("z"))
	client, _ := newBackupScanRPC(t, srv)
	ctx := context.Background()

	// Two committed values. putStatsKV commits at startTS+1.
	putStatsKV(t, base, "alpha", "one", 10)
	putStatsKV(t, base, "beta", "two", 20)

	// gamma has never committed; only its lock exists. A plain Scan cannot
	// report this key at all, which is why backup needs the lock column.
	const lockTS = uint64(50)
	pr, err := base.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: lockTS,
		Primary: []byte("gamma"),
		Mutations: []*pbv2.Mutation{{
			Key: []byte("gamma"), Value: []byte("pending"),
		}},
	})
	if err != nil || !pr.GetSuccess() || pr.GetError() != "" {
		t.Fatalf("prewrite gamma: resp=%+v err=%v", pr, err)
	}

	// A lock strictly above the snapshot TSO is invisible and must not block.
	// range_id=0 is the RemoteScanner path: the server resolves the group
	// from start instead of trusting a possibly stale descriptor.
	below, err := client.ScanForBackup(ctx, &pbv2.ScanForBackupRequest{
		Start: []byte("a"), End: []byte("z"), Ts: lockTS - 1, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ScanForBackup below lock TS: %v", err)
	}
	if below.GetLocked() || below.GetError() != "" {
		t.Fatalf("ScanForBackup below lock TS = %+v, want unlocked success", below)
	}
	if below.GetLeader() != addr {
		t.Fatalf("leader = %q, want %q", below.GetLeader(), addr)
	}
	if got := kvValues(below.GetKvs()); len(got) != 2 || got["alpha"] != "one" || got["beta"] != "two" {
		t.Fatalf("committed values = %v, want alpha/beta", got)
	}

	// A lock at exactly the snapshot TSO is at or below it: fail closed with
	// structured details and no partial values.
	at, err := client.ScanForBackup(ctx, &pbv2.ScanForBackupRequest{
		RangeId: 1, Start: []byte("a"), End: []byte("z"), Ts: lockTS, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ScanForBackup at lock TS: %v", err)
	}
	if !at.GetLocked() {
		t.Fatalf("ScanForBackup at lock TS = %+v, want locked", at)
	}
	if at.GetError() != "locked" {
		t.Fatalf("locked error = %q, want locked", at.GetError())
	}
	if !bytes.Equal(at.GetLockedKey(), []byte("gamma")) || at.GetLockTs() != lockTS {
		t.Fatalf("lock details = key %q ts %d, want gamma/%d", at.GetLockedKey(), at.GetLockTs(), lockTS)
	}
	if len(at.GetKvs()) != 0 {
		t.Fatalf("locked response carried partial values: %+v", at.GetKvs())
	}

	// Rolling the lock back makes the same TS safe again.
	rb, err := base.Rollback(ctx, &pbv2.RollbackRequest{
		StartTs: lockTS, Keys: [][]byte{[]byte("gamma")},
	})
	if err != nil || rb.GetError() != "" {
		t.Fatalf("rollback gamma: resp=%+v err=%v", rb, err)
	}
	after, err := client.ScanForBackup(ctx, &pbv2.ScanForBackupRequest{
		RangeId: 1, Start: []byte("a"), End: []byte("z"), Ts: lockTS, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ScanForBackup after rollback: %v", err)
	}
	if after.GetLocked() || after.GetError() != "" {
		t.Fatalf("ScanForBackup after rollback = %+v, want unlocked success", after)
	}
	if got := kvValues(after.GetKvs()); len(got) != 2 || got["alpha"] != "one" || got["beta"] != "two" {
		t.Fatalf("committed values after rollback = %v, want alpha/beta", got)
	}
}

func TestBackupScanHandlerRejectsRangeMismatch(t *testing.T) {
	srv, _, _ := newStatsServer(t, []byte("a"), []byte("z"))
	client, _ := newBackupScanRPC(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// start is inside the range but end crosses its upper bound.
	resp, err := client.ScanForBackup(ctx, &pbv2.ScanForBackupRequest{
		RangeId: 1, Start: []byte("b"), End: []byte("zz"), Ts: 100, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ScanForBackup: %v", err)
	}
	if resp.GetError() != "range mismatch" {
		t.Fatalf("error = %q, want range mismatch", resp.GetError())
	}
	if resp.GetLocked() {
		t.Fatal("range mismatch reported as a lock")
	}

	// An unknown range id is a normal not-hosted error, not a lock.
	resp, err = client.ScanForBackup(ctx, &pbv2.ScanForBackupRequest{
		RangeId: 99, Start: []byte("b"), End: []byte("c"), Ts: 100, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ScanForBackup unknown range: %v", err)
	}
	if resp.GetError() != "not hosted" {
		t.Fatalf("unknown range error = %q, want not hosted", resp.GetError())
	}
}

func kvValues(kvs []*pbv2.KV) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[string(kv.GetKey())] = string(kv.GetValue())
	}
	return out
}
