package metaserver

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"distributed-db/internal/meta"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
)

// fakeKV is a KVService replica stub: it records the scheduling requests it
// receives and can be configured to reject them.
type fakeKV struct {
	pbv2.UnimplementedKVServiceServer

	mu       sync.Mutex
	splits   []*pbv2.SplitRangeRequest
	merges   []*pbv2.MergeRangesRequest
	splitErr string
	mergeErr string
}

func (f *fakeKV) SplitRange(_ context.Context, req *pbv2.SplitRangeRequest) (*pbv2.SplitRangeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.splits = append(f.splits, req)
	if f.splitErr != "" {
		return &pbv2.SplitRangeResponse{Error: f.splitErr}, nil
	}
	return &pbv2.SplitRangeResponse{Success: true}, nil
}

func (f *fakeKV) MergeRanges(_ context.Context, req *pbv2.MergeRangesRequest) (*pbv2.MergeRangesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges = append(f.merges, req)
	if f.mergeErr != "" {
		return &pbv2.MergeRangesResponse{Error: f.mergeErr}, nil
	}
	return &pbv2.MergeRangesResponse{Success: true}, nil
}

func (f *fakeKV) splitRequests() []*pbv2.SplitRangeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pbv2.SplitRangeRequest(nil), f.splits...)
}

func (f *fakeKV) mergeRequests() []*pbv2.MergeRangesRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pbv2.MergeRangesRequest(nil), f.merges...)
}

// startFakeKV serves a fake replica on a loopback port and returns its address.
func startFakeKV(t *testing.T, splitErr, mergeErr string) (string, *fakeKV) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &fakeKV{splitErr: splitErr, mergeErr: mergeErr}
	srv := grpc.NewServer()
	pbv2.RegisterKVServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), fake
}

func newSchedulerServer(t *testing.T, ranges []meta.Range) *Server {
	t.Helper()
	s, err := New(Config{
		Addr:   "127.0.0.1:0",
		Ranges: ranges,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("metaserver.New: %v", err)
	}
	return s
}

func snapshotByID(t *testing.T, s *Server) map[uint64]meta.Range {
	t.Helper()
	rows := s.Cluster().Snapshot()
	out := make(map[uint64]meta.Range, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func TestSplitRangeAppliesNewRangeMap(t *testing.T) {
	addr1, kv1 := startFakeKV(t, "", "")
	addr2, kv2 := startFakeKV(t, "", "")
	replicas := []string{addr1, addr2}

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: []byte("z"), Replicas: replicas, Leader: addr1, Version: 3},
		{ID: 7, Start: []byte("z"), End: nil, Replicas: replicas, Leader: addr2, Version: 1},
	})

	resp, err := s.SplitRange(context.Background(), &pbv2.MetaSplitRangeRequest{
		RangeId: 1, SplitKey: []byte("m"),
	})
	if err != nil {
		t.Fatalf("SplitRange: %v", err)
	}
	if !resp.GetSuccess() || resp.GetError() != "" {
		t.Fatalf("split failed: %+v", resp)
	}
	if got, want := resp.GetNewRangeId(), uint64(8); got != want {
		t.Fatalf("new range id = %d, want %d (max existing ID + 1)", got, want)
	}

	rows := snapshotByID(t, s)
	if len(rows) != 3 {
		t.Fatalf("range map after split = %+v, want 3 ranges", rows)
	}
	left := rows[1]
	if !bytes.Equal(left.Start, nil) || !bytes.Equal(left.End, []byte("m")) {
		t.Fatalf("left bounds = [%q, %q), want [-inf, m)", left.Start, left.End)
	}
	if left.Version != 4 {
		t.Fatalf("left version = %d, want 4", left.Version)
	}
	if left.Leader != addr1 {
		t.Fatalf("left leader = %q, want %q (unchanged)", left.Leader, addr1)
	}
	right := rows[8]
	if !bytes.Equal(right.Start, []byte("m")) || !bytes.Equal(right.End, []byte("z")) {
		t.Fatalf("right bounds = [%q, %q), want [m, z)", right.Start, right.End)
	}
	if right.Version != 4 {
		t.Fatalf("right version = %d, want 4", right.Version)
	}
	if len(right.Replicas) != 2 || right.Replicas[0] != addr1 || right.Replicas[1] != addr2 {
		t.Fatalf("right replicas = %v, want %v", right.Replicas, replicas)
	}

	for name, fake := range map[string]*fakeKV{"kv1": kv1, "kv2": kv2} {
		reqs := fake.splitRequests()
		if len(reqs) != 1 {
			t.Fatalf("%s split requests = %d, want 1", name, len(reqs))
		}
		req := reqs[0]
		if req.GetRangeId() != 1 || req.GetNewRangeId() != 8 || req.GetEpoch() != 3 ||
			!bytes.Equal(req.GetSplitKey(), []byte("m")) {
			t.Fatalf("%s split request = %+v, want {range 1, new 8, epoch 3, key m}", name, req)
		}
	}
}

func TestSplitRangeReplicaFailureLeavesMapUnchanged(t *testing.T) {
	addr1, kv1 := startFakeKV(t, "", "")
	addr2, _ := startFakeKV(t, "injected split failure", "")
	replicas := []string{addr1, addr2}

	before := []meta.Range{
		{ID: 1, Start: nil, End: nil, Replicas: replicas, Version: 5},
	}
	s := newSchedulerServer(t, before)

	resp, err := s.SplitRange(context.Background(), &pbv2.MetaSplitRangeRequest{
		RangeId: 1, SplitKey: []byte("m"),
	})
	if err != nil {
		t.Fatalf("SplitRange returned transport error: %v", err)
	}
	if resp.GetSuccess() {
		t.Fatalf("split succeeded despite a failing replica: %+v", resp)
	}
	if !strings.Contains(resp.GetError(), addr2) || !strings.Contains(resp.GetError(), "1/2 replicas") {
		t.Fatalf("error = %q, want failed replica and failure count", resp.GetError())
	}
	if len(kv1.splitRequests()) != 1 {
		t.Fatalf("healthy replica was not asked to split")
	}

	rows := snapshotByID(t, s)
	if len(rows) != 1 {
		t.Fatalf("range map changed after failed split: %+v", rows)
	}
	if got := rows[1]; got.Version != 5 || got.End != nil {
		t.Fatalf("range 1 changed after failed split: %+v", got)
	}
}

func TestSplitRangeIdempotentWhenAlreadySplit(t *testing.T) {
	addr, fake := startFakeKV(t, "", "")
	replicas := []string{addr}

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: []byte("m"), Replicas: replicas, Version: 4},
		{ID: 2, Start: []byte("m"), End: nil, Replicas: replicas, Version: 4},
	})

	resp, err := s.SplitRange(context.Background(), &pbv2.MetaSplitRangeRequest{
		RangeId: 1, SplitKey: []byte("m"),
	})
	if err != nil {
		t.Fatalf("SplitRange: %v", err)
	}
	if !resp.GetSuccess() || resp.GetNewRangeId() != 2 {
		t.Fatalf("idempotent split = %+v, want success with existing range 2", resp)
	}
	if got := len(fake.splitRequests()); got != 0 {
		t.Fatalf("idempotent split contacted replicas %d times, want 0", got)
	}
	rows := snapshotByID(t, s)
	if len(rows) != 2 || rows[1].Version != 4 || rows[2].Version != 4 {
		t.Fatalf("range map changed on idempotent retry: %+v", rows)
	}
}

func TestSplitRangeRejectsInvalidKey(t *testing.T) {
	addr, fake := startFakeKV(t, "", "")
	replicas := []string{addr}

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: []byte("a"), Replicas: replicas, Version: 1},
		{ID: 2, Start: []byte("a"), End: []byte("d"), Replicas: replicas, Version: 1},
		{ID: 3, Start: []byte("d"), End: nil, Replicas: replicas, Version: 1},
	})

	for _, splitKey := range []string{"a", "z", ""} {
		resp, err := s.SplitRange(context.Background(), &pbv2.MetaSplitRangeRequest{
			RangeId: 2, SplitKey: []byte(splitKey),
		})
		if err != nil {
			t.Fatalf("SplitRange(%q) transport error: %v", splitKey, err)
		}
		if resp.GetSuccess() || !strings.Contains(resp.GetError(), "not strictly inside") {
			t.Fatalf("SplitRange(%q) = %+v, want not-strictly-inside error", splitKey, resp)
		}
	}

	// A key equal to the range end is the already-applied split boundary: the
	// right-hand sibling starts there, so the retry is idempotent.
	resp, err := s.SplitRange(context.Background(), &pbv2.MetaSplitRangeRequest{
		RangeId: 2, SplitKey: []byte("d"),
	})
	if err != nil {
		t.Fatalf("SplitRange(d): %v", err)
	}
	if !resp.GetSuccess() || resp.GetNewRangeId() != 3 {
		t.Fatalf("split at existing boundary = %+v, want idempotent success with range 3", resp)
	}

	if got := len(fake.splitRequests()); got != 0 {
		t.Fatalf("invalid/idempotent splits contacted replicas %d times, want 0", got)
	}
	if rows := snapshotByID(t, s); len(rows) != 3 {
		t.Fatalf("range map changed after invalid splits: %+v", rows)
	}
}

func TestSplitRangeUnknownRange(t *testing.T) {
	addr, _ := startFakeKV(t, "", "")
	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: nil, Replicas: []string{addr}, Version: 1},
	})

	resp, err := s.SplitRange(context.Background(), &pbv2.MetaSplitRangeRequest{
		RangeId: 42, SplitKey: []byte("m"),
	})
	if err != nil {
		t.Fatalf("SplitRange: %v", err)
	}
	if resp.GetSuccess() || !strings.Contains(resp.GetError(), "range 42") {
		t.Fatalf("unknown range split = %+v, want descriptive error", resp)
	}
}

func TestMergeRangesAppliesAndIsIdempotent(t *testing.T) {
	addr1, kv1 := startFakeKV(t, "", "")
	addr2, kv2 := startFakeKV(t, "", "")
	replicas := []string{addr1, addr2}

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: []byte("m"), Replicas: replicas, Leader: addr1, Version: 2},
		{ID: 4, Start: []byte("m"), End: []byte("t"), Replicas: replicas, Leader: addr2, Version: 2},
		{ID: 9, Start: []byte("t"), End: nil, Replicas: replicas, Version: 1},
	})

	resp, err := s.MergeRanges(context.Background(), &pbv2.MetaMergeRangesRequest{
		LeftRangeId: 1, RightRangeId: 4,
	})
	if err != nil {
		t.Fatalf("MergeRanges: %v", err)
	}
	if !resp.GetSuccess() || resp.GetError() != "" {
		t.Fatalf("merge failed: %+v", resp)
	}

	rows := snapshotByID(t, s)
	if len(rows) != 2 {
		t.Fatalf("range map after merge = %+v, want 2 ranges", rows)
	}
	merged := rows[1]
	if !bytes.Equal(merged.Start, nil) || !bytes.Equal(merged.End, []byte("t")) {
		t.Fatalf("merged bounds = [%q, %q), want [-inf, t)", merged.Start, merged.End)
	}
	if merged.Version != 3 {
		t.Fatalf("merged version = %d, want 3", merged.Version)
	}
	if merged.Leader != addr1 {
		t.Fatalf("merged leader = %q, want %q", merged.Leader, addr1)
	}
	if _, ok := rows[4]; ok {
		t.Fatalf("right range 4 still present after merge")
	}
	if rows[9].Version != 1 {
		t.Fatalf("unrelated range 9 changed: %+v", rows[9])
	}

	for name, fake := range map[string]*fakeKV{"kv1": kv1, "kv2": kv2} {
		reqs := fake.mergeRequests()
		if len(reqs) != 1 {
			t.Fatalf("%s merge requests = %d, want 1", name, len(reqs))
		}
		req := reqs[0]
		if req.GetLeftRangeId() != 1 || req.GetRightRangeId() != 4 || req.GetEpoch() != 2 {
			t.Fatalf("%s merge request = %+v, want {left 1, right 4, epoch 2}", name, req)
		}
	}

	// Retry after a successful merge: right range is gone, so the call is a
	// success and must not touch replicas again.
	resp, err = s.MergeRanges(context.Background(), &pbv2.MetaMergeRangesRequest{
		LeftRangeId: 1, RightRangeId: 4,
	})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("idempotent merge = %+v, %v; want success", resp, err)
	}
	if got := len(kv1.mergeRequests()); got != 1 {
		t.Fatalf("idempotent merge re-contacted replicas: %d calls", got)
	}
}

func TestMergeRangesReplicaFailureLeavesMapUnchanged(t *testing.T) {
	addr1, _ := startFakeKV(t, "", "")
	addr2, _ := startFakeKV(t, "", "injected merge failure")
	replicas := []string{addr1, addr2}

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: []byte("m"), Replicas: replicas, Version: 6},
		{ID: 2, Start: []byte("m"), End: nil, Replicas: replicas, Version: 6},
	})

	resp, err := s.MergeRanges(context.Background(), &pbv2.MetaMergeRangesRequest{
		LeftRangeId: 1, RightRangeId: 2,
	})
	if err != nil {
		t.Fatalf("MergeRanges: %v", err)
	}
	if resp.GetSuccess() {
		t.Fatalf("merge succeeded despite a failing replica: %+v", resp)
	}
	if !strings.Contains(resp.GetError(), addr2) || !strings.Contains(resp.GetError(), "1/2 replicas") {
		t.Fatalf("error = %q, want failed replica and failure count", resp.GetError())
	}

	rows := snapshotByID(t, s)
	if len(rows) != 2 {
		t.Fatalf("range map changed after failed merge: %+v", rows)
	}
	if rows[1].End == nil || !bytes.Equal(rows[1].End, []byte("m")) || rows[1].Version != 6 {
		t.Fatalf("left range changed after failed merge: %+v", rows[1])
	}
}

func TestMergeRangesValidation(t *testing.T) {
	addr1, fake1 := startFakeKV(t, "", "")
	addr2, fake2 := startFakeKV(t, "", "")

	t.Run("non-adjacent", func(t *testing.T) {
		s := newSchedulerServer(t, []meta.Range{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{addr1}, Version: 1},
			{ID: 2, Start: []byte("m"), End: []byte("t"), Replicas: []string{addr1}, Version: 1},
			{ID: 3, Start: []byte("t"), End: nil, Replicas: []string{addr1}, Version: 1},
		})
		resp, err := s.MergeRanges(context.Background(), &pbv2.MetaMergeRangesRequest{
			LeftRangeId: 1, RightRangeId: 3,
		})
		if err != nil {
			t.Fatalf("MergeRanges: %v", err)
		}
		if resp.GetSuccess() || !strings.Contains(resp.GetError(), "not adjacent") {
			t.Fatalf("non-adjacent merge = %+v, want not-adjacent error", resp)
		}
	})

	t.Run("different replicas", func(t *testing.T) {
		s := newSchedulerServer(t, []meta.Range{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{addr1}, Version: 1},
			{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{addr2}, Version: 1},
		})
		resp, err := s.MergeRanges(context.Background(), &pbv2.MetaMergeRangesRequest{
			LeftRangeId: 1, RightRangeId: 2,
		})
		if err != nil {
			t.Fatalf("MergeRanges: %v", err)
		}
		if resp.GetSuccess() || !strings.Contains(resp.GetError(), "different replicas") {
			t.Fatalf("different-replica merge = %+v, want descriptive error", resp)
		}
	})

	t.Run("self merge", func(t *testing.T) {
		s := newSchedulerServer(t, []meta.Range{
			{ID: 1, Start: nil, End: nil, Replicas: []string{addr1}, Version: 1},
		})
		resp, err := s.MergeRanges(context.Background(), &pbv2.MetaMergeRangesRequest{
			LeftRangeId: 1, RightRangeId: 1,
		})
		if err != nil {
			t.Fatalf("MergeRanges: %v", err)
		}
		if resp.GetSuccess() || !strings.Contains(resp.GetError(), "itself") {
			t.Fatalf("self merge = %+v, want self-merge error", resp)
		}
	})

	t.Run("missing left", func(t *testing.T) {
		s := newSchedulerServer(t, []meta.Range{
			{ID: 1, Start: nil, End: nil, Replicas: []string{addr1}, Version: 1},
		})
		resp, err := s.MergeRanges(context.Background(), &pbv2.MetaMergeRangesRequest{
			LeftRangeId: 5, RightRangeId: 6,
		})
		if err != nil {
			t.Fatalf("MergeRanges: %v", err)
		}
		if resp.GetSuccess() || !strings.Contains(resp.GetError(), "left range 5") {
			t.Fatalf("missing-left merge = %+v, want descriptive error", resp)
		}
	})

	if got := len(fake1.mergeRequests()) + len(fake2.mergeRequests()); got != 0 {
		t.Fatalf("invalid merges contacted replicas %d times, want 0", got)
	}
}

func TestSplitRangeUnreachableReplicaReportsFailure(t *testing.T) {
	// Reserve an address, then close it so the split fan-out gets a connection
	// error instead of a KV response.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := lis.Addr().String()
	_ = lis.Close()

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: nil, Replicas: []string{deadAddr}, Version: 1},
	})
	resp, err := s.SplitRange(context.Background(), &pbv2.MetaSplitRangeRequest{
		RangeId: 1, SplitKey: []byte("m"),
	})
	if err != nil {
		t.Fatalf("SplitRange: %v", err)
	}
	if resp.GetSuccess() || !strings.Contains(resp.GetError(), "1/1 replicas") {
		t.Fatalf("unreachable replica split = %+v, want fan-out failure", resp)
	}
	if rows := snapshotByID(t, s); len(rows) != 1 || rows[1].Version != 1 {
		t.Fatalf("range map changed after unreachable replica: %+v", rows)
	}
}
