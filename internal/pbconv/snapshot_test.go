package pbconv

import (
	"reflect"
	"testing"

	"distributed-db/internal/raft"
)

func TestSnapshotMembershipRoundTrip(t *testing.T) {
	in := &raft.SnapshotMembership{
		Voters: []string{"a:1", "b:1"}, Learners: []string{"c:1"}, ConfigEpoch: 7,
	}
	pb := SnapshotMembershipToPB(in)
	if pb.GetConfigEpoch() != 7 || len(pb.GetVoters()) != 2 || len(pb.GetLearners()) != 1 {
		t.Fatalf("unexpected pb: %+v", pb)
	}
	out := SnapshotMembershipFromPB(pb)
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch: %+v != %+v", in, out)
	}
	// The converter must copy slices, not alias the Raft snapshot.
	pb.Voters[0] = "mutated"
	if in.Voters[0] == "mutated" {
		t.Fatal("converter aliased voters")
	}
}

func TestSnapshotMembershipNil(t *testing.T) {
	if SnapshotMembershipToPB(nil) != nil || SnapshotMembershipFromPB(nil) != nil {
		t.Fatal("nil membership must stay nil")
	}
}
