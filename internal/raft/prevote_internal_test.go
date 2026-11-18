package raft

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/model"
)

// newPreVoteHandlerNode builds a follower with a 10-entry log in term 2 and a
// one-second election timeout, so the lease and log rules can be exercised
// deterministically.
func newPreVoteHandlerNode(t *testing.T, id string, voters, votersOld []string) *Node {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	n := &Node{
		cfg: Config{
			ID:                 id,
			ElectionTimeoutMin: time.Second,
			ElectionTimeoutMax: 2 * time.Second,
		},
		store:       store,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		role:        Follower,
		term:        5,
		voters:      append([]string(nil), voters...),
		votersOld:   append([]string(nil), votersOld...),
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

// TestHandlePreVoteRejectsWhileLeaderLeaseActive covers the lease rule and the
// no-persistence rule: with a recent leader contact the grant is refused, and
// after the contact ages past ElectionTimeoutMin it is granted, but the
// receiver's term and vote are unchanged in both cases.
func TestHandlePreVoteRejectsWhileLeaderLeaseActive(t *testing.T) {
	n := newPreVoteHandlerNode(t, "n1", []string{"n1", "n2", "n3"}, nil)
	req := &PreVoteRequest{Term: 6, CandidateID: "n2", LastLogIndex: 10, LastLogTerm: 2}

	n.lastLeaderContact = time.Now()
	resp, err := n.HandlePreVote(context.Background(), req)
	if err != nil {
		t.Fatalf("HandlePreVote(lease active): %v", err)
	}
	if resp.VoteGranted {
		t.Fatalf("pre-vote granted while the leader lease is active: %+v", resp)
	}
	if resp.Term != 5 || n.term != 5 {
		t.Fatalf("term = %d/%d after rejected pre-vote, want 5", resp.Term, n.term)
	}

	// Once the lease has expired, the same well-formed candidate is acceptable.
	n.lastLeaderContact = time.Now().Add(-2 * time.Second)
	resp, err = n.HandlePreVote(context.Background(), req)
	if err != nil {
		t.Fatalf("HandlePreVote(lease expired): %v", err)
	}
	if !resp.VoteGranted {
		t.Fatalf("pre-vote rejected after the leader lease expired: %+v", resp)
	}
	if n.term != 5 || n.votedFor != "" {
		t.Fatalf("pre-vote mutated term/vote to %d/%q", n.term, n.votedFor)
	}
	state, err := n.store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Term != 0 || state.VotedFor != "" {
		t.Fatalf("pre-vote persisted term/vote %d/%q, want none", state.Term, state.VotedFor)
	}
}

// TestHandlePreVoteDoesNotAdoptHigherTerm is the core anti-inflation check on
// the receiver side: a pre-vote carrying a much higher next term is either
// granted or refused on its merits, but the local term never moves and nothing
// is written to the durable store.
func TestHandlePreVoteDoesNotAdoptHigherTerm(t *testing.T) {
	n := newPreVoteHandlerNode(t, "n1", []string{"n1", "n2", "n3"}, nil)
	n.lastLeaderContact = time.Time{} // no lease to protect

	resp, err := n.HandlePreVote(context.Background(), &PreVoteRequest{
		Term:         42,
		CandidateID:  "n2",
		LastLogIndex: 10,
		LastLogTerm:  2,
	})
	if err != nil {
		t.Fatalf("HandlePreVote: %v", err)
	}
	if !resp.VoteGranted {
		t.Fatalf("up-to-date candidate rejected: %+v", resp)
	}
	if resp.Term != 5 || n.term != 5 {
		t.Fatalf("term after higher pre-vote = %d/%d, want 5", resp.Term, n.term)
	}
	state, err := n.store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Term != 0 {
		t.Fatalf("pre-vote persisted term %d, want no write", state.Term)
	}
}

// TestHandlePreVoteRequiresAtLeastAsUpToDateLog covers the log comparison,
// including the case where a candidate with a newer last term but a shorter log
// still wins and one with an older last term loses despite a longer log.
func TestHandlePreVoteRequiresAtLeastAsUpToDateLog(t *testing.T) {
	tests := []struct {
		name    string
		term    uint64
		index   uint64
		granted bool
	}{
		{name: "older last term, longer log", term: 1, index: 20, granted: false},
		{name: "same last term, shorter log", term: 2, index: 9, granted: false},
		{name: "same log", term: 2, index: 10, granted: true},
		{name: "same last term, longer log", term: 2, index: 11, granted: true},
		{name: "newer last term, shorter log", term: 3, index: 1, granted: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := newPreVoteHandlerNode(t, "n1", []string{"n1", "n2", "n3"}, nil)
			resp, err := n.HandlePreVote(context.Background(), &PreVoteRequest{
				Term:         6,
				CandidateID:  "n2",
				LastLogIndex: tc.index,
				LastLogTerm:  tc.term,
			})
			if err != nil {
				t.Fatalf("HandlePreVote: %v", err)
			}
			if resp.VoteGranted != tc.granted {
				t.Fatalf("VoteGranted = %v, want %v (candidate log %d/%d, local 10/2)",
					resp.VoteGranted, tc.granted, tc.index, tc.term)
			}
			if n.term != 5 {
				t.Fatalf("term after pre-vote = %d, want 5", n.term)
			}
		})
	}
}

// TestHandlePreVoteRejectsOutsideElectorateAndLearners keeps the membership
// rules: a candidate absent from the latest configuration is rejected, and a
// learner never answers a solicitation.
func TestHandlePreVoteRejectsOutsideElectorateAndLearners(t *testing.T) {
	n := newPreVoteHandlerNode(t, "n1", []string{"n1", "n2"}, nil)
	resp, err := n.HandlePreVote(context.Background(), &PreVoteRequest{
		Term: 6, CandidateID: "n9", LastLogIndex: 10, LastLogTerm: 2,
	})
	if err != nil {
		t.Fatalf("HandlePreVote: %v", err)
	}
	if resp.VoteGranted {
		t.Fatalf("pre-vote granted to a non-member candidate: %+v", resp)
	}

	learner := newPreVoteHandlerNode(t, "n3", []string{"n1", "n2"}, nil)
	learner.learners["n3"] = true
	resp, err = learner.HandlePreVote(context.Background(), &PreVoteRequest{
		Term: 6, CandidateID: "n1", LastLogIndex: 10, LastLogTerm: 2,
	})
	if err != nil {
		t.Fatalf("HandlePreVote(learner): %v", err)
	}
	if resp.VoteGranted {
		t.Fatalf("learner granted a pre-vote: %+v", resp)
	}
}

// preVoteCampaignTransport is a deterministic PreVote/Transport pair for the
// campaign tests. RequestVote always fails, so a candidate that starts a real
// election stays a candidate instead of becoming leader.
type preVoteCampaignTransport struct {
	mu          sync.Mutex
	granted     map[string]bool
	preVoteCall int
	voteCall    int
}

func (t *preVoteCampaignTransport) PreVote(_ context.Context, peer string, req *PreVoteRequest) (*PreVoteResponse, error) {
	t.mu.Lock()
	t.preVoteCall++
	granted := t.granted[peer]
	t.mu.Unlock()
	return &PreVoteResponse{Term: req.Term - 1, VoteGranted: granted}, nil
}

func (t *preVoteCampaignTransport) RequestVote(context.Context, string, *RequestVoteRequest) (*RequestVoteResponse, error) {
	t.mu.Lock()
	t.voteCall++
	t.mu.Unlock()
	return nil, errors.New("not used")
}

func (t *preVoteCampaignTransport) AppendEntries(context.Context, string, *AppendEntriesRequest) (*AppendEntriesResponse, error) {
	return nil, errors.New("not used")
}

func (t *preVoteCampaignTransport) setGranted(peers ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, peer := range peers {
		t.granted[peer] = true
	}
}

func (t *preVoteCampaignTransport) calls() (preVote, vote int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.preVoteCall, t.voteCall
}

func waitPreVote(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within 2s: %s", what)
}

// TestPreVoteJointQuorumGatesElection proves two things together: a joint
// configuration needs a pre-vote majority of both C_old and C_new before the
// term is incremented, and only then does the node start a real election at
// term+1.
func TestPreVoteJointQuorumGatesElection(t *testing.T) {
	n := newPreVoteHandlerNode(t, "n1",
		[]string{"n1", "n4", "n5"}, // C_new
		[]string{"n1", "n2", "n3"}) // C_old
	tr := &preVoteCampaignTransport{granted: map[string]bool{}}
	n.tr = tr

	// First round: C_new grants (n4, n5) but C_old does not (n2, n3).
	tr.setGranted("n4", "n5")
	n.beginPreVote()

	waitPreVote(t, "first pre-vote round to finish", func() bool {
		preVote, _ := tr.calls()
		n.mu.Lock()
		grants := len(n.preVotes)
		n.mu.Unlock()
		return preVote >= 4 && grants == 3 // self + n4 + n5
	})
	n.mu.Lock()
	term, role := n.term, n.role
	n.mu.Unlock()
	if term != 5 || role != Follower {
		t.Fatalf("C_new-only pre-vote majority advanced term/role to %d/%s", term, role)
	}
	state, err := n.store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Term != 0 {
		t.Fatalf("failed pre-vote persisted term %d", state.Term)
	}

	// Second round: add a C_old majority. The node must now campaign at term 6.
	tr.setGranted("n2", "n3")
	n.beginPreVote()
	waitPreVote(t, "real election after both pre-vote majorities", func() bool {
		n.mu.Lock()
		elected := n.term == 6 && n.role == Candidate && n.votedFor == "n1"
		n.mu.Unlock()
		_, vote := tr.calls()
		return elected && vote > 0
	})
	state, err = n.store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Term != 6 || state.VotedFor != "n1" {
		t.Fatalf("durable state after election = %d/%q, want 6/n1", state.Term, state.VotedFor)
	}
	if _, vote := tr.calls(); vote == 0 {
		t.Fatal("no RequestVote RPC was sent after the pre-vote quorum")
	}
}
