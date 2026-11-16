package metaserver

import (
	"context"
	"strings"
	"testing"

	"distributed-db/internal/meta"
	pbv2 "distributed-db/internal/pb/v2"
)

func leaderTestRanges(replicas []string) []meta.Range {
	return []meta.Range{
		{ID: 1, Start: nil, End: []byte("m"), Replicas: replicas, Version: 1},
		{ID: 2, Start: []byte("m"), End: nil, Replicas: replicas, Version: 1},
	}
}

func collectTransfers(fakes ...*fakeKV) []*pbv2.TransferLeaderRequest {
	var out []*pbv2.TransferLeaderRequest
	for _, f := range fakes {
		out = append(out, f.transferRequests()...)
	}
	return out
}

func TestRebalanceLeadersMovesOneOfTwoLeaders(t *testing.T) {
	state := newFakeLeaderState()
	addr1, kv1 := startFakeKVWithState(t, state)
	addr2, kv2 := startFakeKVWithState(t, state)
	addr3, kv3 := startFakeKVWithState(t, state)
	replicas := []string{addr1, addr2, addr3}
	state.setLeader(1, addr1)
	state.setLeader(2, addr1)

	s := newSchedulerServer(t, leaderTestRanges(replicas))
	moved, err := s.RebalanceLeaders(context.Background())
	if err != nil {
		t.Fatalf("RebalanceLeaders: %v", err)
	}
	if moved != 1 {
		t.Fatalf("RebalanceLeaders moved %d leaders, want 1", moved)
	}

	transfers := collectTransfers(kv1, kv2, kv3)
	if len(transfers) != 1 {
		t.Fatalf("transfer requests = %d, want 1: %+v", len(transfers), transfers)
	}
	req := transfers[0]
	if req.GetRangeId() != 1 && req.GetRangeId() != 2 {
		t.Fatalf("transfer range = %d, want 1 or 2", req.GetRangeId())
	}
	if req.GetTarget() == addr1 || req.GetTarget() == "" {
		t.Fatalf("transfer target = %q, want a replica other than the donor %s", req.GetTarget(), addr1)
	}
	if req.GetEpoch() != 1 {
		t.Fatalf("transfer epoch = %d, want range version 1", req.GetEpoch())
	}
	if got := state.leaderFor(req.GetRangeId()); got != req.GetTarget() {
		t.Fatalf("range %d leader = %q, want target %q", req.GetRangeId(), got, req.GetTarget())
	}

	// One leader remains on the donor and the other moved to a new node.
	leaders := []string{state.leaderFor(1), state.leaderFor(2)}
	counts := make(map[string]int, len(leaders))
	for _, leader := range leaders {
		counts[leader]++
	}
	if counts[addr1] != 1 {
		t.Fatalf("leader counts after rebalance = %v, want one leader on %s", counts, addr1)
	}
	if counts[req.GetTarget()] != 1 {
		t.Fatalf("leader counts after rebalance = %v, want one leader on %s", counts, req.GetTarget())
	}
}

func TestRebalanceLeadersSkipsFailedTransfer(t *testing.T) {
	state := newFakeLeaderState()
	addr1, kv1 := startFakeKVWithState(t, state)
	addr2, kv2 := startFakeKVWithState(t, state)
	addr3, kv3 := startFakeKVWithState(t, state)
	replicas := []string{addr1, addr2, addr3}
	state.setLeader(1, addr1)
	state.setLeader(2, addr1)
	// The donor/leader rejects both handovers (for example because the target
	// is not caught up). The scheduler must skip them and keep the counts.
	kv1.mu.Lock()
	kv1.transferErr = "target not caught up"
	kv1.mu.Unlock()

	s := newSchedulerServer(t, leaderTestRanges(replicas))
	moved, err := s.RebalanceLeaders(context.Background())
	if err == nil || !strings.Contains(err.Error(), "target not caught up") {
		t.Fatalf("RebalanceLeaders error = %v, want target-not-caught-up detail", err)
	}
	if moved != 0 {
		t.Fatalf("RebalanceLeaders moved %d leaders despite rejections, want 0", moved)
	}
	if got := state.leaderFor(1); got != addr1 {
		t.Fatalf("range 1 leader = %q, want unchanged %q", got, addr1)
	}
	if got := state.leaderFor(2); got != addr1 {
		t.Fatalf("range 2 leader = %q, want unchanged %q", got, addr1)
	}
	if len(kv1.transferRequests()) == 0 {
		t.Fatal("no transfer was attempted on the loaded leader")
	}
	if got := collectTransfers(kv2, kv3); len(got) != 0 {
		t.Fatalf("followers received %d transfer requests, want 0", len(got))
	}
}

func TestRebalanceLeadersNoopWhenBalanced(t *testing.T) {
	state := newFakeLeaderState()
	addr1, kv1 := startFakeKVWithState(t, state)
	addr2, kv2 := startFakeKVWithState(t, state)
	addr3, kv3 := startFakeKVWithState(t, state)
	replicas := []string{addr1, addr2, addr3}
	state.setLeader(1, addr1)
	state.setLeader(2, addr2)

	s := newSchedulerServer(t, leaderTestRanges(replicas))
	moved, err := s.RebalanceLeaders(context.Background())
	if err != nil {
		t.Fatalf("RebalanceLeaders: %v", err)
	}
	if moved != 0 {
		t.Fatalf("RebalanceLeaders moved %d leaders of a balanced map, want 0", moved)
	}
	if got := collectTransfers(kv1, kv2, kv3); len(got) != 0 {
		t.Fatalf("balanced map issued %d transfer requests, want 0", len(got))
	}
}

func TestRebalanceLeadersRequiresMetaLeader(t *testing.T) {
	// A replicated server that has not started has no meta Raft node; the
	// scheduler must refuse instead of touching ranges in a passive replica.
	s, err := New(Config{
		Addr:       "127.0.0.1:0",
		DataDir:    t.TempDir(),
		Replicated: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.RebalanceLeaders(context.Background()); err == nil {
		t.Fatal("RebalanceLeaders succeeded without a meta leader")
	}
}
