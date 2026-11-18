package metaserver

import (
	"bytes"
	"context"
	"net"
	"testing"

	"distributed-db/internal/meta"
	pbv2 "distributed-db/internal/pb/v2"
)

func closedTestAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

func TestListRangesPopulatesLeaderFromLiveStatus(t *testing.T) {
	state := newFakeLeaderState()
	addr1, _ := startFakeKVWithState(t, state)
	addr2, _ := startFakeKVWithState(t, state)
	state.setLeader(1, addr1)

	s := newSchedulerServer(t, []meta.Range{{
		ID: 1, Start: nil, End: nil,
		Replicas: []string{addr1, addr2}, Version: 4,
	}})

	resp, err := s.ListRanges(context.Background(), &pbv2.ListRangesRequest{})
	if err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	if len(resp.GetRanges()) != 1 {
		t.Fatalf("ranges = %+v", resp.GetRanges())
	}
	if got := resp.GetRanges()[0].GetLeader(); got != addr1 {
		t.Fatalf("leader = %q, want live leader %q", got, addr1)
	}

	r := snapshotByID(t, s)[1]
	if r.Leader != addr1 || r.LeaderEpoch != 4 || r.LeaderStale() {
		t.Fatalf("recorded hint = %+v, want leader %q epoch 4", r, addr1)
	}
}

func TestListRangesDoesNotInventLeader(t *testing.T) {
	dead := closedTestAddr(t)
	s := newSchedulerServer(t, []meta.Range{{
		ID: 1, Start: nil, End: nil,
		Replicas: []string{dead}, Version: 2,
	}})

	resp, err := s.ListRanges(context.Background(), &pbv2.ListRangesRequest{})
	if err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	if got := resp.GetRanges()[0].GetLeader(); got != "" {
		t.Fatalf("leader = %q, want unknown (empty)", got)
	}
	if r := snapshotByID(t, s)[1]; r.Leader != "" || r.LeaderKnown() {
		t.Fatalf("unknown range got invented leader %+v", r)
	}
}

func TestRefreshRangeLeadersUpdatesStaleHint(t *testing.T) {
	state := newFakeLeaderState()
	addr1, _ := startFakeKVWithState(t, state)
	addr2, _ := startFakeKVWithState(t, state)
	state.setLeader(1, addr1)
	state.setLeader(2, addr2)

	s := newSchedulerServer(t, []meta.Range{
		{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{addr1, addr2}, Version: 3},
		// A hint that disagrees with live leadership is replaced.
		{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{addr1, addr2}, Leader: addr1, Version: 5},
	})

	updated, err := s.RefreshRangeLeaders(context.Background())
	if err != nil {
		t.Fatalf("RefreshRangeLeaders: %v", err)
	}
	if updated != 2 {
		t.Fatalf("updated = %d, want 2", updated)
	}
	rows := snapshotByID(t, s)
	if rows[1].Leader != addr1 || rows[1].LeaderEpoch != 3 {
		t.Fatalf("range 1 = %+v", rows[1])
	}
	if rows[2].Leader != addr2 || rows[2].LeaderEpoch != 5 {
		t.Fatalf("range 2 = %+v", rows[2])
	}
}

func TestListRangesHealthRefreshKeepsMapReadOnly(t *testing.T) {
	state := newFakeLeaderState()
	addr, _ := startFakeKVWithState(t, state)
	state.setLeader(1, addr)

	before := []meta.Range{{
		ID: 1, Start: nil, End: nil,
		Replicas: []string{addr}, Version: 7,
	}}
	s := newSchedulerServer(t, before)
	old := snapshotByID(t, s)[1]

	if _, err := s.ListRanges(context.Background(), &pbv2.ListRangesRequest{}); err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	got := snapshotByID(t, s)[1]
	if got.ID != old.ID || got.Version != old.Version ||
		!bytes.Equal(got.Start, old.Start) || !bytes.Equal(got.End, old.End) ||
		len(got.Replicas) != len(old.Replicas) {
		t.Fatalf("ListRanges changed the map: before %+v, after %+v", old, got)
	}
	if got.Leader != addr {
		t.Fatalf("leader = %q, want %q", got.Leader, addr)
	}
}

func TestQueryRangeLeaderRecordsHint(t *testing.T) {
	state := newFakeLeaderState()
	addr1, _ := startFakeKVWithState(t, state)
	addr2, _ := startFakeKVWithState(t, state)
	state.setLeader(1, addr2)

	s := newSchedulerServer(t, []meta.Range{{
		ID: 1, Start: nil, End: nil,
		Replicas: []string{addr1, addr2}, Version: 9,
	}})
	leader, err := s.queryRangeLeader(context.Background(), s.cluster.Snapshot()[0])
	if err != nil {
		t.Fatalf("queryRangeLeader: %v", err)
	}
	if leader != addr2 {
		t.Fatalf("leader = %q, want %q", leader, addr2)
	}
	if r := snapshotByID(t, s)[1]; r.Leader != addr2 || r.LeaderEpoch != 9 {
		t.Fatalf("query did not record hint: %+v", r)
	}
}
