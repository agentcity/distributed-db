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
	"time"

	"distributed-db/internal/meta"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeKV is a KVService replica stub: it records the scheduling requests it
// receives and can be configured to reject them.
type fakeKV struct {
	pbv2.UnimplementedKVServiceServer

	mu           sync.Mutex
	addr         string
	splits       []*pbv2.SplitRangeRequest
	merges       []*pbv2.MergeRangesRequest
	stats        []*pbv2.RangeStatsRequest
	statsByRange map[uint64]*pbv2.RangeStatsResponse
	splitErr     string
	mergeErr     string
	statsErr     string
	statsLeader  string
	transfers    []*pbv2.TransferLeaderRequest
	transferErr  string
	state        *fakeLeaderState
}

// fakeLeaderState is shared by the fake replicas of new leader-rebalance tests
// so Status and TransferLeader observe the same current leader per range.
type fakeLeaderState struct {
	mu      sync.Mutex
	leaders map[uint64]string
}

func newFakeLeaderState() *fakeLeaderState {
	return &fakeLeaderState{leaders: make(map[uint64]string)}
}

func (s *fakeLeaderState) setLeader(rangeID uint64, leader string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaders[rangeID] = leader
}

func (s *fakeLeaderState) leaderFor(rangeID uint64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaders[rangeID]
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

func (f *fakeKV) RangeStats(_ context.Context, req *pbv2.RangeStatsRequest) (*pbv2.RangeStatsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats = append(f.stats, req)
	if f.statsErr != "" {
		return &pbv2.RangeStatsResponse{Leader: f.statsLeader, Error: f.statsErr}, nil
	}
	if resp := f.statsByRange[req.GetRangeId()]; resp != nil {
		return resp, nil
	}
	return &pbv2.RangeStatsResponse{Success: true, Leader: f.addr}, nil
}

func (f *fakeKV) setRangeStats(rangeID uint64, resp *pbv2.RangeStatsResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statsByRange == nil {
		f.statsByRange = make(map[uint64]*pbv2.RangeStatsResponse)
	}
	f.statsByRange[rangeID] = resp
}

func (f *fakeKV) statsRequests() []*pbv2.RangeStatsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pbv2.RangeStatsRequest(nil), f.stats...)
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

// TransferLeader records the request and updates the shared leader map on
// success. transferErr simulates a replica that rejects the handover.
func (f *fakeKV) TransferLeader(_ context.Context, req *pbv2.TransferLeaderRequest) (*pbv2.TransferLeaderResponse, error) {
	f.mu.Lock()
	f.transfers = append(f.transfers, req)
	errMsg := f.transferErr
	state := f.state
	current := f.addr
	f.mu.Unlock()

	if errMsg != "" {
		return &pbv2.TransferLeaderResponse{Leader: current, Error: errMsg}, nil
	}
	if state != nil {
		state.setLeader(req.GetRangeId(), req.GetTarget())
	}
	return &pbv2.TransferLeaderResponse{Success: true, Leader: req.GetTarget()}, nil
}

// Status reports this fake as leader when the shared leader map says so.
func (f *fakeKV) Status(_ context.Context, req *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	f.mu.Lock()
	state := f.state
	f.mu.Unlock()

	leader := f.addr
	if state != nil {
		if current := state.leaderFor(req.GetRangeId()); current != "" {
			leader = current
		}
	}
	role := "follower"
	if leader == f.addr {
		role = "leader"
	}
	return &pbv2.StatusResponse{Id: f.addr, Role: role, Leader: leader, RangeId: req.GetRangeId()}, nil
}

func (f *fakeKV) transferRequests() []*pbv2.TransferLeaderRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pbv2.TransferLeaderRequest(nil), f.transfers...)
}

// startFakeKV serves a fake replica on a loopback port and returns its address.
func startFakeKV(t *testing.T, splitErr, mergeErr string) (string, *fakeKV) {
	t.Helper()
	return startFakeKVWithStats(t, splitErr, mergeErr, "")
}

// startFakeKVWithStats also configures the fake's RangeStats behavior.
func startFakeKVWithStats(t *testing.T, splitErr, mergeErr, statsErr string) (string, *fakeKV) {
	t.Helper()
	return startFakeKVServer(t, &fakeKV{splitErr: splitErr, mergeErr: mergeErr, statsErr: statsErr})
}

// startFakeKVWithState starts a fake replica that shares leadership state with
// its siblings, so Status and TransferLeader behave like a real replica set.
func startFakeKVWithState(t *testing.T, state *fakeLeaderState) (string, *fakeKV) {
	t.Helper()
	return startFakeKVServer(t, &fakeKV{state: state})
}

func startFakeKVServer(t *testing.T, fake *fakeKV) (string, *fakeKV) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	fake.addr = addr
	srv := grpc.NewServer()
	pbv2.RegisterKVServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return addr, fake
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
	if len(kv1.splitRequests()) < 1 {
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

// fakeRangeStats builds a RangeStats response for the scheduler tests.
func fakeRangeStats(addr string, size, keys uint64, splitKey string, start, end []byte) *pbv2.RangeStatsResponse {
	return &pbv2.RangeStatsResponse{
		Success: true, Leader: addr, SizeBytes: size, KeyCount: keys,
		SplitKey: []byte(splitKey), Start: start, End: end,
	}
}

func TestV2MetaSchedulerAutoSplitOverThreshold(t *testing.T) {
	addr, fake := startFakeKV(t, "", "")
	fake.setRangeStats(1, fakeRangeStats(addr, 4<<20, 1000, "m", nil, nil))

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: nil, Replicas: []string{addr}, Leader: addr, Version: 3},
	})

	n, err := s.AutoSplit(context.Background(), 1<<20)
	if err != nil {
		t.Fatalf("AutoSplit: %v", err)
	}
	if n != 1 {
		t.Fatalf("AutoSplit splits = %d, want 1", n)
	}

	rows := snapshotByID(t, s)
	if len(rows) != 2 {
		t.Fatalf("range map after auto-split = %+v, want 2 ranges", rows)
	}
	left, right := rows[1], rows[2]
	if !bytes.Equal(left.End, []byte("m")) || left.Version != 4 {
		t.Fatalf("left range = %+v, want end m at version 4", left)
	}
	if !bytes.Equal(right.Start, []byte("m")) || right.End != nil || right.Version != 4 {
		t.Fatalf("right range = %+v, want [m, +inf) at version 4", right)
	}

	statsReqs := fake.statsRequests()
	if len(statsReqs) != 1 {
		t.Fatalf("RangeStats requests = %d, want 1", len(statsReqs))
	}
	if statsReqs[0].GetRangeId() != 1 || statsReqs[0].GetEpoch() != 3 {
		t.Fatalf("RangeStats request = %+v, want range 1 epoch 3", statsReqs[0])
	}
	splitReqs := fake.splitRequests()
	if len(splitReqs) != 1 {
		t.Fatalf("SplitRange requests = %d, want 1", len(splitReqs))
	}
	if got := splitReqs[0]; got.GetRangeId() != 1 || got.GetNewRangeId() != 2 ||
		got.GetEpoch() != 3 || !bytes.Equal(got.GetSplitKey(), []byte("m")) {
		t.Fatalf("split request = %+v, want {range 1, new 2, epoch 3, key m}", got)
	}
}

func TestV2MetaSchedulerAutoSplitSkipsBelowThreshold(t *testing.T) {
	addr, fake := startFakeKV(t, "", "")
	fake.setRangeStats(1, fakeRangeStats(addr, 1<<20-1, 1000, "m", nil, nil))

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: nil, Replicas: []string{addr}, Leader: addr, Version: 1},
	})

	n, err := s.AutoSplit(context.Background(), 1<<20)
	if err != nil {
		t.Fatalf("AutoSplit: %v", err)
	}
	if n != 0 {
		t.Fatalf("AutoSplit splits = %d, want 0 below threshold", n)
	}
	if got := len(fake.splitRequests()); got != 0 {
		t.Fatalf("SplitRange calls = %d, want 0", got)
	}
	if rows := snapshotByID(t, s); len(rows) != 1 || rows[1].Version != 1 {
		t.Fatalf("range map changed below threshold: %+v", rows)
	}
}

func TestV2MetaSchedulerAutoSplitSkipsEmptySplitKey(t *testing.T) {
	addr, fake := startFakeKV(t, "", "")
	fake.setRangeStats(1, fakeRangeStats(addr, 8<<20, 1000, "", nil, nil))

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: nil, Replicas: []string{addr}, Leader: addr, Version: 1},
	})

	n, err := s.AutoSplit(context.Background(), 1<<20)
	if err != nil {
		t.Fatalf("AutoSplit: %v", err)
	}
	if n != 0 {
		t.Fatalf("AutoSplit splits = %d, want 0 without a split key", n)
	}
	if got := len(fake.splitRequests()); got != 0 {
		t.Fatalf("SplitRange calls = %d, want 0", got)
	}
	if rows := snapshotByID(t, s); len(rows) != 1 {
		t.Fatalf("range map changed without a split key: %+v", rows)
	}
}

func TestV2MetaSchedulerAutoSplitRequiresLeader(t *testing.T) {
	s := &Server{
		cfg:     Config{Replicated: true},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		cluster: meta.New(),
	}
	n, err := s.AutoSplit(context.Background(), 1<<20)
	if err == nil {
		t.Fatalf("AutoSplit on a non-leader succeeded with %d splits, want error", n)
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("AutoSplit error code = %v (%v), want Unavailable", code, err)
	}
}

func TestV2MetaSchedulerAutoSplitFailedReplicaLeavesMapUnchanged(t *testing.T) {
	healthy, healthyFake := startFakeKV(t, "", "")
	stale, staleFake := startFakeKVWithStats(t, "stale split failure", "", "stale epoch")
	staleFake.setRangeStats(1, fakeRangeStats(stale, 0, 0, "", nil, nil))
	healthyFake.setRangeStats(1, fakeRangeStats(healthy, 8<<20, 1000, "m", nil, nil))

	replicas := []string{stale, healthy}
	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: nil, Replicas: replicas, Leader: healthy, Version: 7},
	})

	n, err := s.AutoSplit(context.Background(), 1<<20)
	if err == nil {
		t.Fatalf("AutoSplit succeeded despite a failing replica (splits=%d)", n)
	}
	if n != 0 {
		t.Fatalf("AutoSplit splits = %d, want 0 when a replica fails", n)
	}
	if !strings.Contains(err.Error(), stale) {
		t.Fatalf("AutoSplit error = %q, want failing replica %s", err, stale)
	}
	// The healthy replica was asked to split; the failure happened during the
	// fan-out, so the meta map must still describe the original range.
	if got := len(healthyFake.splitRequests()); got == 0 {
		t.Fatal("healthy replica was not asked to split")
	}
	if got := len(staleFake.splitRequests()); got == 0 {
		t.Fatal("stale replica was not asked to split; fan-out should include it")
	}
	rows := snapshotByID(t, s)
	if len(rows) != 1 {
		t.Fatalf("range map changed after failed fan-out: %+v", rows)
	}
	if got := rows[1]; got.Version != 7 || got.End != nil {
		t.Fatalf("range 1 changed after failed fan-out: %+v", got)
	}
}

func TestV2MetaSchedulerAutoSplitSkipsUnavailableStats(t *testing.T) {
	addr, fake := startFakeKVWithStats(t, "", "", "stats unavailable")

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: nil, Replicas: []string{addr}, Leader: addr, Version: 1},
	})

	n, err := s.AutoSplit(context.Background(), 1<<20)
	if err == nil {
		t.Fatalf("AutoSplit succeeded without stats (splits=%d)", n)
	}
	if n != 0 {
		t.Fatalf("AutoSplit splits = %d, want 0", n)
	}
	if got := len(fake.splitRequests()); got != 0 {
		t.Fatalf("SplitRange calls = %d, want 0 without stats", got)
	}
	if rows := snapshotByID(t, s); len(rows) != 1 {
		t.Fatalf("range map changed without stats: %+v", rows)
	}
}

func TestV2MetaSchedulerAutoSplitLoopStops(t *testing.T) {
	addr, fake := startFakeKV(t, "", "")
	fake.setRangeStats(1, fakeRangeStats(addr, 8<<20, 1000, "m", nil, nil))

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: nil, Replicas: []string{addr}, Leader: addr, Version: 1},
	})
	if err := s.StartAutoSplit(context.Background(), 10*time.Millisecond, 1<<20); err != nil {
		t.Fatalf("StartAutoSplit: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(snapshotByID(t, s)) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rows := snapshotByID(t, s); len(rows) != 2 {
		t.Fatalf("auto-split loop did not split the range: %+v", rows)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := s.StartAutoSplit(context.Background(), 10*time.Millisecond, 1<<20); err == nil {
		t.Fatal("StartAutoSplit after Stop succeeded, want error")
	}
}

func TestV2MetaSchedulerAutoMergeMergesAdjacentSmallRanges(t *testing.T) {
	addr1, kv1 := startFakeKV(t, "", "")
	addr2, kv2 := startFakeKV(t, "", "")
	replicas := []string{addr1, addr2}
	kv1.setRangeStats(1, fakeRangeStats(addr1, 1<<20, 100, "", nil, []byte("m")))
	kv2.setRangeStats(4, fakeRangeStats(addr2, 1<<20, 100, "", []byte("m"), []byte("t")))

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: []byte("m"), Replicas: replicas, Leader: addr1, Version: 2},
		{ID: 4, Start: []byte("m"), End: []byte("t"), Replicas: replicas, Leader: addr2, Version: 2},
		{ID: 9, Start: []byte("t"), End: nil, Replicas: replicas, Leader: addr1, Version: 1},
	})

	n, err := s.AutoMerge(context.Background(), 4<<20, 1<<10)
	if err != nil {
		t.Fatalf("AutoMerge: %v", err)
	}
	if n != 1 {
		t.Fatalf("AutoMerge merges = %d, want 1", n)
	}

	rows := snapshotByID(t, s)
	if len(rows) != 2 {
		t.Fatalf("range map after auto-merge = %+v, want 2 ranges", rows)
	}
	merged := rows[1]
	if !bytes.Equal(merged.Start, nil) || !bytes.Equal(merged.End, []byte("t")) {
		t.Fatalf("merged bounds = [%q, %q), want [-inf, t)", merged.Start, merged.End)
	}
	if merged.Version != 3 || merged.Leader != addr1 {
		t.Fatalf("merged range = %+v, want version 3 and leader %q", merged, addr1)
	}
	if _, ok := rows[4]; ok {
		t.Fatalf("right range 4 still present after auto-merge")
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
}

func TestV2MetaSchedulerAutoMergeSkipsLargeAndUnderfull(t *testing.T) {
	t.Run("combined too large", func(t *testing.T) {
		addr, fake := startFakeKV(t, "", "")
		fake.setRangeStats(1, fakeRangeStats(addr, 3<<20, 100, "", nil, []byte("m")))
		fake.setRangeStats(2, fakeRangeStats(addr, 3<<20, 100, "", []byte("m"), nil))

		s := newSchedulerServer(t, []meta.Range{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{addr}, Leader: addr, Version: 5},
			{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{addr}, Leader: addr, Version: 5},
		})

		n, err := s.AutoMerge(context.Background(), 4<<20, 1<<10)
		if err != nil {
			t.Fatalf("AutoMerge: %v", err)
		}
		if n != 0 {
			t.Fatalf("AutoMerge merges = %d, want 0 when combined size exceeds max", n)
		}
		if got := len(fake.mergeRequests()); got != 0 {
			t.Fatalf("MergeRanges calls = %d, want 0", got)
		}
		rows := snapshotByID(t, s)
		if len(rows) != 2 || rows[1].Version != 5 || rows[2].Version != 5 {
			t.Fatalf("range map changed for an oversized pair: %+v", rows)
		}
	})

	t.Run("underfull pair", func(t *testing.T) {
		addr, fake := startFakeKV(t, "", "")
		fake.setRangeStats(1, fakeRangeStats(addr, 512<<10, 10, "", nil, []byte("m")))
		fake.setRangeStats(2, fakeRangeStats(addr, 1<<20, 20, "", []byte("m"), nil))

		s := newSchedulerServer(t, []meta.Range{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{addr}, Leader: addr, Version: 3},
			{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{addr}, Leader: addr, Version: 3},
		})

		n, err := s.AutoMerge(context.Background(), 4<<20, 1<<20)
		if err != nil {
			t.Fatalf("AutoMerge: %v", err)
		}
		if n != 0 {
			t.Fatalf("AutoMerge merges = %d, want 0 when a range is underfull", n)
		}
		if got := len(fake.mergeRequests()); got != 0 {
			t.Fatalf("MergeRanges calls = %d, want 0", got)
		}
		if rows := snapshotByID(t, s); len(rows) != 2 {
			t.Fatalf("range map changed for an underfull pair: %+v", rows)
		}
	})
}

func TestV2MetaSchedulerAutoMergeFailedReplicaLeavesMapUnchanged(t *testing.T) {
	healthy, healthyFake := startFakeKV(t, "", "")
	stale, _ := startFakeKV(t, "", "injected merge failure")
	replicas := []string{healthy, stale}
	healthyFake.setRangeStats(1, fakeRangeStats(healthy, 1<<20, 100, "", nil, []byte("m")))
	healthyFake.setRangeStats(2, fakeRangeStats(healthy, 1<<20, 100, "", []byte("m"), nil))

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: []byte("m"), Replicas: replicas, Leader: healthy, Version: 6},
		{ID: 2, Start: []byte("m"), End: nil, Replicas: replicas, Leader: healthy, Version: 6},
	})

	n, err := s.AutoMerge(context.Background(), 4<<20, 1<<10)
	if err == nil {
		t.Fatalf("AutoMerge succeeded despite a failing replica (merges=%d)", n)
	}
	if n != 0 {
		t.Fatalf("AutoMerge merges = %d, want 0 when a replica fails", n)
	}
	if !strings.Contains(err.Error(), stale) || !strings.Contains(err.Error(), "1/2 replicas") {
		t.Fatalf("AutoMerge error = %q, want failing replica and failure count", err)
	}
	if got := len(healthyFake.mergeRequests()); got == 0 {
		t.Fatal("healthy replica was not asked to merge")
	}

	rows := snapshotByID(t, s)
	if len(rows) != 2 {
		t.Fatalf("range map changed after failed merge fan-out: %+v", rows)
	}
	if rows[1].End == nil || !bytes.Equal(rows[1].End, []byte("m")) || rows[1].Version != 6 {
		t.Fatalf("left range changed after failed fan-out: %+v", rows[1])
	}
	if rows[2].Version != 6 {
		t.Fatalf("right range changed after failed fan-out: %+v", rows[2])
	}
}

func TestV2MetaSchedulerAutoMergeRequiresLeader(t *testing.T) {
	s := &Server{
		cfg:     Config{Replicated: true},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		cluster: meta.New(),
	}
	n, err := s.AutoMerge(context.Background(), 1<<20, 1<<10)
	if err == nil {
		t.Fatalf("AutoMerge on a non-leader succeeded with %d merges, want error", n)
	}
	if n != 0 {
		t.Fatalf("AutoMerge merges = %d, want 0 on a non-leader", n)
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("AutoMerge error code = %v (%v), want Unavailable", code, err)
	}
}

func TestV2MetaSchedulerStartLifecycleValidationAndSingleLoop(t *testing.T) {
	addr, _ := startFakeKV(t, "", "")
	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: nil, Replicas: []string{addr}, Version: 1},
	})
	t.Cleanup(func() { _ = s.Stop() })
	ctx := context.Background()

	invalid := []struct {
		name     string
		interval time.Duration
		max      uint64
		min      uint64
	}{
		{name: "zero interval", interval: 0, max: 1 << 20, min: 1},
		{name: "zero max", interval: time.Hour, max: 0, min: 1},
		{name: "zero min", interval: time.Hour, max: 1 << 20, min: 0},
		{name: "min over max", interval: time.Hour, max: 1 << 20, min: 2 << 20},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.StartLifecycle(ctx, tc.interval, tc.max, tc.min); err == nil {
				t.Fatalf("StartLifecycle(%s) succeeded, want validation error", tc.name)
			}
		})
	}

	if err := s.StartLifecycle(ctx, time.Hour, 1<<20, 1<<10); err != nil {
		t.Fatalf("StartLifecycle: %v", err)
	}
	if err := s.StartLifecycle(ctx, time.Hour, 1<<20, 1<<10); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Fatalf("second StartLifecycle = %v, want already-running error", err)
	}
	if err := s.StartAutoSplit(ctx, time.Hour, 1<<20); err == nil ||
		!strings.Contains(err.Error(), "lifecycle already running") {
		t.Fatalf("StartAutoSplit alongside lifecycle = %v, want lifecycle-already-running error", err)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := s.StartLifecycle(ctx, time.Hour, 1<<20, 1<<10); err == nil ||
		!strings.Contains(err.Error(), "stopped") {
		t.Fatalf("StartLifecycle after Stop = %v, want stopped error", err)
	}
}

func TestV2MetaSchedulerLifecycleMergesAndStops(t *testing.T) {
	addr, fake := startFakeKV(t, "", "")
	fake.setRangeStats(1, fakeRangeStats(addr, 1<<10, 10, "", nil, []byte("m")))
	fake.setRangeStats(2, fakeRangeStats(addr, 1<<10, 10, "", []byte("m"), nil))

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{addr}, Leader: addr, Version: 1},
		{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{addr}, Leader: addr, Version: 1},
	})
	t.Cleanup(func() { _ = s.Stop() })

	if err := s.StartLifecycle(context.Background(), 10*time.Millisecond, 8<<20, 1<<10); err != nil {
		t.Fatalf("StartLifecycle: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(snapshotByID(t, s)) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows := snapshotByID(t, s)
	if len(rows) != 1 {
		t.Fatalf("lifecycle loop did not merge the small pair: %+v", rows)
	}
	if rows[1].Version != 2 || rows[1].End != nil {
		t.Fatalf("merged range = %+v, want [nil, nil) at version 2", rows[1])
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stoppedAt := len(fake.statsRequests())
	time.Sleep(60 * time.Millisecond)
	if got := len(fake.statsRequests()); got != stoppedAt {
		t.Fatalf("lifecycle loop still running after Stop: stats requests %d -> %d", stoppedAt, got)
	}
	if err := s.StartLifecycle(context.Background(), 10*time.Millisecond, 8<<20, 1<<10); err == nil {
		t.Fatal("StartLifecycle after Stop succeeded, want error")
	}
}

func TestV2MetaSchedulerLifecycleLeaderOnly(t *testing.T) {
	addr, fake := startFakeKV(t, "", "")
	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: nil, Replicas: []string{addr}, Version: 1},
	})
	// Replicated mode with no Raft node: requireLeader reports Unavailable,
	// exactly like a follower that has not discovered a leader yet.
	s.cfg.Replicated = true
	t.Cleanup(func() { _ = s.Stop() })

	if err := s.StartLifecycle(context.Background(), 10*time.Millisecond, 8<<20, 1<<10); err != nil {
		t.Fatalf("StartLifecycle: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if got := len(fake.statsRequests()); got != 0 {
		t.Fatalf("non-leader lifecycle loop made %d RangeStats calls, want 0", got)
	}
	if rows := snapshotByID(t, s); len(rows) != 1 || rows[1].Version != 1 {
		t.Fatalf("non-leader lifecycle loop changed the range map: %+v", rows)
	}
}
