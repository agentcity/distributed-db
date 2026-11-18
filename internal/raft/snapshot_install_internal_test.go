package raft

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"distributed-db/internal/model"
)

func staleSnapshotNode() *Node {
	n := &Node{
		cfg: Config{
			ID:                 "n1",
			ElectionTimeoutMin: time.Millisecond,
			ElectionTimeoutMax: 2 * time.Millisecond,
		},
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		role:        Follower,
		term:        2,
		voters:      []string{"n1", "n2"},
		learners:    map[string]bool{},
		configEpoch: 5,
		snapshot:    Snapshot{Index: 10, Term: 1},
		lastApplied: 10,
		commitIndex: 10,
	}
	for i := uint64(11); i <= 20; i++ {
		n.entries = append(n.entries, model.Entry{Term: 2, Index: i, Type: model.CommandNoop})
	}
	return n
}

func staleMembershipSnapshot(index uint64) Snapshot {
	return Snapshot{
		Index: index,
		Term:  2,
		Membership: &SnapshotMembership{
			Voters:      []string{"n1", "n2"},
			ConfigEpoch: 3,
		},
	}
}

// TestInstallSnapshotStaleMembershipCoveredByLog verifies that a follower whose
// configuration is newer than a snapshot's membership acknowledges the snapshot
// when its log already reaches the snapshot index. Returning an error left the
// leader retrying the same rejected snapshot forever instead of catching the
// follower up with AppendEntries.
func TestInstallSnapshotStaleMembershipCoveredByLog(t *testing.T) {
	n := staleSnapshotNode()
	resp, err := n.HandleInstallSnapshot(context.Background(), &InstallSnapshotRequest{
		Term:     2,
		LeaderID: "n2",
		Snapshot: staleMembershipSnapshot(15),
	})
	if err != nil {
		t.Fatalf("HandleInstallSnapshot(stale covered) = %v", err)
	}
	if resp == nil || !resp.Success || resp.LastIndex != 15 {
		t.Fatalf("HandleInstallSnapshot(stale covered) = %+v, want success at index 15", resp)
	}
	// The rejected snapshot must not have rolled the log or configuration back.
	if n.snapshot.Index != 10 || n.configEpoch != 5 || n.lastIndexLocked() != 20 {
		t.Fatalf("state changed after covered stale snapshot: snapshot=%d epoch=%d last=%d",
			n.snapshot.Index, n.configEpoch, n.lastIndexLocked())
	}
}

// TestInstallSnapshotStaleMembershipBeyondLogRejects verifies the other side:
// when the follower does not have the snapshot index, a stale membership is
// still refused because the payload is required and cannot be skipped.
func TestInstallSnapshotStaleMembershipBeyondLogRejects(t *testing.T) {
	n := staleSnapshotNode()
	_, err := n.HandleInstallSnapshot(context.Background(), &InstallSnapshotRequest{
		Term:     2,
		LeaderID: "n2",
		Snapshot: staleMembershipSnapshot(25),
	})
	if !errors.Is(err, ErrStaleSnapshotMembership) {
		t.Fatalf("HandleInstallSnapshot(stale beyond log) = %v, want ErrStaleSnapshotMembership", err)
	}
	if n.snapshot.Index != 10 || n.configEpoch != 5 {
		t.Fatalf("state changed after rejected stale snapshot: snapshot=%d epoch=%d", n.snapshot.Index, n.configEpoch)
	}
}
