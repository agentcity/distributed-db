package raft

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"distributed-db/internal/model"
)

func voteTestNode(t *testing.T, id string, voters []string) *Node {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	n := &Node{
		cfg: Config{
			ID:                 id,
			ElectionTimeoutMin: time.Millisecond,
			ElectionTimeoutMax: 2 * time.Millisecond,
		},
		store:       store,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		role:        Follower,
		term:        2,
		voters:      append([]string(nil), voters...),
		learners:    map[string]bool{},
		configEpoch: 5,
		lastApplied: 10,
		commitIndex: 10,
	}
	for i := uint64(1); i <= 10; i++ {
		n.entries = append(n.entries, model.Entry{Term: 2, Index: i, Type: model.CommandNoop})
	}
	return n
}

// TestRemovedVoterStillGrantsVoteToCurrentCandidate covers the liveness
// deadlock after a joint change: the leader applies the final entry c_new,
// which removes it, and steps down. Peers that still have the joint
// configuration need a majority of c_old to elect the leader that will deliver
// that final commit, so a node that already applied the final entry must still
// answer their vote requests. The vote only counts if the candidate's own
// electoral configuration includes this node.
func TestRemovedVoterStillGrantsVoteToCurrentCandidate(t *testing.T) {
	n := voteTestNode(t, "n3", []string{"n1", "n2"})
	resp, err := n.HandleRequestVote(context.Background(), &RequestVoteRequest{
		Term:         3,
		CandidateID:  "n1",
		LastLogIndex: 10,
		LastLogTerm:  2,
	})
	if err != nil {
		t.Fatalf("HandleRequestVote: %v", err)
	}
	if !resp.VoteGranted {
		t.Fatalf("removed voter rejected a current candidate: %+v", resp)
	}
	if resp.Term != 3 || n.term != 3 {
		t.Fatalf("term after vote = %d/%d, want 3", resp.Term, n.term)
	}
}

// TestRemovedVoterRejectsCandidateOutsideItsElectorate keeps the other
// direction: answering vote requests does not mean endorsing a candidate that
// the local (final) configuration no longer contains.
func TestRemovedVoterRejectsCandidateOutsideItsElectorate(t *testing.T) {
	n := voteTestNode(t, "n3", []string{"n1", "n2"})
	resp, err := n.HandleRequestVote(context.Background(), &RequestVoteRequest{
		Term:         3,
		CandidateID:  "n9",
		LastLogIndex: 10,
		LastLogTerm:  2,
	})
	if err != nil {
		t.Fatalf("HandleRequestVote: %v", err)
	}
	if resp.VoteGranted {
		t.Fatalf("removed voter endorsed a non-member candidate: %+v", resp)
	}
}

// TestLearnerNeverGrantsVote keeps the learner rule from the removed-voter
// exemption: a learner's grant is never counted in a quorum, and it must not
// pretend to be a voter either.
func TestLearnerNeverGrantsVote(t *testing.T) {
	n := voteTestNode(t, "n3", []string{"n1", "n2"})
	n.learners["n3"] = true
	resp, err := n.HandleRequestVote(context.Background(), &RequestVoteRequest{
		Term:         3,
		CandidateID:  "n1",
		LastLogIndex: 10,
		LastLogTerm:  2,
	})
	if err != nil {
		t.Fatalf("HandleRequestVote: %v", err)
	}
	if resp.VoteGranted {
		t.Fatalf("learner granted a vote: %+v", resp)
	}
}
