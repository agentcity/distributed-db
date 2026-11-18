package raft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"distributed-db/internal/model"
)

// Joint consensus configuration entries
//
// A normal membership change is two replicated entries:
//
//	CommandJointConfig  C_old,new  (old voters, new voters, learners)
//	CommandFinalConfig  C_new      (new voters, learners)
//
// While C_old,new is applied the node is "joint": elections, quorum leases and
// commit all require a majority of both voter sets. The final entry is only
// appended after the joint entry is committed and applied, and applying it
// atomically drops C_old. This preserves the joint-consensus safety property:
// no two disjoint majorities can decide different logs while the configuration
// changes.
//
// Entries carry a monotonic ConfigEpoch. An entry whose epoch is not exactly one
// past the applied configuration is stale, belongs to a conflicting history, or
// is a duplicate; stale/conflicting entries are never applied. Replaying an
// already-applied identical entry is an idempotent no-op.
const (
	// CommandJointConfig is the intermediate C_old,new configuration entry.
	// Entry.Value is a JSON ConfigEntryData with VotersOld set.
	CommandJointConfig model.CommandType = "config_joint"
	// CommandFinalConfig is the final C_new configuration entry. Entry.Value
	// is a JSON ConfigEntryData with VotersOld empty.
	CommandFinalConfig model.CommandType = "config_final"
)

// ConfigEntryData is the normalized payload of a CommandJointConfig or
// CommandFinalConfig entry. VotersOld is C_old and is non-empty exactly for a
// joint entry; Voters is C_new. Learners never overlap either voter set.
// AutoFinalize asks the leader to append the final entry automatically once the
// joint configuration is applied; the single-change APIs set it so that a
// crash or leadership change in the joint state is repaired by the next leader.
type ConfigEntryData struct {
	VotersOld    []string `json:"voters_old,omitempty"`
	Voters       []string `json:"voters"`
	Learners     []string `json:"learners,omitempty"`
	AutoFinalize bool     `json:"auto_finalize,omitempty"`
	Epoch        uint64   `json:"epoch"`
}

// EncodeConfigEntry validates data and returns its canonical JSON payload.
func EncodeConfigEntry(data ConfigEntryData) ([]byte, error) {
	normalized, err := normalizeConfigEntryData(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

// DecodeConfigEntry parses and structurally validates a configuration entry
// payload. It does not decide whether the entry is joint or final; that is the
// entry type's job and is checked by the apply path.
func DecodeConfigEntry(value []byte) (ConfigEntryData, error) {
	if len(value) == 0 {
		return ConfigEntryData{}, fmt.Errorf("%w: empty payload", ErrInvalidConfigEntry)
	}
	var data ConfigEntryData
	if err := json.Unmarshal(value, &data); err != nil {
		return ConfigEntryData{}, fmt.Errorf("%w: decode: %v", ErrInvalidConfigEntry, err)
	}
	return normalizeConfigEntryData(data)
}

// normalizeConfigEntryData canonicalizes and validates the structural fields.
// It rejects empty and duplicate ids rather than silently dropping them: a
// malformed config entry is a bug, not a request to tidy up.
func normalizeConfigEntryData(data ConfigEntryData) (ConfigEntryData, error) {
	if data.Epoch == 0 {
		return ConfigEntryData{}, fmt.Errorf("%w: missing config epoch", ErrInvalidConfigEntry)
	}
	voters, err := canonicalConfigIDs(data.Voters, "voter")
	if err != nil {
		return ConfigEntryData{}, err
	}
	if len(voters) == 0 {
		return ConfigEntryData{}, fmt.Errorf("%w: no voters", ErrInvalidConfigEntry)
	}
	votersOld, err := canonicalConfigIDs(data.VotersOld, "old voter")
	if err != nil {
		return ConfigEntryData{}, err
	}
	learners, err := canonicalConfigIDs(data.Learners, "learner")
	if err != nil {
		return ConfigEntryData{}, err
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
			return ConfigEntryData{}, fmt.Errorf("%w: %q is both a voter and a learner", ErrInvalidConfigEntry, id)
		}
	}
	if data.AutoFinalize && len(votersOld) == 0 {
		return ConfigEntryData{}, fmt.Errorf("%w: auto-finalize on a non-joint entry", ErrInvalidConfigEntry)
	}
	return ConfigEntryData{
		VotersOld:    votersOld,
		Voters:       voters,
		Learners:     learners,
		AutoFinalize: data.AutoFinalize,
		Epoch:        data.Epoch,
	}, nil
}

// validateJointEntry additionally requires a non-empty old voter set.
func validateJointEntry(data ConfigEntryData) (ConfigEntryData, error) {
	normalized, err := normalizeConfigEntryData(data)
	if err != nil {
		return ConfigEntryData{}, err
	}
	if len(normalized.VotersOld) == 0 {
		return ConfigEntryData{}, fmt.Errorf("%w: joint entry has no old voters", ErrInvalidConfigEntry)
	}
	return normalized, nil
}

// validateFinalEntry requires a final entry: C_new only, no auto-finalize.
func validateFinalEntry(data ConfigEntryData) (ConfigEntryData, error) {
	normalized, err := normalizeConfigEntryData(data)
	if err != nil {
		return ConfigEntryData{}, err
	}
	if len(normalized.VotersOld) != 0 {
		return ConfigEntryData{}, fmt.Errorf("%w: final entry carries old voters", ErrInvalidConfigEntry)
	}
	if normalized.AutoFinalize {
		return ConfigEntryData{}, fmt.Errorf("%w: final entry requests auto-finalize", ErrInvalidConfigEntry)
	}
	return normalized, nil
}

// canonicalConfigIDs rejects empty and duplicate ids and returns a sorted copy.
func canonicalConfigIDs(ids []string, what string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("%w: empty %s id", ErrInvalidConfigEntry, what)
		}
		if seen[id] {
			return nil, fmt.Errorf("%w: duplicate %s id %q", ErrInvalidConfigEntry, what, id)
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// configState is the complete applied membership configuration.
type configState struct {
	votersOld    []string // non-empty exactly while joint (C_old)
	voters       []string // C_new; outside a joint config, the current voters
	learners     []string
	autoFinalize bool
	epoch        uint64
}

func (c configState) joint() bool { return len(c.votersOld) > 0 }

// configStateLocked snapshots the node's configuration. Caller holds mu.
func (n *Node) configStateLocked() configState {
	return configState{
		votersOld:    append([]string(nil), n.votersOld...),
		voters:       append([]string(nil), n.voters...),
		learners:     n.learnerListLocked(),
		autoFinalize: n.autoFinalize,
		epoch:        n.configEpoch,
	}
}

// saveConfigState persists a configuration through the richest capability the
// store supports. A store without JointMembershipStore still gets the C_new
// voter set and learners; the joint detail is then recoverable only from the
// log or a snapshot.
func saveConfigState(store LogStore, c configState) error {
	if js, ok := store.(JointMembershipStore); ok {
		return js.SaveJointMembership(c.votersOld, c.voters, c.learners, c.autoFinalize, c.epoch)
	}
	if mes, ok := store.(MembershipEpochStore); ok {
		return mes.SaveMembershipEpoch(c.voters, c.learners, c.epoch)
	}
	if ms, ok := store.(MembershipStore); ok {
		return ms.SaveMembership(c.voters, c.learners)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Quorum rules
// ---------------------------------------------------------------------------

// quorumInLocked requires a strict majority of set to satisfy ack. An empty set
// can never form a quorum; every real configuration has at least one voter.
func (n *Node) quorumInLocked(set []string, ack func(string) bool) bool {
	if len(set) == 0 {
		return false
	}
	need := len(set)/2 + 1
	count := 0
	for _, id := range set {
		if ack(id) {
			count++
			if count >= need {
				return true
			}
		}
	}
	return false
}

// commitQuorumLocked reports whether index is stored on a majority of every
// voter set in the current configuration. In a joint configuration that means a
// majority of C_old and a majority of C_new; an old-only or new-only majority is
// not enough.
func (n *Node) commitQuorumInConfigLocked(st configState, index uint64) bool {
	ack := func(id string) bool {
		if id == n.cfg.ID {
			// The leader's own entry is in its log; only a voter in this set
			// reaches this branch through the set iteration.
			return true
		}
		return n.matchIndex[id] >= index
	}
	return n.quorumInConfig(st, ack)
}

// leaseQuorumLocked reports whether the leader has heard from a majority of
// every voter set within the election timeout. While joint, losing either
// majority makes the lease expire.
func (n *Node) leaseQuorumLocked(now time.Time) bool {
	ack := func(id string) bool {
		if id == n.cfg.ID {
			return true
		}
		ts, ok := n.lastAck[id]
		return ok && now.Sub(ts) <= n.cfg.ElectionTimeoutMin
	}
	return n.quorumInLocked(n.voters, ack) &&
		(!n.isJointLocked() || n.quorumInLocked(n.votersOld, ack))
}

// voteQuorumLocked reports whether the collected votes form a majority of every
// voter set. A joint configuration therefore needs majorities of both C_old and
// C_new to elect a leader.
func (n *Node) voteQuorumLocked() bool {
	ack := func(id string) bool { return n.votes[id] }
	return n.quorumInConfig(n.electoralConfigLocked(), ack)
}

// preVoteQuorumLocked applies the same electoral quorum rule to pre-vote
// grants: a majority of C_new and, while joint, a majority of C_old. A
// C_new-only or C_old-only pre-vote majority must not let a candidate start an
// election.
func (n *Node) preVoteQuorumLocked() bool {
	ack := func(id string) bool { return n.preVotes[id] }
	return n.quorumInConfig(n.electoralConfigLocked(), ack)
}

// electoralVotersExceptSelfLocked lists the peers a candidate must ask for a
// vote: the union of the latest voter sets in its log, minus itself.
func (n *Node) electoralVotersExceptSelfLocked() []string {
	members := electoralVotingMembers(n.electoralConfigLocked())
	out := make([]string, 0, len(members))
	for _, v := range members {
		if v != n.cfg.ID {
			out = append(out, v)
		}
	}
	return out
}

// pendingConfigLocked reports whether the log contains an unapplied
// configuration change. Until it is applied, a new configuration change must
// not start: the epochs and voter sets it would compute could race the pending
// entry. Caller holds mu.
func (n *Node) pendingConfigLocked() bool {
	st := n.electoralConfigLocked()
	return st.joint() || st.epoch > n.configEpoch
}

// selfIsElectoralVoterLocked reports whether the local node may campaign.
//
// Every voter of either C_old or C_new may campaign, including a node that only
// C_old still contains while joint. Such a node cannot become leader on its own:
// voteQuorumLocked requires a majority of both sets. But it must be allowed to
// campaign, because it can hold the longest log. A proposal that reached a
// majority of only one side stays uncommitted, yet it makes that side's log
// more up to date; if only C_new voters could campaign, the C_new candidates
// would be rejected by the more up-to-date C_old voters and no leader could be
// elected until a new entry arrived from a leader that does not exist
// (liveness deadlock after a partition). A joint leader that C_new excludes
// steps down once the final entry is applied.
func (n *Node) selfIsElectoralVoterLocked() bool {
	return configVotingMember(n.electoralConfigLocked(), n.cfg.ID)
}

// ---------------------------------------------------------------------------
// Joint configuration APIs
// ---------------------------------------------------------------------------

// EnterJoint appends and applies the intermediate C_old,new configuration,
// where old is the current voter set and new is voters. learners is the final
// learner set. It must be called on the leader. While the node is joint no other
// configuration change is accepted; call LeaveJoint to append C_new. The caller
// is responsible for choosing a configuration whose two majorities it can
// reach: a joint entry commits only when a majority of both C_old and C_new
// acknowledge it.
//
// The single-change APIs (AddLearner, PromoteLearner, RemoveVoter) call this
// with AutoFinalize set and complete the change themselves; EnterJoint is for
// callers that need to hold the joint configuration across several steps.
func (n *Node) EnterJoint(ctx context.Context, voters, learners []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	target, err := normalizeConfigEntryData(ConfigEntryData{
		Voters:   voters,
		Learners: learners,
		Epoch:    1, // structural validation only; the real epoch is set under mu
	})
	if err != nil {
		return err
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
	if n.isJointLocked() {
		n.mu.Unlock()
		return fmt.Errorf("%w: configuration is joint", ErrConfigChangeInProgress)
	}
	if n.configInFlight {
		n.mu.Unlock()
		return ErrConfigChangeInProgress
	}
	data := ConfigEntryData{
		VotersOld: append([]string(nil), n.voters...),
		Voters:    target.Voters,
		Learners:  target.Learners,
		Epoch:     n.configEpoch + 1,
	}
	n.configInFlight = true
	n.mu.Unlock()

	return n.proposeJointConfig(ctx, data)
}

// LeaveJoint appends and applies C_new, leaving the joint configuration. It
// must be called on the leader. It is idempotent on a node that is no longer
// joint (returns ErrNoJointConfig) and waits for the final entry to be applied
// before returning.
func (n *Node) LeaveJoint(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return ErrStopped
	}
	if !n.isJointLocked() {
		n.mu.Unlock()
		return ErrNoJointConfig
	}
	n.mu.Unlock()
	return n.leaveJointInternal(ctx)
}

// proposeJointConfig appends one already-validated joint entry and waits for it
// to apply. Caller has set configInFlight and must not hold mu.
func (n *Node) proposeJointConfig(ctx context.Context, data ConfigEntryData) error {
	value, err := EncodeConfigEntry(data)
	if err != nil {
		n.clearConfigInFlight()
		return err
	}
	_, err = n.Propose(ctx, model.Entry{Type: CommandJointConfig, Value: value})
	if err == nil {
		return nil
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	// The entry may have committed and applied even though the waiter saw a
	// timeout (or cancellation). Report success in that case.
	if n.isJointLocked() && n.configEpoch >= data.Epoch {
		return nil
	}
	// A timeout or cancellation can leave a durable entry that may still
	// commit. Keep the gate closed so a second change cannot overlap; it is
	// cleared when the configuration is applied (or by a step-down).
	if n.role != Leader || n.stopped || !errorsIsRetryableTimeout(err) {
		n.configInFlight = false
	}
	return err
}

// errorsIsRetryableTimeout mirrors the proposal gate rules: only timeout-like
// failures may have left a durable entry behind.
func errorsIsRetryableTimeout(err error) bool {
	return errors.Is(err, ErrTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// leaveJointInternal appends C_new directly (it has no client proposal waiter)
// and waits until the configuration is no longer joint. When a final entry is
// already in flight it waits for that entry instead of appending a duplicate.
func (n *Node) leaveJointInternal(ctx context.Context) error {
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return ErrStopped
	}
	if !n.isJointLocked() {
		n.mu.Unlock()
		return nil
	}
	if n.role != Leader {
		n.mu.Unlock()
		return ErrNotLeader
	}
	expected := n.configEpoch + 1
	if !n.finalInFlight {
		data := ConfigEntryData{
			Voters:   append([]string(nil), n.voters...),
			Learners: n.learnerListLocked(),
			Epoch:    expected,
		}
		value, err := EncodeConfigEntry(data)
		if err != nil {
			n.mu.Unlock()
			return err
		}
		entry := model.Entry{
			Term:  n.term,
			Index: n.lastIndexLocked() + 1,
			Type:  CommandFinalConfig,
			Value: value,
		}
		if err := n.store.Append(entry); err != nil {
			n.mu.Unlock()
			return fmt.Errorf("raft: append final configuration: %w", err)
		}
		n.entries = append(n.entries, entry)
		n.finalInFlight = true
		n.signalAllLocked()
		n.maybeCommitLocked()
	}
	n.mu.Unlock()

	return n.waitConfig(ctx, func() bool {
		return !n.isJointLocked() && n.configEpoch >= expected
	})
}

// clearConfigInFlight clears the one-change gate, unless the node stopped or a
// joint configuration is applied (in which case the gate must stay closed).
func (n *Node) clearConfigInFlight() {
	n.mu.Lock()
	if !n.stopped && !n.isJointLocked() {
		n.configInFlight = false
	}
	n.mu.Unlock()
}

// waitConfig blocks until pred is true, the node stops or ctx ends. pred runs
// with mu held. It uses a broadcast channel rather than polling so an applied
// config wakes every waiter.
func (n *Node) waitConfig(ctx context.Context, pred func() bool) error {
	for {
		n.mu.Lock()
		if n.stopped {
			n.mu.Unlock()
			return ErrStopped
		}
		if pred() {
			n.mu.Unlock()
			return nil
		}
		ch := n.configCh
		n.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return proposalContextError(ctx)
		case <-n.stopCh:
			return ErrStopped
		}
	}
}

// notifyConfigLocked wakes configuration waiters. Caller holds mu.
func (n *Node) notifyConfigLocked() {
	if n.configCh != nil {
		close(n.configCh)
	}
	n.configCh = make(chan struct{})
}

// signalAutoFinalLocked asks the finalizer goroutine to complete an
// auto-finalizing joint configuration. Caller holds mu.
func (n *Node) signalAutoFinalLocked() {
	if !n.isJointLocked() || !n.autoFinalize {
		return
	}
	select {
	case n.autoFinalSignal <- struct{}{}:
	default:
	}
}

// autoFinalizeLoop completes joint configurations created by the single-change
// APIs. It retries while this node remains leader; a new leader also signals it
// on election, so a crash in the joint state is recovered.
func (n *Node) autoFinalizeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stopCh:
			return
		case <-n.autoFinalSignal:
		}
		for {
			n.mu.Lock()
			active := !n.stopped && n.role == Leader && n.isJointLocked() && n.autoFinalize
			n.mu.Unlock()
			if !active {
				break
			}
			timeout := n.configWaitTimeout()
			waitCtx, cancel := context.WithTimeout(ctx, timeout)
			err := n.leaveJointInternal(waitCtx)
			cancel()
			if err == nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-n.stopCh:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// configWaitTimeout bounds one automatic finalization attempt.
func (n *Node) configWaitTimeout() time.Duration {
	d := 10 * n.cfg.HeartbeatInterval
	if d < 500*time.Millisecond {
		d = 500 * time.Millisecond
	}
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

// ---------------------------------------------------------------------------
// Configuration entry application (caller holds n.mu)
// ---------------------------------------------------------------------------

// applyJointConfigLocked applies a committed C_old,new entry. It is
// deterministic and idempotent: an identical entry at the applied epoch is a
// no-op, while an older or conflicting entry is rejected without touching
// state.
func (n *Node) applyJointConfigLocked(entry model.Entry) (bool, error) {
	data, err := DecodeConfigEntry(entry.Value)
	if err != nil {
		return false, err
	}
	data, err = validateJointEntry(data)
	if err != nil {
		return false, err
	}
	if n.matchesJointLocked(data) {
		return false, nil
	}
	if data.Epoch <= n.configEpoch {
		return false, fmt.Errorf("%w: joint epoch %d, applied epoch %d", ErrStaleConfigChange, data.Epoch, n.configEpoch)
	}
	// A strictly increasing epoch is normally applied only when it continues
	// the local configuration (epoch+1, not already joint, old set matches).
	// A gap means intermediate configuration entries were compacted away or
	// this node fell behind; the entry is committed and in our log, so it is
	// the authoritative configuration at this index. Adopting it is the same
	// recovery a snapshot would provide, whereas rejecting it would leave the
	// node permanently behind and unable to apply later config entries.
	if data.Epoch == n.configEpoch+1 && !n.isJointLocked() && equalStrings(data.VotersOld, n.voters) {
		n.votersOld = append([]string(nil), data.VotersOld...)
		n.voters = append([]string(nil), data.Voters...)
		n.learners = stringSet(data.Learners)
		n.autoFinalize = data.AutoFinalize
		n.configEpoch = data.Epoch
		n.finalInFlight = false
		return true, nil
	}
	n.logger.Warn("raft: adopting joint configuration across a log gap",
		"id", n.cfg.ID, "entry_epoch", data.Epoch, "applied_epoch", n.configEpoch,
		"joint", n.isJointLocked(), "entry_old", data.VotersOld, "applied_voters", n.voters)
	n.votersOld = append([]string(nil), data.VotersOld...)
	n.voters = append([]string(nil), data.Voters...)
	n.learners = stringSet(data.Learners)
	n.autoFinalize = data.AutoFinalize
	n.configEpoch = data.Epoch
	n.finalInFlight = false
	return true, nil
}

// applyFinalConfigLocked applies a committed C_new entry, leaving the joint
// configuration. An identical entry at the applied epoch is a no-op; anything
// else that does not exactly complete the current joint configuration is
// rejected.
func (n *Node) applyFinalConfigLocked(entry model.Entry) (bool, error) {
	data, err := DecodeConfigEntry(entry.Value)
	if err != nil {
		return false, err
	}
	data, err = validateFinalEntry(data)
	if err != nil {
		return false, err
	}
	if n.matchesFinalLocked(data) {
		return false, nil
	}
	if data.Epoch <= n.configEpoch {
		return false, fmt.Errorf("%w: final epoch %d, applied epoch %d", ErrStaleConfigChange, data.Epoch, n.configEpoch)
	}
	// The sequential case is a final entry that completes the current joint
	// configuration. A gap (or a non-joint applied state) means intermediate
	// entries were compacted or this node fell behind; the committed final
	// entry is authoritative, so adopt it exactly as a snapshot install would.
	if n.isJointLocked() && data.Epoch == n.configEpoch+1 &&
		equalStrings(data.Voters, n.voters) && equalStrings(data.Learners, n.learnerListLocked()) {
		n.votersOld = nil
		n.voters = append([]string(nil), data.Voters...)
		n.learners = stringSet(data.Learners)
		n.autoFinalize = false
		n.configEpoch = data.Epoch
		n.finalInFlight = false
		n.configInFlight = false
		return true, nil
	}
	n.logger.Warn("raft: adopting final configuration across a log gap",
		"id", n.cfg.ID, "entry_epoch", data.Epoch, "applied_epoch", n.configEpoch,
		"joint", n.isJointLocked(), "entry_voters", data.Voters, "entry_learners", data.Learners,
		"applied_voters", n.voters, "applied_learners", n.learnerListLocked())
	n.votersOld = nil
	n.voters = append([]string(nil), data.Voters...)
	n.learners = stringSet(data.Learners)
	n.autoFinalize = false
	n.configEpoch = data.Epoch
	n.finalInFlight = false
	n.configInFlight = false
	return true, nil
}

// matchesJointLocked reports whether data is exactly the applied joint state.
func (n *Node) matchesJointLocked(data ConfigEntryData) bool {
	return n.isJointLocked() &&
		n.configEpoch == data.Epoch &&
		n.autoFinalize == data.AutoFinalize &&
		equalStrings(data.VotersOld, n.votersOld) &&
		equalStrings(data.Voters, n.voters) &&
		equalStrings(data.Learners, n.learnerListLocked())
}

// matchesFinalLocked reports whether data is exactly the applied final state.
func (n *Node) matchesFinalLocked(data ConfigEntryData) bool {
	return !n.isJointLocked() &&
		n.configEpoch == data.Epoch &&
		!n.autoFinalize &&
		equalStrings(data.Voters, n.voters) &&
		equalStrings(data.Learners, n.learnerListLocked())
}

// stringSet builds a set from ids.
func stringSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return make(map[string]bool)
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// Electoral configuration
// ---------------------------------------------------------------------------
//
// Raft decisions use the latest configuration in a server's log, including a
// config entry that is appended but not yet committed. The applied
// configuration (n.voters/n.votersOld) still governs commits, quorum leases and
// the membership exposed to callers; applying an uncommitted config there could
// expose a configuration that is later truncated. Elections, however, must see
// a config entry already in the log: otherwise a node that has the committed
// joint entry but not the leader's commit index would keep treating itself as a
// learner and refuse to vote, and no leader could ever deliver that commit
// index (a liveness deadlock after a partition).
//
// electoralConfigLocked therefore replays the config entries in the log suffix
// after lastApplied on top of the applied configuration. A pending final entry
// is deliberately not applied here either: until it is committed and applied the
// group is still joint, so elections must reach both majorities. The result is
// used only for campaign eligibility, vote grants and vote counting.

func (n *Node) electoralConfigLocked() configState {
	st := n.configStateLocked()
	last := n.lastIndexLocked()
	for i := n.lastApplied + 1; i <= last; i++ {
		if i <= n.snapshot.Index {
			continue
		}
		entry := n.entries[i-n.snapshot.Index-1]
		st = nextCommitConfig(st, entry)
	}
	return st
}

// nextElectoralConfig applies one config entry to a configuration copy. It
// returns ok=false for a non-config entry or a config entry that does not
// extend the configuration sequentially; the caller stops replaying there.
func nextElectoralConfig(st configState, entry model.Entry) (configState, bool) {
	switch entry.Type {
	case CommandJointConfig:
		data, err := DecodeConfigEntry(entry.Value)
		if err != nil {
			return st, false
		}
		data, err = validateJointEntry(data)
		if err != nil {
			return st, false
		}
		if st.joint() && st.epoch == data.Epoch && st.autoFinalize == data.AutoFinalize &&
			equalStrings(st.votersOld, data.VotersOld) && equalStrings(st.voters, data.Voters) &&
			equalStrings(st.learners, data.Learners) {
			return st, true // duplicate
		}
		if st.joint() || data.Epoch != st.epoch+1 || !equalStrings(data.VotersOld, st.voters) {
			return st, false
		}
		return configState{
			votersOld:    append([]string(nil), data.VotersOld...),
			voters:       append([]string(nil), data.Voters...),
			learners:     append([]string(nil), data.Learners...),
			autoFinalize: data.AutoFinalize,
			epoch:        data.Epoch,
		}, true
	case CommandFinalConfig:
		data, err := DecodeConfigEntry(entry.Value)
		if err != nil {
			return st, false
		}
		data, err = validateFinalEntry(data)
		if err != nil {
			return st, false
		}
		if !st.joint() && st.epoch == data.Epoch && equalStrings(st.voters, data.Voters) &&
			equalStrings(st.learners, data.Learners) {
			return st, true // duplicate
		}
		if !st.joint() || data.Epoch != st.epoch+1 ||
			!equalStrings(data.Voters, st.voters) || !equalStrings(data.Learners, st.learners) {
			return st, false
		}
		return configState{
			voters:       append([]string(nil), data.Voters...),
			learners:     append([]string(nil), data.Learners...),
			autoFinalize: false,
			epoch:        data.Epoch,
		}, true
	case model.CommandAddLearner, model.CommandPromoteLearner, model.CommandRemoveVoter:
		if st.joint() || entry.Key == "" {
			return st, false
		}
		next := st
		next.voters = append([]string(nil), st.voters...)
		next.learners = append([]string(nil), st.learners...)
		beforeVoters := append([]string(nil), next.voters...)
		beforeLearners := append([]string(nil), next.learners...)
		switch entry.Type {
		case model.CommandAddLearner:
			next.voters = removeID(next.voters, entry.Key)
			next.learners = canonicalLearners(append(next.learners, entry.Key), next.voters)
		case model.CommandPromoteLearner:
			if !configNewVoter(st, entry.Key) {
				next.voters = append(next.voters, entry.Key)
				sort.Strings(next.voters)
			}
			next.learners = removeID(next.learners, entry.Key)
		case model.CommandRemoveVoter:
			next.voters = removeID(next.voters, entry.Key)
			next.learners = removeID(next.learners, entry.Key)
		}
		if !equalStrings(next.voters, beforeVoters) || !equalStrings(next.learners, beforeLearners) {
			next.epoch++
		}
		return next, true
	default:
		return st, false
	}
}

// nextCommitConfig advances the configuration that governs commits across one
// log entry. A joint entry takes effect for its own commit, so entering the
// joint state itself requires majorities of both C_old and C_new; this keeps an
// unavailable new voter set from committing a configuration the cluster cannot
// then leave. A final (leave-joint) entry is deliberately not applied here until
// it is committed and applied: it must commit under the joint configuration, and
// a C_new-only majority must never be able to leave the joint state without the
// old majority participating.
func nextCommitConfig(st configState, entry model.Entry) configState {
	switch entry.Type {
	case CommandFinalConfig:
		// Keep the joint configuration until the final entry is applied.
		return st
	default:
		next, ok := nextElectoralConfig(st, entry)
		if !ok {
			return st
		}
		return next
	}
}

// commitConfigThroughLocked returns the commit-governing configuration after
// replaying log entries up to and including index. Caller holds mu.
func (n *Node) commitConfigThroughLocked(st configState, index uint64) configState {
	if index < n.lastApplied+1 {
		return st
	}
	last := n.lastIndexLocked()
	if index > last {
		index = last
	}
	for i := n.lastApplied + 1; i <= index; i++ {
		if i <= n.snapshot.Index {
			continue
		}
		st = nextCommitConfig(st, n.entries[i-n.snapshot.Index-1])
	}
	return st
}

// configVotingMember reports whether id votes in st (C_new or, while joint,
// C_old).
func configVotingMember(st configState, id string) bool {
	return configNewVoter(st, id) || configOldVoter(st, id)
}

// configNewVoter reports whether id is in C_new (the current voter set outside
// a joint configuration). Only C_new voters may campaign.
func configNewVoter(st configState, id string) bool {
	for _, v := range st.voters {
		if v == id {
			return true
		}
	}
	return false
}

func configOldVoter(st configState, id string) bool {
	for _, v := range st.votersOld {
		if v == id {
			return true
		}
	}
	return false
}

// electoralVotingMembers returns the sorted union of the electoral voter sets.
func electoralVotingMembers(st configState) []string {
	seen := make(map[string]bool, len(st.voters)+len(st.votersOld))
	out := make([]string, 0, len(st.voters)+len(st.votersOld))
	for _, v := range st.voters {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range st.votersOld {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// quorumInConfig requires a majority of every voter set in st.
func (n *Node) quorumInConfig(st configState, ack func(string) bool) bool {
	if !n.quorumInLocked(st.voters, ack) {
		return false
	}
	return !st.joint() || n.quorumInLocked(st.votersOld, ack)
}

func removeID(ids []string, id string) []string {
	out := ids[:0]
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}
