package kvserver

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func testFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve addr: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestMultiRangeHost(t *testing.T) {
	ctx := context.Background()
	addr := testFreeAddr(t)
	srv, err := New(Config{
		ID:       addr,
		RaftAddr: addr,
		DataDir:  t.TempDir(),
		// Two single-node ranges in one process. The peer sets are disjoint
		// (both empty); the Raft nodes themselves are independent.
		Ranges: []RangeConfig{
			{ID: 1, Start: nil, End: []byte("m")},
			{ID: 2, Start: []byte("m"), End: nil},
		},
		ElectionTimeoutMin: 50 * time.Millisecond,
		ElectionTimeoutMax: 100 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             testLogger(),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Stop(stopCtx); err != nil {
			t.Errorf("stop: %v", err)
		}
		// Stop must be idempotent.
		if err := srv.Stop(stopCtx); err != nil {
			t.Errorf("second stop: %v", err)
		}
	})

	if got, want := srv.Groups(), []uint64{1, 2}; !equalIDs(got, want) {
		t.Fatalf("Groups() = %v, want %v", got, want)
	}
	ranges := srv.Ranges()
	if len(ranges) != 2 || ranges[0].ID != 1 || ranges[1].ID != 2 {
		t.Fatalf("Ranges() = %+v", ranges)
	}
	if ranges[0].Start != nil || !bytes.Equal(ranges[0].End, []byte("m")) {
		t.Fatalf("range 1 bounds = [%q,%q)", ranges[0].Start, ranges[0].End)
	}
	if !bytes.Equal(ranges[1].Start, []byte("m")) || ranges[1].End != nil {
		t.Fatalf("range 2 bounds = [%q,%q)", ranges[1].Start, ranges[1].End)
	}

	// Accessors used by tests and the future split path.
	node1, err := srv.GroupForID(1)
	if err != nil || node1 == nil {
		t.Fatalf("GroupForID(1) = %v, %v", node1, err)
	}
	node2, err := srv.GroupForID(2)
	if err != nil || node2 == nil {
		t.Fatalf("GroupForID(2) = %v, %v", node2, err)
	}
	if node1 == node2 {
		t.Fatal("range 1 and range 2 share a Raft node")
	}
	if srv.Raft() != node1 {
		t.Fatal("Raft() does not return the default (first) range")
	}
	if n, err := srv.GroupForKey([]byte("a")); err != nil || n != node1 {
		t.Fatalf("GroupForKey(a) = %v, %v; want range 1", n, err)
	}
	if n, err := srv.GroupForKey([]byte("m")); err != nil || n != node2 {
		t.Fatalf("GroupForKey(m) = %v, %v; want range 2", n, err)
	}
	if n, err := srv.GroupForKey([]byte("z")); err != nil || n != node2 {
		t.Fatalf("GroupForKey(z) = %v, %v; want range 2", n, err)
	}

	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	client := pbv2.NewKVServiceClient(cc)

	waitLeader := func(rangeID uint64) *pbv2.StatusResponse {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			callCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			st, err := client.Status(callCtx, &pbv2.StatusRequest{RangeId: rangeID})
			cancel()
			if err == nil && strings.EqualFold(st.GetRole(), "leader") {
				return st
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("range %d did not elect a leader", rangeID)
		return nil
	}

	st1 := waitLeader(1)
	st2 := waitLeader(2)
	if st1.GetRangeId() != 1 || st2.GetRangeId() != 2 {
		t.Fatalf("status ranges = %d/%d, want 1/2", st1.GetRangeId(), st2.GetRangeId())
	}
	if st1.GetId() != addr || st2.GetId() != addr {
		t.Fatalf("status node ids = %q/%q, want %q", st1.GetId(), st2.GetId(), addr)
	}
	if st1.GetRangeStart() != nil || !bytes.Equal(st1.GetRangeEnd(), []byte("m")) {
		t.Fatalf("range 1 status bounds = [%q,%q)", st1.GetRangeStart(), st1.GetRangeEnd())
	}
	if !bytes.Equal(st2.GetRangeStart(), []byte("m")) || st2.GetRangeEnd() != nil {
		t.Fatalf("range 2 status bounds = [%q,%q)", st2.GetRangeStart(), st2.GetRangeEnd())
	}

	// Status without range_id stays backward compatible and selects the
	// default (first) group.
	defaultStatus, err := client.Status(ctx, &pbv2.StatusRequest{})
	if err != nil {
		t.Fatalf("default status: %v", err)
	}
	if defaultStatus.GetRangeId() != 1 {
		t.Fatalf("default status range = %d, want 1", defaultStatus.GetRangeId())
	}
	if _, err := client.Status(ctx, &pbv2.StatusRequest{RangeId: 99}); err == nil {
		t.Fatal("Status(range 99) succeeded, want not-hosted error")
	}

	put := func(key, value string, startTS, commitTS uint64) {
		t.Helper()
		pr, err := client.Prewrite(ctx, &pbv2.PrewriteRequest{
			StartTs: startTS,
			Primary: []byte(key),
			Mutations: []*pbv2.Mutation{{
				Key:   []byte(key),
				Value: []byte(value),
			}},
		})
		if err != nil || !pr.GetSuccess() {
			t.Fatalf("prewrite %q: %v %+v", key, err, pr)
		}
		cr, err := client.Commit(ctx, &pbv2.CommitRequest{
			StartTs:  startTS,
			CommitTs: commitTS,
			Keys:     [][]byte{[]byte(key)},
		})
		if err != nil || !cr.GetSuccess() {
			t.Fatalf("commit %q: %v %+v", key, err, cr)
		}
		gr, err := client.Get(ctx, &pbv2.GetRequest{Key: []byte(key), Ts: commitTS})
		if err != nil {
			t.Fatalf("get %q: %v", key, err)
		}
		if !gr.GetFound() || string(gr.GetValue()) != value {
			t.Fatalf("get %q = %q, found=%v, err=%q", key, gr.GetValue(), gr.GetFound(), gr.GetError())
		}
	}

	// One transaction in each range through the same pb client and gRPC port.
	put("alpha", "one", 100, 101)
	put("zulu", "two", 200, 201)

	// Operations that span ranges are rejected before reaching Raft.
	pr, err := client.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: 300,
		Primary: []byte("alpha"),
		Mutations: []*pbv2.Mutation{
			{Key: []byte("alpha"), Value: []byte("x")},
			{Key: []byte("zulu"), Value: []byte("y")},
		},
	})
	if err != nil {
		t.Fatalf("cross-range prewrite: %v", err)
	}
	if pr.GetSuccess() || !strings.Contains(pr.GetError(), "range mismatch") {
		t.Fatalf("cross-range prewrite = %+v, want range mismatch", pr)
	}
	cr, err := client.Commit(ctx, &pbv2.CommitRequest{
		StartTs: 300, CommitTs: 301,
		Keys: [][]byte{[]byte("alpha"), []byte("zulu")},
	})
	if err != nil || cr.GetSuccess() || !strings.Contains(cr.GetError(), "range mismatch") {
		t.Fatalf("cross-range commit = %+v, %v", cr, err)
	}
	rb, err := client.Rollback(ctx, &pbv2.RollbackRequest{
		StartTs: 300, Keys: [][]byte{[]byte("alpha"), []byte("zulu")},
	})
	if err != nil || rb.GetSuccess() || !strings.Contains(rb.GetError(), "range mismatch") {
		t.Fatalf("cross-range rollback = %+v, %v", rb, err)
	}
	rl, err := client.ResolveLock(ctx, &pbv2.ResolveLockRequest{
		StartTs: 300, CommitTs: 301,
		Keys: [][]byte{[]byte("alpha"), []byte("zulu")},
	})
	if err != nil || rl.GetSuccess() || !strings.Contains(rl.GetError(), "range mismatch") {
		t.Fatalf("cross-range resolve = %+v, %v", rl, err)
	}
	sr, err := client.Scan(ctx, &pbv2.ScanRequest{
		Start: []byte("alpha"), End: []byte("zulu"), Ts: 1 << 62,
	})
	if err != nil || !strings.Contains(sr.GetError(), "range mismatch") {
		t.Fatalf("cross-range scan = %+v, %v, want range mismatch", sr, err)
	}

	// A scan that stays inside range 2 is served by range 2.
	sr, err = client.Scan(ctx, &pbv2.ScanRequest{Start: []byte("m"), End: []byte("zzz"), Ts: 1 << 62})
	if err != nil || sr.GetError() != "" {
		t.Fatalf("range 2 scan: %+v, %v", sr, err)
	}
	if len(sr.GetKvs()) != 1 || string(sr.GetKvs()[0].GetKey()) != "zulu" {
		t.Fatalf("range 2 scan rows = %+v, want zulu", sr.GetKvs())
	}
}

func equalIDs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
