package raft

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"distributed-db/internal/model"
)

// Membership model
//
// Node.voters holds every voting member, including the local node when it is a
// voter. Node.cfg.Peers is only the bootstrap configuration ("the other
// members"): after New the mutable voters/learners sets are authoritative and
// are what quorum, elections and replication use.
//
// Voters is kept sorted so membership is deterministic on every replica and in
// the persisted record. Learners are non-voting: the leader replicates to them,
// but they never campaign, never grant votes, and never count toward a quorum
// or commit index.
//
// This implementation changes one server at a time and has no joint consensus
// (see the package documentation on membership). Every change is a replicated
// entry applied in log order on every node; callers wait for it to commit and
// apply before it returns, so there is never more than one in-flight change.

// Membership returns copies of the current voter and learner sets. Voters
// includes the local node when it is a voter.
func (n *Node) Membership() (voters, learners []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.voters...), n.learnerListLocked()
}

// AddLearner registers peer as a non-voting member of the group and waits for
// the change to commit and apply. It must be called on the leader. Adding a
// peer that is already a voter returns ErrAlreadyVoter, and an existing learner
// returns ErrAlreadyLearner.
func (n *Node) AddLearner(ctx context.Context, peer string) error {
	return n.changeMembership(ctx, model.CommandAddLearner, peer)
}

// PromoteLearner turns a caught-up learner into a voter and waits for the
// change to commit and apply. It must be called on the leader. The caller is
// responsible for promoting only after the learner has caught up; a promotion
// changes the quorum immediately, so promoting a lagging learner can reduce
// availability.
func (n *Node) PromoteLearner(ctx context.Context, peer string) error {
	return n.changeMembership(ctx, model.CommandPromoteLearner, peer)
}

// RemoveVoter removes a voter from the group and waits for the change to commit
// and apply. It must be called on the leader. Removing the leader itself is
// rejected: leadership must be transferred first. Removing the last voter is
// rejected as unsafe.
func (n *Node) RemoveVoter(ctx context.Context, peer string) error {
	return n.changeMembership(ctx, model.CommandRemoveVoter, peer)
}

// changeMembership validates one single-server membership change, appends it
// as a replicated config entry and waits for it to be applied. The
// configInFlight gate enforces "one change at a time" even when callers race.
func (n *Node) changeMembership(ctx context.Context, typ model.CommandType, peer string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if peer == "" {
		return fmt.Errorf("%w: empty peer id", ErrUnknownTarget)
	}

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return ErrStopped
	}
	if n.role != Leader {
		n.mu.Unlock()
		return ErrNotLeader
	}
	if n.configInFlight {
		n.mu.Unlock()
		return ErrConfigChangeInProgress
	}
	if err := n.validateMembershipChangeLocked(typ, peer); err != nil {
		n.mu.Unlock()
		return err
	}
	n.configInFlight = true
	n.mu.Unlock()

	_, err := n.Propose(ctx, model.Entry{Type: typ, Key: peer})
	if err == nil {
		// Propose returns after the apply loop consumed the entry, and
		// applyConfigEntry has already cleared the gate.
		return nil
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	// A timeout or cancellation can leave a durable entry that may still
	// commit. Keep the gate closed so a second change cannot overlap; it is
	// cleared when the entry is applied or when this node stops being leader.
	if n.role != Leader || n.stopped ||
		(!errors.Is(err, ErrTimeout) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)) {
		n.configInFlight = false
	}
	return err
}

// validateMembershipChangeLocked checks one change against the current
// membership. Caller holds mu.
func (n *Node) validateMembershipChangeLocked(typ model.CommandType, peer string) error {
	switch typ {
	case model.CommandAddLearner:
		if n.isVoterLocked(peer) {
			return fmt.Errorf("%w: %s", ErrAlreadyVoter, peer)
		}
		if n.isLearnerLocked(peer) {
			return fmt.Errorf("%w: %s", ErrAlreadyLearner, peer)
		}
	case model.CommandPromoteLearner:
		if !n.isLearnerLocked(peer) {
			if n.isVoterLocked(peer) {
				return fmt.Errorf("%w: %s", ErrAlreadyVoter, peer)
			}
			return fmt.Errorf("%w: %s", ErrNotLearner, peer)
		}
	case model.CommandRemoveVoter:
		if peer == n.cfg.ID {
			return fmt.Errorf("%w: cannot remove the current leader %s; transfer leadership first",
				ErrUnsafeConfigChange, peer)
		}
		if !n.isVoterLocked(peer) {
			return fmt.Errorf("%w: %s", ErrNotVoter, peer)
		}
		if len(n.voters) <= 1 {
			return fmt.Errorf("%w: cannot remove the last voter %s", ErrUnsafeConfigChange, peer)
		}
	default:
		return fmt.Errorf("raft: %q is not a membership command", typ)
	}
	return nil
}

// applyConfigEntry applies one committed membership command. It is called by
// the apply loop instead of the application state machine and is deterministic:
// every replica applies the same entries in the same order.
func (n *Node) applyConfigEntry(entry model.Entry) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return ErrStopped
	}
	peer := entry.Key
	if peer == "" {
		return fmt.Errorf("raft: %q config entry with empty peer", entry.Type)
	}

	switch entry.Type {
	case model.CommandAddLearner:
		// Adding a peer as a learner also downgrades it if a stale local view
		// still lists it as a voter (for example a restarted target node).
		n.removeVoterLocked(peer)
		n.learners[peer] = true
	case model.CommandPromoteLearner:
		delete(n.learners, peer)
		n.addVoterLocked(peer)
	case model.CommandRemoveVoter:
		n.removeVoterLocked(peer)
		delete(n.learners, peer)
	default:
		return fmt.Errorf("raft: %q is not a config entry", entry.Type)
	}

	voters := append([]string(nil), n.voters...)
	learners := n.learnerListLocked()
	persistErr := n.persistMembershipLocked(voters, learners)

	// The committed entry is authoritative even if the durable membership
	// record failed: keep serving with the new membership and report the
	// persistence failure to the proposal waiter.
	n.configInFlight = false
	n.syncReplicatorsLocked()
	n.signalAllLocked()
	if persistErr != nil {
		n.logger.Error("raft: persist membership failed", "id", n.cfg.ID, "error", persistErr)
	}
	return persistErr
}

// persistMembershipLocked saves the voter/learner sets through the optional
// MembershipStore capability. Stores without it keep membership only in memory
// and in the replicated log. Caller holds mu.
func (n *Node) persistMembershipLocked(voters, learners []string) error {
	ms, ok := n.store.(MembershipStore)
	if !ok {
		return nil
	}
	return ms.SaveMembership(voters, learners)
}

// initMembership recovers the membership from the store, falling back to the
// static Config for a new or pre-membership log. The returned voters always
// include the local node, sorted; the local node is a learner instead when
// cfg.Learner is set (and was not overridden by a persisted record).
func initMembership(cfg Config, store LogStore) ([]string, []string, error) {
	if cfg.ForceLearner {
		voters := canonicalVoters(cfg.Peers)
		if len(voters) == 0 {
			return nil, nil, errors.New("raft: learner configuration has no voters")
		}
		learners := canonicalLearners([]string{cfg.ID}, voters)
		return voters, learners, nil
	}
	if ms, ok := store.(MembershipStore); ok {
		voters, learners, err := ms.LoadMembership()
		if err != nil {
			return nil, nil, fmt.Errorf("raft: load membership: %w", err)
		}
		if len(voters) > 0 {
			v := canonicalVoters(voters)
			l := canonicalLearners(learners, v)
			return v, l, nil
		}
	}

	if cfg.Learner {
		voters := canonicalVoters(cfg.Peers)
		if len(voters) == 0 {
			return nil, nil, errors.New("raft: learner configuration has no voters")
		}
		learners := canonicalLearners([]string{cfg.ID}, voters)
		return voters, learners, nil
	}
	voters := canonicalVoters(append(append([]string(nil), cfg.Peers...), cfg.ID))
	return voters, nil, nil
}

// canonicalVoters de-duplicates and sorts voters. A nil/empty input is
// returned as nil so initMembership can distinguish "not persisted".
func canonicalVoters(voters []string) []string {
	if len(voters) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(voters))
	out := make([]string, 0, len(voters))
	for _, v := range voters {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// canonicalLearners de-duplicates learners, sorts them and drops any id that is
// also a voter.
func canonicalLearners(learners, voters []string) []string {
	if len(learners) == 0 {
		return nil
	}
	isVoter := make(map[string]bool, len(voters))
	for _, v := range voters {
		isVoter[v] = true
	}
	seen := make(map[string]bool, len(learners))
	out := make([]string, 0, len(learners))
	for _, l := range learners {
		if l == "" || seen[l] || isVoter[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func (n *Node) isVoterLocked(id string) bool {
	for _, v := range n.voters {
		if v == id {
			return true
		}
	}
	return false
}

func (n *Node) isLearnerLocked(id string) bool {
	return n.learners[id]
}

// selfIsVoterLocked reports whether the local node may campaign and vote.
func (n *Node) selfIsVoterLocked() bool {
	return n.isVoterLocked(n.cfg.ID)
}

func (n *Node) addVoterLocked(id string) {
	if id == "" || n.isVoterLocked(id) {
		return
	}
	n.voters = append(n.voters, id)
	sort.Strings(n.voters)
}

func (n *Node) removeVoterLocked(id string) {
	for i, v := range n.voters {
		if v == id {
			n.voters = append(n.voters[:i], n.voters[i+1:]...)
			return
		}
	}
}

// learnerListLocked returns a sorted copy of the learner set.
func (n *Node) learnerListLocked() []string {
	if len(n.learners) == 0 {
		return nil
	}
	out := make([]string, 0, len(n.learners))
	for id := range n.learners {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// votersExceptSelfLocked returns this node's voters excluding the local id.
func (n *Node) votersExceptSelfLocked() []string {
	out := make([]string, 0, len(n.voters))
	for _, v := range n.voters {
		if v != n.cfg.ID {
			out = append(out, v)
		}
	}
	return out
}

// replicationTargetsLocked lists every non-self peer the leader must replicate
// to: all other voters plus every learner, sorted.
func (n *Node) replicationTargetsLocked() []string {
	seen := make(map[string]bool, len(n.voters)+len(n.learners))
	out := make([]string, 0, len(n.voters)+len(n.learners))
	for _, v := range n.voters {
		if v == n.cfg.ID || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	for l := range n.learners {
		if l == n.cfg.ID || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// replicationTarget reports whether peer is still a voter or learner this node
// must replicate to. It is used by long-lived replicator loops to notice a
// removal without a per-peer cancellation channel.
func (n *Node) replicationTarget(peer string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.isVoterLocked(peer) || n.isLearnerLocked(peer)
}

// syncReplicatorsLocked starts a replicator for every new member and forgets
// bookkeeping for members that were removed. Existing loops are left alone.
// Caller holds mu and must be leader.
func (n *Node) syncReplicatorsLocked() {
	if n.role != Leader || n.leaderCancel == nil || n.stopped {
		return
	}
	targets := n.replicationTargetsLocked()
	want := make(map[string]bool, len(targets))
	for _, peer := range targets {
		want[peer] = true
	}
	for peer := range n.triggers {
		if want[peer] {
			continue
		}
		// The loop observes the membership change on its next tick and exits.
		delete(n.triggers, peer)
		delete(n.nextIndex, peer)
		delete(n.matchIndex, peer)
		delete(n.lastAck, peer)
	}

	term := n.term
	leaderCtx := n.leaderCtx
	for _, peer := range targets {
		if _, ok := n.triggers[peer]; ok {
			continue
		}
		if _, ok := n.nextIndex[peer]; !ok {
			// A fresh learner starts from the leader's snapshot when the log
			// prefix it needs is already compacted. With no snapshot the
			// standard last+1 probe backs up through AppendEntries.
			if n.snapshot.Index > 0 {
				n.nextIndex[peer] = n.snapshot.Index
			} else {
				n.nextIndex[peer] = n.lastIndexLocked() + 1
			}
			n.matchIndex[peer] = 0
		}
		trigger := make(chan struct{}, 1)
		n.triggers[peer] = trigger
		n.wg.Add(1)
		go func(peer string, trigger <-chan struct{}, term uint64, ctx context.Context) {
			defer n.wg.Done()
			n.replicateLoop(ctx, peer, trigger, term)
		}(peer, trigger, term, leaderCtx)
	}
}
