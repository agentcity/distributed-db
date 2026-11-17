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
// Node.voters holds the current voter set C_new (or the only voter set outside
// a joint configuration). While Node.votersOld is non-empty the node is joint:
// C_old and C_new both vote, both must reach a majority for elections, quorum
// leases and commits, and no other configuration change is allowed until the
// final C_new entry is applied. Node.learners is the single learner set.
//
// Node.cfg.Peers is only the bootstrap configuration ("the other members"):
// after New the mutable sets are authoritative and are what quorum, elections
// and replication use. Voters is kept sorted so membership is deterministic on
// every replica and in the persisted record. Learners are non-voting: the
// leader replicates to them, but they never campaign, never grant votes and
// never count toward a quorum.
//
// Every change is a replicated entry applied in log order on every node. The
// single-change APIs use joint consensus: they first append C_old,new with
// AutoFinalize set, then the leader appends C_new once the joint entry is
// applied. A caller waits until C_new is applied, so other configuration
// changes cannot overlap it; the joint state itself also rejects them.
//
// Membership is applied only when the entry is committed (the apply loop runs
// only up to commitIndex). Snapshots embed the applied configuration, including
// C_old and the auto-finalize intent, so a restart or a snapshot install cannot
// resurrect a stale voter set.

// Membership returns copies of the current voting membership (the union of both
// voter sets while joint) and the learner set. Voters includes the local node
// when it is a voter.
func (n *Node) Membership() (voters, learners []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.votingMembersLocked(), n.learnerListLocked()
}

// AddLearner registers peer as a non-voting member of the group and waits for
// the change to commit and apply. It must be called on the leader. Adding a
// peer that is already a voter returns ErrAlreadyVoter, and an existing learner
// returns ErrAlreadyLearner. The change is a joint configuration whose two
// voter sets are equal.
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
// and apply. It must be called on the leader. Removing the leader is allowed:
// the leader keeps leading through the joint configuration (it is still in
// C_old) and steps down only after C_new, which excludes it, is applied.
// Removing the last voter is rejected as unsafe.
func (n *Node) RemoveVoter(ctx context.Context, peer string) error {
	return n.changeMembership(ctx, model.CommandRemoveVoter, peer)
}

// changeMembership implements one single-server change as enter-joint followed
// by leave-joint. It computes the target configuration under mu, proposes the
// joint entry with AutoFinalize set, then waits for the final entry. The leader
// appends C_new automatically (apply-side signal); a later leader repairs the
// change if this one loses leadership first.
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
	if n.configInFlight || n.isJointLocked() {
		n.mu.Unlock()
		return ErrConfigChangeInProgress
	}
	voters, learners, err := n.targetConfigLocked(typ, peer)
	if err != nil {
		n.mu.Unlock()
		return err
	}
	data := ConfigEntryData{
		VotersOld:    append([]string(nil), n.voters...),
		Voters:       voters,
		Learners:     learners,
		AutoFinalize: true,
		Epoch:        n.configEpoch + 1,
	}
	n.configInFlight = true
	n.mu.Unlock()

	if err := n.proposeJointConfig(ctx, data); err != nil {
		n.mu.Lock()
		jointApplied := n.isJointLocked() && n.configEpoch >= data.Epoch && n.autoFinalize
		finalApplied := !n.isJointLocked() && n.configEpoch >= data.Epoch+1
		n.mu.Unlock()
		if !jointApplied && !finalApplied {
			return err
		}
	}

	// Wait until C_new is applied. The automatic finalizer appends it as soon
	// as the joint entry is applied on the leader; a new leader does the same
	// after an election, so waiting through a leadership change is safe.
	return n.waitConfig(ctx, func() bool {
		return !n.isJointLocked() && n.configEpoch >= data.Epoch+1
	})
}

// targetConfigLocked computes the target C_new for one single-change command.
// Caller holds mu and has checked that the node is leader and not joint.
func (n *Node) targetConfigLocked(typ model.CommandType, peer string) (voters, learners []string, err error) {
	switch typ {
	case model.CommandAddLearner:
		if n.isVotingMemberLocked(peer) {
			return nil, nil, fmt.Errorf("%w: %s", ErrAlreadyVoter, peer)
		}
		if n.isLearnerLocked(peer) {
			return nil, nil, fmt.Errorf("%w: %s", ErrAlreadyLearner, peer)
		}
		voters = append([]string(nil), n.voters...)
		learners = append(n.learnerListLocked(), peer)
		sort.Strings(learners)
		return voters, learners, nil
	case model.CommandPromoteLearner:
		if n.isVotingMemberLocked(peer) {
			return nil, nil, fmt.Errorf("%w: %s", ErrAlreadyVoter, peer)
		}
		if !n.isLearnerLocked(peer) {
			return nil, nil, fmt.Errorf("%w: %s", ErrNotLearner, peer)
		}
		voters = append(append([]string(nil), n.voters...), peer)
		sort.Strings(voters)
		for _, l := range n.learnerListLocked() {
			if l != peer {
				learners = append(learners, l)
			}
		}
		return voters, learners, nil
	case model.CommandRemoveVoter:
		if !n.isVotingMemberLocked(peer) {
			return nil, nil, fmt.Errorf("%w: %s", ErrNotVoter, peer)
		}
		if len(n.voters) <= 1 {
			return nil, nil, fmt.Errorf("%w: cannot remove the last voter %s", ErrUnsafeConfigChange, peer)
		}
		for _, v := range n.voters {
			if v != peer {
				voters = append(voters, v)
			}
		}
		for _, l := range n.learnerListLocked() {
			if l != peer {
				learners = append(learners, l)
			}
		}
		return voters, learners, nil
	default:
		return nil, nil, fmt.Errorf("raft: %q is not a membership command", typ)
	}
}

// applyConfigEntry applies one committed membership command. It is called by
// the apply loop instead of the application state machine and is deterministic:
// every replica applies the same entries in the same order. Configuration
// changes take effect only here, after commit.
func (n *Node) applyConfigEntry(entry model.Entry) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return ErrStopped
	}

	var (
		changed bool
		err     error
	)
	switch entry.Type {
	case CommandJointConfig:
		changed, err = n.applyJointConfigLocked(entry)
	case CommandFinalConfig:
		changed, err = n.applyFinalConfigLocked(entry)
	case model.CommandAddLearner, model.CommandPromoteLearner, model.CommandRemoveVoter:
		if n.isJointLocked() {
			return fmt.Errorf("%w: legacy config entry while joint", ErrConfigChangeInProgress)
		}
		changed, err = n.applyLegacyConfigLocked(entry)
	default:
		return fmt.Errorf("raft: %q is not a config entry", entry.Type)
	}
	if err != nil {
		return err
	}

	// Keep the one-change gate closed while joint; the final entry clears it.
	if n.isJointLocked() {
		n.configInFlight = true
	} else {
		n.configInFlight = false
		n.finalInFlight = false
	}

	var persistErr error
	if changed {
		persistErr = n.persistConfigLocked()
	}
	n.syncReplicatorsLocked()
	n.signalAllLocked()
	// Configuration waiters are notified by the apply loop after lastApplied
	// advances, so a caller that observes the new configuration also sees the
	// snapshot/index state that includes it.
	// A single-change joint configuration is completed by the dedicated
	// finalizer goroutine; EnterJoint leaves AutoFinalize false and waits for
	// an explicit LeaveJoint.
	n.signalAutoFinalLocked()
	if !n.selfIsVoterLocked() && (n.role == Leader || n.role == Candidate) {
		// The applied configuration excludes this node from every voter set;
		// it can no longer lead or campaign. A learner may remain a follower.
		n.stepDownWithErrorLocked("", ErrNotLeader)
	}
	if persistErr != nil {
		n.logger.Error("raft: persist membership failed", "id", n.cfg.ID, "error", persistErr)
		return persistErr
	}
	return nil
}

// applyLegacyConfigLocked applies one of the original single-server commands.
// New code writes joint entries instead, but a log created by an older version
// (or an entry proposed before an upgrade) must still apply deterministically.
// Caller holds mu.
func (n *Node) applyLegacyConfigLocked(entry model.Entry) (bool, error) {
	peer := entry.Key
	if peer == "" {
		return false, fmt.Errorf("raft: %q config entry with empty peer", entry.Type)
	}
	beforeVoters := append([]string(nil), n.voters...)
	beforeLearners := n.learnerListLocked()

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
		return false, fmt.Errorf("raft: %q is not a config entry", entry.Type)
	}

	voters := append([]string(nil), n.voters...)
	learners := n.learnerListLocked()
	// Advance the epoch only when this entry actually changes the sets. A
	// replayed legacy entry is then a no-op for the epoch.
	changed := !equalStrings(voters, beforeVoters) || !equalStrings(learners, beforeLearners)
	if changed {
		n.configEpoch++
	}
	return changed, nil
}

// persistConfigLocked saves the applied configuration through the optional
// membership capabilities. Stores without them keep membership only in memory
// and in the replicated log/snapshots. Caller holds mu.
func (n *Node) persistConfigLocked() error {
	return saveConfigState(n.store, n.configStateLocked())
}

// initMembership recovers the configuration from the snapshot, then the store,
// then the static Config. The snapshot wins when its config epoch is at least
// the persisted epoch, so a node that missed membership changes and restarts
// after installing a snapshot cannot keep a stale voter set. A snapshot without
// membership (legacy) falls back to the persisted record or Config.
func initMembership(cfg Config, store LogStore, snapMembership *SnapshotMembership) (configState, error) {
	var persisted configState
	var hasPersisted bool
	if js, ok := store.(JointMembershipStore); ok {
		votersOld, voters, learners, auto, epoch, err := js.LoadJointMembership()
		if err != nil {
			return configState{}, fmt.Errorf("raft: load membership: %w", err)
		}
		persisted, hasPersisted = canonicalConfigState(votersOld, voters, learners, auto, epoch)
	} else if mes, ok := store.(MembershipEpochStore); ok {
		voters, learners, epoch, err := mes.LoadMembershipEpoch()
		if err != nil {
			return configState{}, fmt.Errorf("raft: load membership: %w", err)
		}
		persisted, hasPersisted = canonicalConfigState(nil, voters, learners, false, epoch)
	} else if ms, ok := store.(MembershipStore); ok {
		voters, learners, err := ms.LoadMembership()
		if err != nil {
			return configState{}, fmt.Errorf("raft: load membership: %w", err)
		}
		persisted, hasPersisted = canonicalConfigState(nil, voters, learners, false, 0)
	}

	snapMembership, err := normalizeSnapshotMembership(snapMembership)
	if err != nil {
		return configState{}, err
	}

	if cfg.ForceLearner {
		voters := canonicalVoters(cfg.Peers)
		if len(voters) == 0 {
			return configState{}, errors.New("raft: learner configuration has no voters")
		}
		learners := canonicalLearners([]string{cfg.ID}, voters)
		// ForceLearner deliberately replaces the configuration with a fresh
		// one, so advance past any epoch this node may already have seen.
		epoch := persisted.epoch
		if snapMembership != nil && snapMembership.ConfigEpoch > epoch {
			epoch = snapMembership.ConfigEpoch
		}
		if epoch == 0 {
			epoch = 1
		} else {
			epoch++
		}
		return configState{voters: voters, learners: learners, epoch: epoch}, nil
	}

	if snapMembership != nil && (!hasPersisted || snapMembership.ConfigEpoch >= persisted.epoch) {
		// The snapshot is at least as new as the persisted membership. Persist
		// the reconciled state so a restart without the snapshot (or with a
		// different load path) still sees the same configuration.
		state := configState{
			votersOld:    append([]string(nil), snapMembership.VotersOld...),
			voters:       append([]string(nil), snapMembership.Voters...),
			learners:     append([]string(nil), snapMembership.Learners...),
			autoFinalize: snapMembership.AutoFinalize,
			epoch:        snapMembership.ConfigEpoch,
		}
		if err := saveConfigState(store, state); err != nil {
			return configState{}, fmt.Errorf("raft: persist snapshot membership: %w", err)
		}
		return state, nil
	}

	if hasPersisted {
		if persisted.epoch == 0 {
			// A record written before config epochs existed is the initial
			// configuration.
			persisted.epoch = 1
		}
		return persisted, nil
	}

	if cfg.Learner {
		voters := canonicalVoters(cfg.Peers)
		if len(voters) == 0 {
			return configState{}, errors.New("raft: learner configuration has no voters")
		}
		learners := canonicalLearners([]string{cfg.ID}, voters)
		return configState{voters: voters, learners: learners, epoch: 1}, nil
	}
	voters := canonicalVoters(append(append([]string(nil), cfg.Peers...), cfg.ID))
	return configState{voters: voters, epoch: 1}, nil
}

// canonicalConfigState normalizes a persisted record. It tolerates empty and
// duplicate ids from legacy records but reports "not persisted" when no voter
// set was ever written.
func canonicalConfigState(votersOld, voters, learners []string, auto bool, epoch uint64) (configState, bool) {
	voters = canonicalVoters(voters)
	if len(voters) == 0 {
		return configState{}, false
	}
	votersOld = canonicalVoters(votersOld)
	learners = canonicalLearners(learners, append(append([]string(nil), voters...), votersOld...))
	if len(votersOld) == 0 {
		auto = false
	}
	return configState{
		votersOld:    votersOld,
		voters:       voters,
		learners:     learners,
		autoFinalize: auto,
		epoch:        epoch,
	}, true
}

// normalizeSnapshotMembership validates a membership envelope and returns a
// canonical copy (sorted, de-duplicated slices). A nil envelope is a legacy
// snapshot and passes through as nil. Corrupt values are rejected: a present
// envelope must have a positive config epoch, at least one voter, no empty ids,
// no duplicates, no id that is both a voter and a learner, and an old voter set
// exactly when it is joint (AutoFinalize requires one).
func normalizeSnapshotMembership(m *SnapshotMembership) (*SnapshotMembership, error) {
	if m == nil {
		return nil, nil
	}
	if m.ConfigEpoch == 0 {
		return nil, fmt.Errorf("%w: missing config epoch", ErrInvalidSnapshotMembership)
	}
	voters, err := canonicalSnapshotIDs(m.Voters, "voter")
	if err != nil {
		return nil, err
	}
	if len(voters) == 0 {
		return nil, fmt.Errorf("%w: no voters", ErrInvalidSnapshotMembership)
	}
	votersOld, err := canonicalSnapshotIDs(m.VotersOld, "old voter")
	if err != nil {
		return nil, err
	}
	learners, err := canonicalSnapshotIDs(m.Learners, "learner")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(voters)+len(votersOld))
	for _, id := range voters {
		seen[id] = true
	}
	for _, id := range votersOld {
		seen[id] = true
	}
	for _, id := range learners {
		if seen[id] {
			return nil, fmt.Errorf("%w: %q is both a voter and a learner", ErrInvalidSnapshotMembership, id)
		}
	}
	if m.AutoFinalize && len(votersOld) == 0 {
		return nil, fmt.Errorf("%w: auto-finalize on a non-joint membership", ErrInvalidSnapshotMembership)
	}
	return &SnapshotMembership{
		Voters:       voters,
		VotersOld:    votersOld,
		Learners:     learners,
		AutoFinalize: m.AutoFinalize,
		ConfigEpoch:  m.ConfigEpoch,
	}, nil
}

func canonicalSnapshotIDs(ids []string, what string) ([]string, error) {
	out, err := canonicalConfigIDs(ids, what)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSnapshotMembership, err)
	}
	return out, nil
}

// snapshotMembershipLocked returns the current configuration as a snapshot
// envelope. Caller holds mu. The result is always non-nil and canonical, and
// preserves the joint old set and auto-finalize intent.
func (n *Node) snapshotMembershipLocked() *SnapshotMembership {
	return &SnapshotMembership{
		Voters:       append([]string(nil), n.voters...),
		VotersOld:    append([]string(nil), n.votersOld...),
		Learners:     n.learnerListLocked(),
		AutoFinalize: n.autoFinalize,
		ConfigEpoch:  n.configEpoch,
	}
}

// applySnapshotMembershipLocked replaces the in-memory membership with the
// envelope carried by an installed snapshot and persists it. A nil envelope
// (legacy snapshot) leaves the current membership untouched. The snapshot is
// authoritative: any stale voter or learner is removed, the config epoch moves
// to the snapshot's epoch (never backwards), and a leader/candidate that is no
// longer a voter in any of the snapshot's voter sets steps down. Caller holds
// mu.
func (n *Node) applySnapshotMembershipLocked(m *SnapshotMembership) error {
	if m == nil {
		return nil
	}
	normalized, err := normalizeSnapshotMembership(m)
	if err != nil {
		return err
	}
	if normalized.ConfigEpoch < n.configEpoch {
		return fmt.Errorf("%w: snapshot epoch %d, local epoch %d",
			ErrStaleSnapshotMembership, normalized.ConfigEpoch, n.configEpoch)
	}
	n.votersOld = append([]string(nil), normalized.VotersOld...)
	n.voters = append([]string(nil), normalized.Voters...)
	n.learners = stringSet(normalized.Learners)
	n.autoFinalize = normalized.AutoFinalize
	n.configEpoch = normalized.ConfigEpoch
	n.configInFlight = n.isJointLocked()
	n.finalInFlight = false
	n.syncReplicatorsLocked()
	n.signalAllLocked()
	n.notifyConfigLocked()
	n.signalAutoFinalLocked()
	if !n.selfIsVoterLocked() && (n.role == Leader || n.role == Candidate) {
		n.stepDownWithErrorLocked("", ErrNotLeader)
	}
	if err := n.persistConfigLocked(); err != nil {
		n.logger.Error("raft: persist snapshot membership failed", "id", n.cfg.ID, "error", err)
		return err
	}
	return nil
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

// isJointLocked reports whether the node is in the C_old,new configuration.
func (n *Node) isJointLocked() bool {
	return len(n.votersOld) > 0
}

// isVoterLocked reports membership in C_new (the current voter set outside a
// joint configuration).
func (n *Node) isVoterLocked(id string) bool {
	for _, v := range n.voters {
		if v == id {
			return true
		}
	}
	return false
}

// isOldVoterLocked reports membership in C_old while joint.
func (n *Node) isOldVoterLocked(id string) bool {
	for _, v := range n.votersOld {
		if v == id {
			return true
		}
	}
	return false
}

// isVotingMemberLocked reports whether id may vote and campaign in the current
// configuration: it is in C_new, or in C_old while joint.
func (n *Node) isVotingMemberLocked(id string) bool {
	return n.isVoterLocked(id) || n.isOldVoterLocked(id)
}

func (n *Node) isLearnerLocked(id string) bool {
	return n.learners[id]
}

// selfIsVoterLocked reports whether the local node may campaign and vote. While
// joint this includes nodes that only C_old still contains: they remain voters
// until C_new is applied, and step down only then if removed.
func (n *Node) selfIsVoterLocked() bool {
	return n.isVotingMemberLocked(n.cfg.ID)
}

// votingMembersLocked returns the sorted union of C_old and C_new. Outside a
// joint configuration it is simply the current voter set.
func (n *Node) votingMembersLocked() []string {
	if !n.isJointLocked() {
		return append([]string(nil), n.voters...)
	}
	seen := make(map[string]bool, len(n.voters)+len(n.votersOld))
	out := make([]string, 0, len(n.voters)+len(n.votersOld))
	for _, v := range n.voters {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, v := range n.votersOld {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
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

// replicationVotingMembersLocked returns the voters this leader must replicate
// to. It is the union of the applied voter sets and the electoral sets derived
// from config entries already in the log. Adding the pending joint target means
// a leader starts replicating to brand-new voters as soon as it appends
// C_old,new: without that, a joint entry that requires a majority of those new
// voters could never commit. The applied sets are always kept, so a pending
// final entry cannot stop replication to C_old voters before the final entry is
// committed.
func (n *Node) replicationVotingMembersLocked() []string {
	applied := n.votingMembersLocked()
	pending := electoralVotingMembers(n.electoralConfigLocked())
	seen := make(map[string]bool, len(applied)+len(pending))
	out := make([]string, 0, len(applied)+len(pending))
	for _, v := range applied {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, v := range pending {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// replicationTargetsLocked lists every non-self peer the leader must replicate
// to: every voting member (both applied and pending sets) plus every learner,
// sorted.
func (n *Node) replicationTargetsLocked() []string {
	members := n.replicationVotingMembersLocked()
	seen := make(map[string]bool, len(members)+len(n.learners))
	out := make([]string, 0, len(members)+len(n.learners))
	for _, v := range members {
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

// isReplicationTargetLocked reports whether peer is an applied or pending
// voter, or a learner. Caller holds mu.
func (n *Node) isReplicationTargetLocked(peer string) bool {
	if n.isLearnerLocked(peer) {
		return true
	}
	for _, v := range n.replicationVotingMembersLocked() {
		if v == peer {
			return true
		}
	}
	return false
}

// replicationTarget reports whether peer is still a voter or learner this node
// must replicate to. It is used by long-lived replicator loops to notice a
// removal without a per-peer cancellation channel.
func (n *Node) replicationTarget(peer string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.isReplicationTargetLocked(peer)
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
