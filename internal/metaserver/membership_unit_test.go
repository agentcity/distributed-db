package metaserver

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"distributed-db/internal/meta"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
)

// membershipFake is a minimal KVService replica for meta membership
// orchestration tests. Every fake reports itself as the leader of range 1, so
// the first replica in the descriptor's list is used as the leader.
type membershipFake struct {
	pbv2.UnimplementedKVServiceServer

	addr        string
	failAdd     bool
	failPromote bool
	failRemove  bool
}

func (f *membershipFake) Status(context.Context, *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	return &pbv2.StatusResponse{
		Id: f.addr, Role: "leader", Leader: f.addr,
		CommitIndex: 5, LastApplied: 5, RangeId: 1,
	}, nil
}

func (f *membershipFake) AddReplica(context.Context, *pbv2.AddReplicaRequest) (*pbv2.AddReplicaResponse, error) {
	return &pbv2.AddReplicaResponse{Success: true}, nil
}

func (f *membershipFake) AddLearner(context.Context, *pbv2.AddLearnerRequest) (*pbv2.MembershipResponse, error) {
	if f.failAdd {
		return &pbv2.MembershipResponse{Leader: f.addr, Error: "injected add failure"}, nil
	}
	return &pbv2.MembershipResponse{Success: true, Leader: f.addr}, nil
}

func (f *membershipFake) PromoteReplica(context.Context, *pbv2.PromoteReplicaRequest) (*pbv2.MembershipResponse, error) {
	if f.failPromote {
		return &pbv2.MembershipResponse{Leader: f.addr, Error: "injected promote failure"}, nil
	}
	return &pbv2.MembershipResponse{Success: true, Leader: f.addr}, nil
}

func (f *membershipFake) RemoveVoter(context.Context, *pbv2.RemoveVoterRequest) (*pbv2.MembershipResponse, error) {
	if f.failRemove {
		return &pbv2.MembershipResponse{Leader: f.addr, Error: "injected remove failure"}, nil
	}
	return &pbv2.MembershipResponse{Success: true, Leader: f.addr}, nil
}

func (f *membershipFake) RemoveReplica(context.Context, *pbv2.RemoveReplicaRequest) (*pbv2.RemoveReplicaResponse, error) {
	return &pbv2.RemoveReplicaResponse{Success: true}, nil
}

func (f *membershipFake) UpdateRangeEpoch(context.Context, *pbv2.UpdateRangeEpochRequest) (*pbv2.UpdateRangeEpochResponse, error) {
	return &pbv2.UpdateRangeEpochResponse{Success: true}, nil
}

func startMembershipFake(t *testing.T, fake *membershipFake) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake.addr = lis.Addr().String()
	srv := grpc.NewServer()
	pbv2.RegisterKVServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return fake.addr
}

func newMembershipMeta(t *testing.T, addrs []string) *Server {
	t.Helper()
	s, err := New(Config{
		Addr: "127.0.0.1:0",
		Ranges: []meta.Range{{
			ID: 1, Start: nil, End: nil,
			Replicas: append([]string(nil), addrs...),
			Version:  1,
		}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("metaserver.New: %v", err)
	}
	return s
}

func TestMetaAddReplicaUpdatesMapOnlyOnSuccess(t *testing.T) {
	ctx := context.Background()
	// Three initial replicas plus a target. The first fake is the leader.
	fakes := make([]*membershipFake, 4)
	addrs := make([]string, 4)
	for i := range fakes {
		fakes[i] = &membershipFake{}
		addrs[i] = startMembershipFake(t, fakes[i])
	}
	target := addrs[3]

	ok := newMembershipMeta(t, addrs[:3])
	if err := ok.AddReplica(ctx, 1, target); err != nil {
		t.Fatalf("AddReplica: %v", err)
	}
	r := ok.Cluster().Snapshot()[0]
	if len(r.Replicas) != 4 || r.Version != 2 {
		t.Fatalf("range after success = replicas %v version %d, want 4 and 2", r.Replicas, r.Version)
	}

	failures := newMembershipMeta(t, addrs[:3])
	fakes[0].failAdd = true
	failCtx, failCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer failCancel()
	if err := failures.AddReplica(failCtx, 1, target); err == nil {
		t.Fatal("AddReplica succeeded with a failing AddLearner")
	}
	if got := failures.Cluster().Snapshot()[0].Replicas; len(got) != 3 {
		t.Fatalf("replicas changed on failed add: %v", got)
	}
	fakes[0].failAdd = false
}

func TestMetaRemoveReplicaUpdatesMapOnlyOnSuccess(t *testing.T) {
	ctx := context.Background()
	fakes := make([]*membershipFake, 4)
	addrs := make([]string, 4)
	for i := range fakes {
		fakes[i] = &membershipFake{}
		addrs[i] = startMembershipFake(t, fakes[i])
	}
	victim := addrs[3]

	ok := newMembershipMeta(t, addrs)
	if err := ok.RemoveReplica(ctx, 1, victim); err != nil {
		t.Fatalf("RemoveReplica: %v", err)
	}
	r := ok.Cluster().Snapshot()[0]
	if len(r.Replicas) != 3 || r.Version != 2 {
		t.Fatalf("range after success = replicas %v version %d, want 3 and 2", r.Replicas, r.Version)
	}
	for _, addr := range r.Replicas {
		if addr == victim {
			t.Fatalf("removed replica %s still present: %v", victim, r.Replicas)
		}
	}

	failures := newMembershipMeta(t, addrs)
	fakes[0].failRemove = true
	failCtx, failCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer failCancel()
	if err := failures.RemoveReplica(failCtx, 1, victim); err == nil {
		t.Fatal("RemoveReplica succeeded with a failing RemoveVoter")
	}
	if got := failures.Cluster().Snapshot()[0].Replicas; len(got) != 4 {
		t.Fatalf("replicas changed on failed remove: %v", got)
	}
}

func TestMetaRemoveReplicaRejectsLastReplica(t *testing.T) {
	addrs := []string{startMembershipFake(t, &membershipFake{})}
	s := newMembershipMeta(t, addrs)
	err := s.RemoveReplica(context.Background(), 1, addrs[0])
	if err == nil {
		t.Fatal("RemoveReplica(last) succeeded, want an error")
	}
	if got := s.Cluster().Snapshot()[0].Replicas; len(got) != 1 {
		t.Fatalf("replicas changed after rejected remove: %v", got)
	}
}
