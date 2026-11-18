package v2client

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"

	pbv2 "distributed-db/internal/pb/v2"
)

// healthKV is a KVServiceClient stub used only for Status probes. Embedding the
// interface keeps the type small; any other RPC panics, which makes an
// accidental data-path call in a read-only test obvious.
type healthKV struct {
	pbv2.KVServiceClient

	addr      string
	role      string
	leader    string
	voters    []string
	learners  []string
	learner   bool
	epoch     uint64
	err       error
	statusNum *atomic.Int64
}

func (f *healthKV) Status(context.Context, *pbv2.StatusRequest, ...grpc.CallOption) (*pbv2.StatusResponse, error) {
	if f.statusNum != nil {
		f.statusNum.Add(1)
	}
	if f.err != nil {
		return nil, f.err
	}
	return &pbv2.StatusResponse{
		Id: f.addr, Role: f.role, Leader: f.leader, Epoch: f.epoch,
		Voters:   append([]string(nil), f.voters...),
		Learners: append([]string(nil), f.learners...),
		Learner:  f.learner,
	}, nil
}

func healthTestClient(t *testing.T, leader string, replicas []string) (*Client, *fakeMeta) {
	t.Helper()
	fake := newFakeMeta("meta1")
	fake.setListRanges(func() ([]*pbv2.RangeInfo, error) {
		return []*pbv2.RangeInfo{{
			Id: 1, Replicas: append([]string(nil), replicas...),
			Leader: leader, Version: 4,
		}}, nil
	})
	return newTestClient(fake), fake
}

func TestListRangesWithHealthFillsLiveView(t *testing.T) {
	c, _ := healthTestClient(t, "", []string{"kv1", "kv2"})
	c.clients["kv1"] = &healthKV{
		addr: "kv1", role: "leader", leader: "kv1", epoch: 4,
		voters: []string{"kv1", "kv2"},
	}
	c.clients["kv2"] = &healthKV{
		addr: "kv2", role: "follower", leader: "kv1", epoch: 4,
		voters: []string{"kv1", "kv2"},
	}

	got, err := c.ListRangesWithHealth(context.Background())
	if err != nil {
		t.Fatalf("ListRangesWithHealth: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ranges = %+v", got)
	}
	r := got[0]
	if r.Leader != "kv1" || !r.LeaderKnown {
		t.Fatalf("leader = %q known=%v, want kv1", r.Leader, r.LeaderKnown)
	}
	if r.ReplicaCount != 2 || r.Epoch != 4 || r.Stale {
		t.Fatalf("health = %+v", r)
	}
	if want := []string{"kv1", "kv2"}; !reflect.DeepEqual(r.LiveVoters, want) {
		t.Fatalf("live voters = %v, want %v", r.LiveVoters, want)
	}
	// The wire fields are unchanged.
	if r.ID != 1 || r.Version != 4 {
		t.Fatalf("descriptor = %+v", r)
	}
}

func TestListRangesWithHealthMarksStaleHint(t *testing.T) {
	c, _ := healthTestClient(t, "kv2", []string{"kv1", "kv2"})
	c.clients["kv1"] = &healthKV{
		addr: "kv1", role: "leader", leader: "kv1", epoch: 4,
		voters: []string{"kv1", "kv2"},
	}
	c.clients["kv2"] = &healthKV{
		addr: "kv2", role: "follower", leader: "kv1", epoch: 4,
		voters: []string{"kv1", "kv2"},
	}

	got, err := c.ListRangesWithHealth(context.Background())
	if err != nil {
		t.Fatalf("ListRangesWithHealth: %v", err)
	}
	r := got[0]
	if r.Leader != "kv1" || !r.LeaderKnown || !r.Stale {
		t.Fatalf("health = %+v, want live leader kv1 and stale=true", r)
	}
}

func TestListRangesWithHealthUnreachableKeepsUnknown(t *testing.T) {
	c, _ := healthTestClient(t, "", []string{"kv1", "kv2"})
	c.clients["kv1"] = &healthKV{addr: "kv1", err: errors.New("down")}
	c.clients["kv2"] = &healthKV{addr: "kv2", err: errors.New("down")}

	got, err := c.ListRangesWithHealth(context.Background())
	if err != nil {
		t.Fatalf("ListRangesWithHealth: %v", err)
	}
	r := got[0]
	if r.Leader != "" || r.LeaderKnown {
		t.Fatalf("unknown leader was invented: %+v", r)
	}
	if r.LiveVoters == nil || len(r.LiveVoters) != 0 {
		t.Fatalf("live voters = %v, want empty non-nil", r.LiveVoters)
	}
	if r.Stale || r.ReplicaCount != 2 || r.Epoch != 4 {
		t.Fatalf("health = %+v", r)
	}
}

func TestListRangesPlainDoesNotProbe(t *testing.T) {
	var calls atomic.Int64
	c, _ := healthTestClient(t, "kv1", []string{"kv1"})
	c.clients["kv1"] = &healthKV{
		addr: "kv1", role: "leader", leader: "kv1", epoch: 4,
		statusNum: &calls,
	}

	if _, err := c.ListRanges(context.Background()); err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("plain ListRanges issued %d Status probes, want 0", calls.Load())
	}
	if _, err := c.ListRangesWithHealth(context.Background()); err != nil {
		t.Fatalf("ListRangesWithHealth: %v", err)
	}
	if calls.Load() == 0 {
		t.Fatal("ListRangesWithHealth did not probe any replica")
	}
}
