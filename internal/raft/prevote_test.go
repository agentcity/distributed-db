package raft_test

import (
	"testing"
	"time"

	"distributed-db/internal/model"
)

// isolateBothWays blocks every link in both directions between id and peers.
// rafttest.Partition is one-way, so a fully isolated node needs both calls.
func isolateBothWays(c *testCluster, id string, peers []string) {
	c.net.Partition(id, peers...)
	for _, peer := range peers {
		c.net.Partition(peer, id)
	}
}

// TestIsolatedStaleNodeCannotInflateTermOrDisruptLeader covers the main
// PreVote protection: a follower that is cut off in both directions keeps
// timing out, but it cannot persist a higher term without a pre-vote quorum.
// When it is healed it is both stale (its log missed a commit) and at the old
// term, so it must neither unseat the leader nor force a term change.
func TestIsolatedStaleNodeCannotInflateTermOrDisruptLeader(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 100*time.Millisecond, 200*time.Millisecond, 20*time.Millisecond)
	leader, leaderID := c.waitLeader(c.ids, testWait)
	c.proposeWithRetry(c.ids, model.Entry{Type: model.CommandPut, Key: "before", Value: []byte("partition")}, testWait)
	c.requireValue(c.ids, "before", "partition", testWait)

	leaderTerm := leader.Status().Term
	eventually(t, testWait, "all nodes adopt the leader term", func() bool {
		for _, id := range c.ids {
			if node := c.nodes[id]; node == nil || node.Status().Term != leaderTerm {
				return false
			}
		}
		return true
	})

	followerIDs := peersExcept(c.ids, leaderID)
	isolated := followerIDs[0]
	majority := peersExcept(c.ids, isolated)
	preVotesBefore := c.transports[isolated].preVotes.Load()
	grantsBefore := c.transports[isolated].preVoteGrants.Load()

	isolateBothWays(c, isolated, majority)

	// Keep the majority busy while the victim is cut off: its log falls behind,
	// so even after healing it must lose the log comparison.
	c.proposeWithRetry(majority, model.Entry{Type: model.CommandPut, Key: "while-isolated", Value: []byte("yes")}, testWait)

	// Several election timeouts: every pre-vote attempt is unanswered, so the
	// victim must not advance its term at all.
	time.Sleep(600 * time.Millisecond)

	victim := c.nodes[isolated]
	if got := victim.Status().Term; got != leaderTerm {
		t.Fatalf("isolated node term = %d, want %d (pre-vote must not persist a term)", got, leaderTerm)
	}
	if victim.IsLeader() {
		t.Fatalf("isolated node %s became leader", isolated)
	}
	if !leader.IsLeader() {
		t.Fatalf("leader %s stepped down while a follower was isolated", leaderID)
	}
	if got := leader.Status().Term; got != leaderTerm {
		t.Fatalf("leader term = %d, want %d while the stale node is isolated", got, leaderTerm)
	}
	preVotes, grants := c.transports[isolated].preVotes.Load(), c.transports[isolated].preVoteGrants.Load()
	if preVotes == preVotesBefore {
		t.Fatal("isolated node never attempted a pre-vote")
	}
	if grants != grantsBefore {
		t.Fatalf("isolated node received %d new pre-vote grants without a reachable quorum", grants-grantsBefore)
	}

	// Heal and let the stale node campaign against a live leader. Its equal
	// (not higher) term and shorter log must leave the leader in place.
	c.net.Heal(isolated)
	time.Sleep(400 * time.Millisecond)

	if !leader.IsLeader() {
		t.Fatalf("leader %s lost leadership after the stale node healed", leaderID)
	}
	if got := leader.Status().Term; got != leaderTerm {
		t.Fatalf("leader term after heal = %d, want %d", got, leaderTerm)
	}
	if got := victim.Status().Term; got != leaderTerm {
		t.Fatalf("healed stale node term = %d, want %d", got, leaderTerm)
	}

	// It must catch up and the cluster must keep serving writes.
	c.requireValue(c.ids, "while-isolated", "yes", testWait)
	c.proposeWithRetry(c.ids, model.Entry{Type: model.CommandPut, Key: "after", Value: []byte("heal")}, testWait)
	c.requireValue(c.ids, "after", "heal", testWait)
}

// TestRemovedNodeCannotInflateTermOrDisruptLeader covers the removed-node
// half of the term-inflation problem. A voter is isolated and removed while it
// still believes it belongs to the configuration. It cannot advance its term
// while isolated; after healing, the remaining voters reject its pre-votes
// because it is no longer in their electorate, so the live leader and its term
// survive until the victim learns the final configuration.
func TestRemovedNodeCannotInflateTermOrDisruptLeader(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 100*time.Millisecond, 200*time.Millisecond, 20*time.Millisecond)
	leader, leaderID := c.waitLeader(c.ids, testWait)

	victim := peersExcept(c.ids, leaderID)[0]
	majority := peersExcept(c.ids, victim)
	isolateBothWays(c, victim, majority)

	// The remaining majority removes the isolated voter: both the joint entry
	// and the final C_new entry commit without it.
	c.removeVoterWithRetry(victim)
	leaderTerm := leader.Status().Term

	// The removed node has not learned about the change. It keeps timing out
	// against its old configuration but cannot persist a higher term.
	time.Sleep(500 * time.Millisecond)
	if got := c.nodes[victim].Status().Term; got != leaderTerm {
		t.Fatalf("removed node term = %d, want %d while isolated", got, leaderTerm)
	}
	if c.nodes[victim].IsLeader() {
		t.Fatalf("removed node %s became leader while isolated", victim)
	}
	grantsBefore := c.transports[victim].preVoteGrants.Load()

	c.net.Heal(victim)
	// Its pre-votes can reach the cluster now, but the peers' electorate no
	// longer contains the victim, so none may be granted and no term changes.
	time.Sleep(400 * time.Millisecond)

	if got := c.nodes[victim].Status().Term; got != leaderTerm {
		t.Fatalf("removed node term after heal = %d, want %d", got, leaderTerm)
	}
	if !leader.IsLeader() {
		t.Fatalf("leader %s was unseated by a removed node", leaderID)
	}
	if got := leader.Status().Term; got != leaderTerm {
		t.Fatalf("leader term after removed node healed = %d, want %d", got, leaderTerm)
	}
	if grants := c.transports[victim].preVoteGrants.Load(); grants != grantsBefore {
		t.Fatalf("peers granted %d pre-votes to a removed node", grants-grantsBefore)
	}
	if c.nodes[victim].IsLeader() {
		t.Fatalf("removed node %s became leader after the partition healed", victim)
	}

	// The removed node is no longer a replication target, so it may never
	// learn the final configuration; it must remain a harmless follower at the
	// old term while the majority keeps serving writes.
	c.proposeWithRetry(majority, model.Entry{Type: model.CommandPut, Key: "removed", Value: []byte("clean")}, testWait)
	c.requireValue(majority, "removed", "clean", testWait)
	if got := c.nodes[victim].Status().Term; got != leaderTerm {
		t.Fatalf("removed node term after writes = %d, want %d", got, leaderTerm)
	}
}

// TestPreVoteQuorumFailsWhileLeaderLeaseActive blocks only the incoming
// direction of one follower: its pre-votes still reach the peers, but it does
// not receive heartbeats, so its election timer fires repeatedly. The peers
// must keep refusing because their leader lease is live, which both protects
// the leader and proves that the candidate never collects a quorum grant.
func TestPreVoteQuorumFailsWhileLeaderLeaseActive(t *testing.T) {
	c := newTestClusterWithTimeouts(t, 3, 500*time.Millisecond, time.Second, 50*time.Millisecond)
	leader, leaderID := c.waitLeader(c.ids, testWait)

	leaderTerm := leader.Status().Term
	isolated := peersExcept(c.ids, leaderID)[0]
	majority := peersExcept(c.ids, isolated)
	preVotesBefore := c.transports[isolated].preVotes.Load()
	grantsBefore := c.transports[isolated].preVoteGrants.Load()

	// One-way block: peers cannot send heartbeats to the victim, but the
	// victim can still send pre-votes to them.
	for _, id := range majority {
		c.net.Partition(id, isolated)
	}

	deadline := time.Now().Add(1600 * time.Millisecond)
	for time.Now().Before(deadline) {
		victim := c.nodes[isolated]
		if got := victim.Status().Term; got != leaderTerm {
			t.Fatalf("victim term = %d, want %d: it started a real election without a pre-vote quorum", got, leaderTerm)
		}
		if victim.IsLeader() {
			t.Fatalf("victim %s became leader while the leader lease was active", isolated)
		}
		if !leader.IsLeader() {
			t.Fatalf("leader %s stepped down while peers held a live lease", leaderID)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if preVotes := c.transports[isolated].preVotes.Load(); preVotes == preVotesBefore {
		t.Fatal("victim never sent pre-votes, so the lease rejection was not exercised")
	}
	if grants := c.transports[isolated].preVoteGrants.Load(); grants != grantsBefore {
		t.Fatalf("peers granted %d pre-votes during an active leader lease", grants-grantsBefore)
	}

	// The majority keeps committing without the isolated follower.
	c.proposeWithRetry(majority, model.Entry{Type: model.CommandPut, Key: "lease", Value: []byte("live")}, testWait)
	c.requireValue(majority, "lease", "live", testWait)

	c.net.Heal(isolated)
	time.Sleep(300 * time.Millisecond)
	if !leader.IsLeader() {
		t.Fatalf("leader %s lost leadership after healing", leaderID)
	}
	if got := leader.Status().Term; got != leaderTerm {
		t.Fatalf("leader term after heal = %d, want %d", got, leaderTerm)
	}
	c.requireValue(c.ids, "lease", "live", testWait)
}
