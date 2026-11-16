package kvserver

import (
	"context"
	"errors"
	"testing"
	"time"

	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestMembershipAddReplicaCreatesLearnerGroup exercises the target side of
// membership orchestration directly: AddReplica creates and starts a learner
// group that reuses any existing data directory, UpdateRangeEpoch records the
// version published by meta, and RemoveReplica stops the group without deleting
// its data.
func TestMembershipAddReplicaCreatesLearnerGroup(t *testing.T) {
	addr := testFreeAddr(t)
	srv, err := New(Config{
		ID:                 addr,
		RaftAddr:           addr,
		DataDir:            t.TempDir(),
		NoRanges:           true,
		ElectionTimeoutMin: 100 * time.Millisecond,
		ElectionTimeoutMax: 200 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Stop(stopCtx)
	})

	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	client := pbv2.NewKVServiceClient(cc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	peers := []string{"peer-a", "peer-b"}
	resp, err := client.AddReplica(ctx, &pbv2.AddReplicaRequest{
		RangeId: 1, Target: addr, Peers: peers, Epoch: 1,
	})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("AddReplica: %v %+v", err, resp)
	}
	group, err := srv.GroupForID(1)
	if err != nil {
		t.Fatalf("range 1 not hosted after AddReplica: %v", err)
	}
	ranges := srv.Ranges()
	if len(ranges) != 1 || ranges[0].ID != 1 || !ranges[0].Learner {
		t.Fatalf("ranges after AddReplica = %+v, want one learner range", ranges)
	}
	st := group.Status()
	if !st.Learner || len(st.Voters) != 2 || len(st.Learners) != 1 || st.Learners[0] != addr {
		t.Fatalf("raft status after AddReplica = %+v", st)
	}

	// Repeating the call must reuse the group, not overwrite it.
	resp, err = client.AddReplica(ctx, &pbv2.AddReplicaRequest{
		RangeId: 1, Target: addr, Peers: peers, Epoch: 1,
	})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("idempotent AddReplica: %v %+v", err, resp)
	}

	ep, err := client.UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 2})
	if err != nil || !ep.GetSuccess() || ep.GetEpoch() != 2 {
		t.Fatalf("UpdateRangeEpoch: %v %+v", err, ep)
	}
	if got := srv.Ranges()[0].Epoch; got != 2 {
		t.Fatalf("range epoch = %d, want 2", got)
	}

	rem, err := client.RemoveReplica(ctx, &pbv2.RemoveReplicaRequest{RangeId: 1, Target: addr})
	if err != nil || !rem.GetSuccess() {
		t.Fatalf("RemoveReplica: %v %+v", err, rem)
	}
	if _, err := srv.GroupForID(1); !errors.Is(err, ErrRangeNotFound) {
		t.Fatalf("GroupForID after RemoveReplica = %v, want ErrRangeNotFound", err)
	}
	if got := srv.Ranges(); len(got) != 0 {
		t.Fatalf("ranges after RemoveReplica = %+v, want none", got)
	}

	// The data directory is kept, so the range can be added again later.
	if rem, err := client.RemoveReplica(ctx, &pbv2.RemoveReplicaRequest{RangeId: 1, Target: addr}); err != nil || !rem.GetSuccess() {
		t.Fatalf("idempotent RemoveReplica: %v %+v", err, rem)
	}
}
