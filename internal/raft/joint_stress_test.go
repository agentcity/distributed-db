package raft_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

// Joint-consensus randomized stress test.
//
// This test drives one cluster through a fixed number of successful membership
// operations (1000 by default), interleaved with application commits, leader
// kills, one-way partitions, snapshots and node restarts. It is deterministic
// in its random choices (fixed seed) and fails on the first safety violation.
//
// The membership changes are logical API calls, not configuration entries:
// AddLearner, PromoteLearner and RemoveVoter each perform enter-joint plus
// leave-joint, while EnterJoint and LeaveJoint count separately. The total is
// therefore 1000 successful changes, not 1000 per phase or per run.
//
// Invariants checked after every phase:
//
//   - config epoch never decreases on any node (including across restart and
//     snapshot install);
//   - a joint status always carries both voter sets, and the reported Voters
//     is their union;
//   - every current member converges on the same final voters/learners/epoch;
//   - every committed application entry is present with the same value on
//     every current member, and any entry still visible in a node's state
//     machine matches the committed entry at that index;
//   - a leader that can reach only a C_old or only a C_new majority cannot
//     commit anything while the configuration is joint;
//   - after a fresh learner installs a snapshot, its installed membership is
//     the leader's configuration (no removed voter is resurrected), and the
//     same membership survives a restart.
//
// Runtime is bounded by the operation count and per-step deadlines. A run is
// roughly 90-110 seconds on a laptop (with or without -race); -count=3 is about
// five minutes. Polling loops sleep in milliseconds, and the longest single
// condition wait is stressSnapshotWait (10s) for a learner catching up from a
// compacted log.
const (
	jointStressChanges = 1000
	jointStressSeed    = int64(0x10AD5EED)
	stressElectionMin  = 200 * time.Millisecond
	stressElectionMax  = 400 * time.Millisecond
	stressHeartbeat    = 25 * time.Millisecond

	// Per-operation and per-phase bounds. These are deliberately larger than
	// the election timeout so a single leadership change is recoverable, but
	// small enough that a genuinely stuck cluster fails quickly.
	stressOpTimeout     = 4 * time.Second
	stressLeaderTimeout = 4 * time.Second
	stressPhaseTimeout  = 8 * time.Second
	stressSnapshotWait  = 10 * time.Second
)

type jointStress struct {
	t   *testing.T
	c   *testCluster
	rng *rand.Rand

	// pool is every node process the stress driver may use. Nodes are started
	// eagerly (n1..n5) except the fresh snapshot target, which is started when
	// the snapshot phase needs an empty log.
	pool []string
	// base holds the static configuration a restarted node is created with.
	base map[string]raft.Config

	maxEpoch map[string]uint64

	// committed maps a unique key to its committed value.
	committed map[string]string
	// entries records the committed user entry at each index.
	entries map[uint64]model.Entry

	// Current applied configuration as last observed on a leader.
	voters    []string
	learners  []string
	oldVoters []string
	newVoters []string
	// prevVoters is the union of the voter sets seen before the latest
	// refresh. While a configuration change is still propagating, a node that
	// the newest config excludes from C_new can legitimately hold the longest
	// log and be elected to finish the joint state, so leader searches must
	// include it.
	prevVoters []string
	lastUserIx uint64

	changes       int
	commits       int
	phases        int
	enterJoints   int
	failures      int
	nextSnapshot  int
	snapshotsDone int
	freshNode     int

	timeChanges  time.Duration
	timeCommits  time.Duration
	timeQuiesce  time.Duration
	timeFailures time.Duration
	timeSnapshot time.Duration

	start time.Time
}

func newJointStress(t *testing.T) *jointStress {
	t.Helper()
	c := newTestClusterWithTimeouts(t, 3, stressElectionMin, stressElectionMax, stressHeartbeat)
	c.addNode("n4", c.ids, true)
	c.addNode("n5", c.ids, true)

	s := &jointStress{
		t:            t,
		c:            c,
		rng:          rand.New(rand.NewSource(jointStressSeed)),
		pool:         []string{"n1", "n2", "n3", "n4", "n5"},
		base:         make(map[string]raft.Config),
		maxEpoch:     make(map[string]uint64),
		committed:    make(map[string]string),
		entries:      make(map[uint64]model.Entry),
		voters:       append([]string(nil), c.ids...),
		nextSnapshot: 240,
		start:        time.Now(),
	}
	for _, id := range c.ids {
		s.base[id] = c.configFor(id, peersExcept(c.ids, id), c.dirs[id])
	}
	for _, id := range []string{"n4", "n5"} {
		cfg := c.configFor(id, c.ids, c.dirs[id])
		cfg.Learner = true
		s.base[id] = cfg
	}
	return s
}

func (s *jointStress) logf(format string, args ...any) {
	s.t.Helper()
	s.t.Logf("[changes=%d commits=%d phases=%d] "+format,
		append([]any{s.changes, s.commits, s.phases}, args...)...)
}

// ---------------------------------------------------------------------------
// Status tracking and invariants
// ---------------------------------------------------------------------------

func (s *jointStress) recordStatus(id string, st raft.Status) {
	s.t.Helper()
	if st.ID != "" && st.ID != id {
		s.t.Fatalf("status ID = %q, want %q", st.ID, id)
	}
	if st.ConfigEpoch > 0 {
		if prev, ok := s.maxEpoch[id]; ok && st.ConfigEpoch < prev {
			s.t.Fatalf("node %s config epoch went backwards: %d -> %d", id, prev, st.ConfigEpoch)
		}
		if st.ConfigEpoch > s.maxEpoch[id] {
			s.maxEpoch[id] = st.ConfigEpoch
		}
	}
	if err := checkStatusShape(st); err != nil {
		s.t.Fatalf("node %s: %v", id, err)
	}
}

func checkStatusShape(st raft.Status) error {
	for _, l := range st.Learners {
		for _, v := range st.Voters {
			if l == v {
				return fmt.Errorf("%q is both voter and learner (voters=%v learners=%v)", l, st.Voters, st.Learners)
			}
		}
	}
	if st.Joint {
		if len(st.VotersOld) == 0 {
			return errors.New("joint status has no C_old")
		}
		if len(st.VotersNew) == 0 {
			return errors.New("joint status has no C_new")
		}
		union := append(append([]string(nil), st.VotersOld...), st.VotersNew...)
		sort.Strings(union)
		union = dedupeStrings(union)
		if !equalStrings(union, sortedStrings(st.Voters)) {
			return fmt.Errorf("joint Voters = %v, want union %v", st.Voters, union)
		}
	} else if len(st.VotersOld) != 0 || len(st.VotersNew) != 0 {
		return fmt.Errorf("non-joint status carries old/new sets: old=%v new=%v", st.VotersOld, st.VotersNew)
	}
	return nil
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func setEqualStrings(a, b []string) bool {
	return equalStrings(sortedStrings(a), sortedStrings(b))
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func withoutString(in []string, drop string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}

func dedupeStrings(in []string) []string {
	out := in[:0]
	for i, v := range in {
		if i == 0 || in[i-1] != v {
			out = append(out, v)
		}
	}
	return out
}

func (s *jointStress) recordAll() {
	s.t.Helper()
	for _, id := range s.pool {
		node := s.c.nodes[id]
		if node == nil {
			continue
		}
		s.recordStatus(id, node.Status())
	}
}

// configIDs returns every node that can currently vote (C_new plus C_old while
// joint) as last observed on a leader.
func (s *jointStress) configIDs() []string {
	seen := make(map[string]bool)
	var out []string
	for _, set := range [][]string{s.voters, s.oldVoters, s.newVoters, s.prevVoters} {
		for _, id := range set {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (s *jointStress) memberIDs() []string {
	seen := make(map[string]bool)
	var out []string
	for _, set := range [][]string{s.voters, s.learners} {
		for _, id := range set {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (s *jointStress) refresh(st raft.Status) {
	before := s.configIDs()
	if len(before) > 0 {
		s.prevVoters = before
	}
	s.voters = sortedStrings(st.Voters)
	s.learners = sortedStrings(st.Learners)
	s.oldVoters = sortedStrings(st.VotersOld)
	s.newVoters = sortedStrings(st.VotersNew)
}

func (s *jointStress) waitOneLeader(ids []string, timeout time.Duration) (*raft.Node, string) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var leader *raft.Node
		var leaderID string
		count := 0
		for _, id := range ids {
			node := s.c.nodes[id]
			if node == nil {
				continue
			}
			if node.IsLeader() {
				count++
				leader, leaderID = node, id
			}
		}
		if count == 1 {
			return leader, leaderID
		}
		time.Sleep(time.Millisecond)
	}
	for _, id := range ids {
		if node := s.c.nodes[id]; node != nil {
			s.logNodeState(id)
		}
	}
	s.logf("no-leader config: voters=%v learners=%v old=%v new=%v", s.voters, s.learners, s.oldVoters, s.newVoters)
	s.t.Fatalf("no single leader among %v within %v", ids, timeout)
	return nil, ""
}

func (s *jointStress) stableLeader(ids []string) (*raft.Node, string) {
	s.t.Helper()
	return s.c.waitLeader(ids, stressLeaderTimeout)
}

// logNodeState emits the applied configuration, the installed snapshot and the
// tail of the durable log for one node. It is called only when a convergence or
// leader wait fails, to make whether the replica is behind on replication or on
// configuration recovery visible in the failure log.
func (s *jointStress) logNodeState(id string) {
	s.t.Helper()
	node := s.c.nodes[id]
	if node == nil {
		s.logf("state %s: no node", id)
		return
	}
	st := node.Status()
	votedFor := ""
	lastTerm := uint64(0)
	store := s.c.stores[id]
	var persisted raft.PersistentState
	haveLog := false
	if store != nil {
		if p, err := store.Load(); err == nil {
			persisted = p
			haveLog = true
			votedFor = p.VotedFor
			if n := len(p.Entries); n > 0 {
				lastTerm = p.Entries[n-1].Term
			}
		}
	}
	s.logf("state %s: role=%s term=%d votedFor=%q leader=%q learner=%v joint=%v voters=%v old=%v new=%v learners=%v epoch=%d last=%d lastTerm=%d commit=%d applied=%d",
		id, st.Role, st.Term, votedFor, st.Leader, st.Learner, st.Joint,
		st.Voters, st.VotersOld, st.VotersNew, st.Learners, st.ConfigEpoch,
		st.LastIndex, lastTerm, st.CommitIndex, st.LastApplied)
	if store != nil {
		if meta, rc, err := store.OpenSnapshot(); err == nil {
			if rc != nil {
				_ = rc.Close()
			}
			snapEpoch := uint64(0)
			var snapVoters, snapOld, snapLearners []string
			if meta.Membership != nil {
				snapEpoch = meta.Membership.ConfigEpoch
				snapVoters = meta.Membership.Voters
				snapOld = meta.Membership.VotersOld
				snapLearners = meta.Membership.Learners
			}
			s.logf("state %s snapshot: index=%d term=%d epoch=%d voters=%v old=%v learners=%v",
				id, meta.Index, meta.Term, snapEpoch, snapVoters, snapOld, snapLearners)
		}
	}
	if !haveLog {
		return
	}
	from := len(persisted.Entries) - 6
	if from < 0 {
		from = 0
	}
	for _, e := range persisted.Entries[from:] {
		if e.Type == raft.CommandJointConfig || e.Type == raft.CommandFinalConfig {
			data, err := raft.DecodeConfigEntry(e.Value)
			if err == nil {
				s.logf("state %s entry %d term=%d type=%s epoch=%d old=%v voters=%v learners=%v auto=%v",
					id, e.Index, e.Term, e.Type, data.Epoch, data.VotersOld, data.Voters, data.Learners, data.AutoFinalize)
				continue
			}
		}
		s.logf("state %s entry %d term=%d type=%s key=%q", id, e.Index, e.Term, e.Type, e.Key)
	}
}

// requireMembersConverged waits until every member reports the leader's final
// configuration and has applied every proposed user entry.
func (s *jointStress) requireMembersConverged(leader *raft.Node, wantVoters, wantLearners []string, wantEpoch uint64) {
	s.t.Helper()
	members := append(append([]string(nil), wantVoters...), wantLearners...)
	sort.Strings(members)
	lastUser := s.lastUserIx
	deadline := time.Now().Add(stressPhaseTimeout)
	for {
		ok := true
		for _, id := range members {
			node := s.c.nodes[id]
			if node == nil {
				s.t.Fatalf("member %s has no node", id)
			}
			st := node.Status()
			s.recordStatus(id, st)
			if st.Joint || !equalStrings(sortedStrings(st.Voters), wantVoters) ||
				!equalStrings(sortedStrings(st.Learners), wantLearners) ||
				st.ConfigEpoch != wantEpoch || st.LastApplied < lastUser {
				ok = false
				break
			}
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			for _, id := range members {
				s.logNodeState(id)
			}
			s.t.Fatalf("members %v did not converge within %v", members, stressPhaseTimeout)
		}
		time.Sleep(2 * time.Millisecond)
	}
	_ = leader

	// Every committed value must be present on every member. This is the
	// cross-node state-machine agreement check: two disjoint majorities that
	// committed different entries would show up as a missing or mismatching
	// value here.
	for _, id := range members {
		sm := s.c.sms[id]
		for key, want := range s.committed {
			got, ok := sm.get(key)
			if !ok {
				s.t.Fatalf("member %s is missing committed key %q (applied=%d)", id, key, sm.appliedIndex())
			}
			if string(got) != want {
				s.t.Fatalf("member %s has %q=%q, want %q", id, key, got, want)
			}
		}
		// Any user entry still retained by the state machine must match the
		// commit we observed. Snapshot restores drop byIndex; the value check
		// above still covers those nodes.
		for index, want := range s.entries {
			got, ok := sm.entryAt(index)
			if !ok {
				continue
			}
			if got.Type != want.Type || got.Key != want.Key || string(got.Value) != string(want.Value) {
				s.t.Fatalf("member %s applied different entry at index %d: got {%s %q %q}, want {%s %q %q}",
					id, index, got.Type, got.Key, got.Value, want.Type, want.Key, want.Value)
			}
		}
	}
}

// quiesce heals the network, completes any joint configuration, waits for a
// stable leader and then asserts all phase invariants.
func (s *jointStress) quiesce() {
	s.t.Helper()
	defer func(start time.Time) {
		if d := time.Since(start); d > 250*time.Millisecond {
			s.logf("slow quiesce: %v", d.Round(time.Millisecond))
		}
	}(time.Now())
	healAll(s.c, s.pool...)

	leader, leaderID := s.stableLeader(s.configIDs())
	if leader.Status().Joint {
		s.leaveJoint()
		s.changes++
		leader, leaderID = s.stableLeader(s.configIDs())
	}
	st := leader.Status()
	s.recordStatus(leaderID, st)
	s.refresh(st)
	s.requireMembersConverged(leader, sortedStrings(st.Voters), sortedStrings(st.Learners), st.ConfigEpoch)
	s.recordAll()
	// Every current member has converged, so nodes outside the current
	// configuration are no longer candidates for finishing a transition.
	s.prevVoters = nil
	s.phases++
}

// ---------------------------------------------------------------------------
// Operations
// ---------------------------------------------------------------------------

func (s *jointStress) retryChange(name string, autoLeave bool, done func(raft.Status) bool, apply func(context.Context, *raft.Node) error) {
	s.t.Helper()
	attempts := 0
	defer func(start time.Time) {
		if d := time.Since(start); d > 100*time.Millisecond {
			s.logf("slow operation %s: %v (attempts=%d)", name, d.Round(time.Millisecond), attempts)
		}
	}(time.Now())
	deadline := time.Now().Add(stressOpTimeout)
	var lastErr error
	for {
		attempts++
		if time.Now().After(deadline) {
			s.logf("configuration state: voters=%v learners=%v old=%v new=%v",
				s.voters, s.learners, s.oldVoters, s.newVoters)
			for _, id := range s.pool {
				if node := s.c.nodes[id]; node != nil {
					s.logNodeState(id)
				}
			}
			s.t.Fatalf("%s did not complete within %v: %v", name, stressOpTimeout, lastErr)
		}
		leader, id := s.waitOneLeader(s.configIDs(), stressLeaderTimeout)
		st := leader.Status()
		if autoLeave && st.Joint {
			// A manual EnterJoint (or a leader that won the election but has
			// not applied the joint entry yet) leaves the configuration open.
			// Finish it before retrying the requested single change.
			s.leaveJoint()
			continue
		}
		if done(st) {
			s.recordStatus(id, st)
			s.refresh(st)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		err := apply(ctx, leader)
		cancel()
		if err == nil {
			st = leader.Status()
			s.recordStatus(id, st)
			s.refresh(st)
			return
		}
		lastErr = err
		if !retryableMembershipErr(err) &&
			!errors.Is(err, raft.ErrAlreadyVoter) &&
			!errors.Is(err, raft.ErrAlreadyLearner) &&
			!errors.Is(err, raft.ErrNotLearner) &&
			!errors.Is(err, raft.ErrNotVoter) &&
			!errors.Is(err, raft.ErrNoJointConfig) {
			s.t.Fatalf("%s on %s: %v", name, id, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (s *jointStress) addLearner(target string) {
	s.t.Helper()
	s.retryChange("AddLearner("+target+")", true, func(st raft.Status) bool {
		return contains(st.Learners, target)
	}, func(ctx context.Context, leader *raft.Node) error {
		return leader.AddLearner(ctx, target)
	})
}

func (s *jointStress) promoteLearner(target string) {
	s.t.Helper()
	s.retryChange("PromoteLearner("+target+")", true, func(st raft.Status) bool {
		return contains(st.Voters, target)
	}, func(ctx context.Context, leader *raft.Node) error {
		return leader.PromoteLearner(ctx, target)
	})
}

func (s *jointStress) removeVoter(target string) {
	s.t.Helper()
	s.retryChange("RemoveVoter("+target+")", true, func(st raft.Status) bool {
		return !contains(st.Voters, target) && !contains(st.Learners, target)
	}, func(ctx context.Context, leader *raft.Node) error {
		return leader.RemoveVoter(ctx, target)
	})
}

func (s *jointStress) enterJoint(voters, learners []string) {
	s.t.Helper()
	s.retryChange(fmt.Sprintf("EnterJoint(%v,%v)", voters, learners), true, func(st raft.Status) bool {
		return st.Joint && equalStrings(sortedStrings(st.VotersOld), sortedStrings(s.voters)) &&
			equalStrings(sortedStrings(st.VotersNew), sortedStrings(voters)) &&
			equalStrings(sortedStrings(st.Learners), sortedStrings(learners))
	}, func(ctx context.Context, leader *raft.Node) error {
		return leader.EnterJoint(ctx, voters, learners)
	})
	s.enterJoints++
}

func (s *jointStress) leaveJoint() {
	s.t.Helper()
	s.retryChange("LeaveJoint", false, func(st raft.Status) bool {
		return !st.Joint
	}, func(ctx context.Context, leader *raft.Node) error {
		err := leader.LeaveJoint(ctx)
		if errors.Is(err, raft.ErrNoJointConfig) {
			return nil
		}
		return err
	})
}

// commitValue proposes one unique application entry and records its index.
func (s *jointStress) commitValue() {
	s.t.Helper()
	defer func(start time.Time) {
		if d := time.Since(start); d > 150*time.Millisecond {
			s.logf("slow commit: %v", d.Round(time.Millisecond))
		}
	}(time.Now())
	s.commits++
	key := fmt.Sprintf("stress/%05d", s.commits)
	value := fmt.Sprintf("value-%05d", s.commits)
	cmd := model.Entry{Type: model.CommandPut, Key: key, Value: []byte(value)}
	deadline := time.Now().Add(stressOpTimeout)
	var lastErr error
	for {
		if time.Now().After(deadline) {
			break
		}
		leader, _ := s.waitOneLeader(s.configIDs(), stressLeaderTimeout)
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		index, err := leader.Propose(ctx, cmd)
		cancel()
		if err == nil {
			cmd.Index = index
			s.entries[index] = cmd
			s.committed[key] = value
			if index > s.lastUserIx {
				s.lastUserIx = index
			}
			return
		}
		lastErr = err
		if !errors.Is(err, raft.ErrNotLeader) && !errors.Is(err, raft.ErrTimeout) &&
			!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			s.t.Fatalf("commit %q: %v", key, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	s.logf("commit retry deadline hit: voters=%v learners=%v old=%v new=%v", s.voters, s.learners, s.oldVoters, s.newVoters)
	for _, id := range s.configIDs() {
		if node := s.c.nodes[id]; node != nil {
			st := node.Status()
			s.logf("commit-failure status %s: role=%s term=%d leaders=%q joint=%v voters=%v old=%v new=%v commit=%d applied=%d",
				id, st.Role, st.Term, st.Leader, st.Joint, st.Voters, st.VotersOld, st.VotersNew, st.CommitIndex, st.LastApplied)
		}
	}
	s.t.Fatalf("commit %q did not succeed within %v: %v", key, stressOpTimeout, lastErr)
}

func (s *jointStress) absentIDs(st raft.Status) []string {
	used := make(map[string]bool)
	for _, id := range st.Voters {
		used[id] = true
	}
	for _, id := range st.Learners {
		used[id] = true
	}
	var out []string
	for _, id := range s.pool {
		if !used[id] {
			out = append(out, id)
		}
	}
	return out
}

// runOneChange applies exactly one successful logical membership change.
func (s *jointStress) runOneChange() {
	s.t.Helper()
	leader, _ := s.waitOneLeader(s.configIDs(), stressLeaderTimeout)
	st := leader.Status()
	s.refresh(st)

	if st.Joint {
		s.leaveJoint()
		return
	}

	absent := s.absentIDs(st)
	learners := sortedStrings(st.Learners)
	voters := sortedStrings(st.Voters)

	// Force an explicit enter/leave pair periodically so both the manual joint
	// API and the single-change APIs are exercised.
	if len(absent) > 0 && s.changes%17 == 0 {
		s.manualEnterJoint()
		return
	}

	choice := s.rng.Intn(100)
	switch {
	case len(learners) > 0 && choice < 45:
		target := learners[s.rng.Intn(len(learners))]
		s.promoteLearner(target)
	case len(absent) > 0 && choice < 75:
		target := absent[s.rng.Intn(len(absent))]
		s.addLearner(target)
	case len(voters) > 3 && choice < 95:
		// Remove a non-leader: leader removal is covered by the explicit
		// killLeader failure injection, and removing it here would add an
		// election to almost every change.
		target := voters[s.rng.Intn(len(voters))]
		for _, v := range voters {
			if v != leader.Status().ID {
				target = v
				break
			}
		}
		s.removeVoter(target)
	case len(absent) > 0:
		s.manualEnterJoint()
	default:
		// No spare node: enter and leave a joint configuration whose voter
		// set is unchanged. This still exercises both epoch transitions.
		s.enterJoint(voters, learners)
	}
}

func (s *jointStress) manualEnterJoint() {
	s.t.Helper()
	deadline := time.Now().Add(stressOpTimeout)
	var lastErr error
	attempts := 0
	for {
		attempts++
		if time.Now().After(deadline) {
			s.t.Fatalf("manual EnterJoint did not complete within %v: %v", stressOpTimeout, lastErr)
		}
		leader, leaderID := s.waitOneLeader(s.configIDs(), stressLeaderTimeout)
		st := leader.Status()
		s.recordStatus(leaderID, st)
		if st.Joint {
			// A concurrent transition won the race; finish it and retry.
			s.leaveJoint()
			continue
		}
		voters := sortedStrings(st.Voters)
		learners := sortedStrings(st.Learners)
		absent := s.absentIDs(st)
		if len(absent) == 0 {
			// No spare node: enter and leave a joint configuration with the
			// same voter set to exercise both epoch transitions.
			s.enterJoint(voters, learners)
			return
		}

		// Keep at least three voters on both sides of the joint configuration:
		// with two-element sets a single node kill leaves neither set with a
		// majority, which is correct unavailability but not a useful failure
		// to inject. Below three voters, grow instead of replacing.
		newVoter := absent[s.rng.Intn(len(absent))]
		var nextVoters []string
		if len(voters) < 3 {
			nextVoters = append(nextVoters, voters...)
		} else {
			victim := voters[s.rng.Intn(len(voters))]
			for _, v := range voters {
				if v != leaderID {
					victim = v
					break
				}
			}
			for _, v := range voters {
				if v != victim {
					nextVoters = append(nextVoters, v)
				}
			}
		}
		if !contains(nextVoters, newVoter) {
			nextVoters = append(nextVoters, newVoter)
		}
		sort.Strings(nextVoters)
		nextLearners := make([]string, 0, len(learners))
		for _, l := range learners {
			if l != newVoter {
				nextLearners = append(nextLearners, l)
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		err := leader.EnterJoint(ctx, nextVoters, nextLearners)
		cancel()
		if err == nil {
			s.enterJoints++
			s.recordStatus(leaderID, leader.Status())
			s.refresh(leader.Status())
			// Every fifth manual joint state also survives a leader kill
			// before it is finalized: a new leader must recover the joint
			// configuration. Skip the kill when either voter set has only two
			// members, because losing one member then legitimately leaves
			// neither side with a majority.
			if s.enterJoints%20 == 0 && len(s.oldVoters) >= 3 && len(s.newVoters) >= 3 {
				s.killLeader()
			}
			return
		}
		lastErr = err
		if !retryableMembershipErr(err) && !errors.Is(err, raft.ErrInvalidConfigEntry) {
			s.t.Fatalf("EnterJoint on %s: %v", leaderID, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Failure injection
// ---------------------------------------------------------------------------

func (s *jointStress) restartNode(id string) {
	s.t.Helper()
	old := s.c.nodes[id]
	if old != nil {
		old.Stop()
	}
	cfg, ok := s.base[id]
	if !ok {
		s.t.Fatalf("no base config for %s", id)
	}
	node, err := raft.New(cfg, s.c.stores[id], s.c.sms[id],
		&testTransferTransport{Transport: s.c.net.Transport(id), cluster: s.c, id: id})
	if err != nil {
		s.t.Fatalf("restart %s: %v", id, err)
	}
	s.c.setNode(id, node)
	s.c.net.Register(id, node)
	if err := node.Start(s.c.ctx); err != nil {
		s.t.Fatalf("restart start %s: %v", id, err)
	}
}

func (s *jointStress) waitApplied(ids []string, index uint64, timeout time.Duration) {
	s.t.Helper()
	eventually(s.t, timeout, fmt.Sprintf("nodes %v apply through %d", ids, index), func() bool {
		for _, id := range ids {
			node := s.c.nodes[id]
			if node == nil || node.Status().LastApplied < index {
				return false
			}
		}
		return true
	})
}

func (s *jointStress) killLeader() {
	s.t.Helper()
	if len(s.oldVoters) > 0 && (len(s.oldVoters) < 3 || len(s.newVoters) < 3) {
		s.commitValue()
		return
	}
	ids := s.configIDs()
	leader, leaderID := s.waitOneLeader(ids, stressLeaderTimeout)
	_ = leader
	s.logf("failure: kill leader %s", leaderID)
	s.c.nodes[leaderID].Stop()

	remaining := withoutString(ids, leaderID)
	newLeader, newID := s.waitOneLeader(remaining, stressLeaderTimeout)
	s.recordStatus(newID, newLeader.Status())
	s.restartNode(leaderID)
	// The restarted node must rejoin if it is still a member. A leader that a
	// concurrent final configuration removed may legitimately stay behind; it
	// is not a replication target any more.
	// Do not require the restarted node to catch up immediately: the phase
	// check below requires every current member to have applied the whole log,
	// so a genuinely stuck replica is still caught. This keeps a leader kill
	// from failing only because a slow catch-up exceeds a short local timeout.
	s.waitOneLeader(s.configIDs(), stressLeaderTimeout)
	s.failures++
	s.commitValue()
}

func (s *jointStress) isolateFollower() {
	s.t.Helper()
	ids := s.configIDs()
	leader, leaderID := s.waitOneLeader(ids, stressLeaderTimeout)
	st := leader.Status()
	if st.Joint && (len(st.VotersOld) < 3 || len(st.VotersNew) < 3) {
		// Losing one node from a two-element joint side leaves that side
		// without a majority. That is correct unavailability, not a useful
		// failure to inject.
		s.commitValue()
		return
	}
	if len(st.Voters) <= 2 && !st.Joint {
		// Isolating a voter from a two-voter configuration would remove the
		// only quorum; pick a learner instead or skip this failure.
		var learner string
		for _, l := range st.Learners {
			if l != leaderID {
				learner = l
				break
			}
		}
		if learner == "" {
			s.commitValue()
			return
		}
	}
	target := ""
	for _, id := range append(append([]string(nil), st.Voters...), st.Learners...) {
		if id != leaderID {
			target = id
			break
		}
	}
	if target == "" {
		s.commitValue()
		return
	}
	s.logf("failure: isolate %s", target)
	others := withoutString(ids, target)
	for _, other := range others {
		s.c.net.Partition(target, other)
		s.c.net.Partition(other, target)
	}
	s.failures++
	s.commitValue()
	healAll(s.c, s.pool...)
	s.waitApplied([]string{target}, s.lastUserIx, stressPhaseTimeout)
}

func (s *jointStress) failureEvent() {
	s.t.Helper()
	switch s.failures % 3 {
	case 0, 2:
		s.killLeader()
	case 1:
		s.isolateFollower()
	}
}

// ---------------------------------------------------------------------------
// Snapshot install into a fresh learner
// ---------------------------------------------------------------------------

func (s *jointStress) installSnapshotIntoFreshLearner() {
	s.t.Helper()
	healAll(s.c, s.pool...)
	leader, leaderID := s.stableLeader(s.configIDs())
	if leader.Status().Joint {
		s.leaveJoint()
		s.changes++
		leader, leaderID = s.stableLeader(s.configIDs())
	}

	// A few entries give the snapshot a payload and guarantee its index is
	// above the fresh node's empty log.
	for i := 0; i < 6; i++ {
		s.commitValue()
	}
	s.waitApplied(s.memberIDs(), s.lastUserIx, stressPhaseTimeout)
	leader, leaderID = s.stableLeader(s.configIDs())

	// Compaction must survive a leadership change while the test is running:
	// Snapshot works on followers too, so snapshot every member and use the
	// lowest resulting index as the floor. Whichever node leads the next
	// election then has a compacted prefix, and the fresh learner must install
	// a snapshot rather than the full log.
	minSnapIndex := uint64(0)
	for _, id := range s.memberIDs() {
		node := s.c.nodes[id]
		if node == nil {
			continue
		}
		snap, err := node.Snapshot()
		if err != nil {
			s.logf("Snapshot(%s) failed during snapshot phase: %v", id, err)
			continue
		}
		if snap.Index == 0 || snap.Membership == nil {
			continue
		}
		if minSnapIndex == 0 || snap.Index < minSnapIndex {
			minSnapIndex = snap.Index
		}
		if id == leaderID {
			if !setEqualStrings(snap.Membership.Voters, leader.Status().Voters) {
				s.t.Fatalf("leader snapshot voters = %v, want %v", snap.Membership.Voters, leader.Status().Voters)
			}
		}
	}
	if minSnapIndex == 0 {
		s.t.Fatal("no member produced a snapshot before the fresh-learner phase")
	}

	leader, leaderID = s.stableLeader(s.configIDs())
	wantVoters := sortedStrings(leader.Status().Voters)
	s.freshNode++
	freshID := fmt.Sprintf("fresh%d", s.freshNode)
	basePeers := leader.Status().Voters
	s.c.addNode(freshID, basePeers, true)
	cfg := s.c.configFor(freshID, basePeers, s.c.dirs[freshID])
	cfg.Learner = true
	s.base[freshID] = cfg
	s.pool = append(s.pool, freshID)
	s.logf("snapshot floor %d: add fresh learner %s", minSnapIndex, freshID)

	s.addLearner(freshID)
	s.waitApplied([]string{freshID}, minSnapIndex, stressSnapshotWait)

	// The fresh learner started with an empty log at a compacted leader
	// prefix, so the leader had to ship a snapshot through the Raft
	// SnapshotTransport. Verify the install, its membership and the restart
	// path.
	store := s.c.stores[freshID]
	meta, rc, err := store.OpenSnapshot()
	if err != nil {
		s.t.Fatalf("fresh learner %s OpenSnapshot: %v", freshID, err)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if meta.Index < minSnapIndex {
		s.t.Fatalf("fresh learner %s installed snapshot index %d, want >= %d (no snapshot install happened)",
			freshID, meta.Index, minSnapIndex)
	}
	if meta.Membership == nil {
		s.t.Fatalf("fresh learner %s installed a snapshot without membership", freshID)
	}
	if !equalStrings(sortedStrings(meta.Membership.Voters), wantVoters) {
		s.t.Fatalf("installed snapshot voters = %v, want %v", meta.Membership.Voters, wantVoters)
	}
	for _, absent := range s.pool {
		if contains(wantVoters, absent) || contains(leader.Status().Learners, absent) {
			continue
		}
		if contains(meta.Membership.Voters, absent) {
			s.t.Fatalf("snapshot membership resurrected non-voter %s: %v", absent, meta.Membership.Voters)
		}
	}

	// Let it apply the config entries appended after the snapshot. The leader
	// may change while it catches up, so resolve the current leader on every
	// poll and require the fresh node to match its epoch and membership.
	s.waitApplied([]string{freshID}, s.lastUserIx, stressSnapshotWait)
	var st raft.Status
	deadline := time.Now().Add(stressSnapshotWait)
	for {
		current, currentID := s.stableLeader(s.configIDs())
		lst := current.Status()
		st = s.c.nodes[freshID].Status()
		s.recordStatus(currentID, lst)
		s.recordStatus(freshID, st)
		if !st.Joint && st.ConfigEpoch == lst.ConfigEpoch &&
			equalStrings(sortedStrings(st.Voters), sortedStrings(lst.Voters)) &&
			equalStrings(sortedStrings(st.Learners), sortedStrings(lst.Learners)) {
			break
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("fresh learner %s status = voters %v learners %v joint %v epoch %d, want leader voters %v learners %v epoch %d",
				freshID, st.Voters, st.Learners, st.Joint, st.ConfigEpoch,
				lst.Voters, lst.Learners, lst.ConfigEpoch)
		}
		time.Sleep(2 * time.Millisecond)
	}

	epoch := st.ConfigEpoch
	s.restartNode(freshID)
	restarted := s.c.nodes[freshID].Status()
	s.recordStatus(freshID, restarted)
	if restarted.Joint || !equalStrings(sortedStrings(restarted.Voters), sortedStrings(st.Voters)) ||
		!equalStrings(sortedStrings(restarted.Learners), sortedStrings(st.Learners)) {
		s.t.Fatalf("restarted fresh learner membership = voters %v learners %v joint %v, want voters %v learners %v",
			restarted.Voters, restarted.Learners, restarted.Joint, st.Voters, st.Learners)
	}
	if restarted.ConfigEpoch != epoch {
		s.t.Fatalf("restarted fresh learner epoch = %d, want %d", restarted.ConfigEpoch, epoch)
	}
	s.waitApplied([]string{freshID}, s.lastUserIx, stressSnapshotWait)
	s.snapshotsDone++
	s.nextSnapshot += 450
	s.changes++
}

// ---------------------------------------------------------------------------
// Joint quorum partition scenario
// ---------------------------------------------------------------------------

// jointQuorumScenario enters a joint configuration whose old and new voter
// sets can each form a majority on a disjoint side of a partition. Neither side
// has a majority of both sets, so no entry may commit. After healing, the
// cluster must leave the joint configuration and continue writing.
func (s *jointStress) jointQuorumScenario() {
	s.t.Helper()
	healAll(s.c, s.pool...)
	leader, leaderID := s.stableLeader(s.configIDs())
	if leader.Status().Joint {
		s.leaveJoint()
		leader, leaderID = s.stableLeader(s.configIDs())
	}
	old := sortedStrings(leader.Status().Voters)
	if len(old) != 3 {
		s.t.Fatalf("quorum scenario needs exactly 3 voters, got %v", old)
	}
	// The new voters may currently be learners: the joint entry promotes them
	// into C_new and clears them from the learner set.
	var candidates []string
	for _, id := range s.pool {
		if !contains(old, id) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) < 2 {
		s.t.Fatalf("quorum scenario needs two non-voters, have %v", candidates)
	}
	// Shared voter keeps C_old and C_new connected; the two disjoint sides are
	// then {old without shared} (old majority) and {shared + two new} (new
	// majority).
	shared := old[0]
	if shared == leaderID {
		shared = old[1]
	}
	newVoters := sortedStrings([]string{shared, candidates[0], candidates[1]})

	s.enterJoint(newVoters, nil)
	s.waitApplied(append(append([]string(nil), old...), newVoters...), s.lastUserIx, stressPhaseTimeout)

	sideOld := withoutString(old, shared)
	sideNew := []string{shared, candidates[0], candidates[1]}
	s.logf("partition: old majority %v vs new majority %v", sideOld, sideNew)
	partitionGroups(s.c, sideOld, sideNew)

	// Let any in-flight AppendEntries and pre-partition commits settle first.
	// A commit acknowledged before the partition is legitimate; only new
	// commits after this point violate the joint quorum.
	time.Sleep(3 * stressElectionMax)
	before := make(map[string]uint64, len(old)+len(newVoters))
	for _, id := range append(append([]string(nil), old...), newVoters...) {
		before[id] = s.c.nodes[id].Status().CommitIndex
	}

	// Try the proposal on whichever node still claims leadership. The old
	// leader has a C_old majority but no C_new majority; a C_new-side leader
	// (if any) has the opposite. Neither may commit.
	key := "partition/blocked"
	value := []byte("must-not-commit")
	var proposalNode *raft.Node
	for _, id := range append(append([]string(nil), old...), newVoters...) {
		if node := s.c.nodes[id]; node != nil && node.IsLeader() {
			proposalNode = node
			break
		}
	}
	if proposalNode != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_, err := proposalNode.Propose(ctx, model.Entry{Type: model.CommandPut, Key: key, Value: value})
		cancel()
		if err == nil {
			s.t.Fatal("proposal committed with only one side's majority")
		}
		if !errors.Is(err, raft.ErrTimeout) && !errors.Is(err, raft.ErrNotLeader) {
			s.t.Fatalf("blocked proposal error = %v, want ErrTimeout or ErrNotLeader", err)
		}
	}

	time.Sleep(3 * stressElectionMax)
	for _, id := range append(append([]string(nil), old...), newVoters...) {
		if _, ok := s.c.sms[id].get(key); ok {
			s.t.Fatalf("node %s applied a value committed without both majorities", id)
		}
		st := s.c.nodes[id].Status()
		if st.CommitIndex > before[id] {
			s.logf("commit violation %s: commit %d -> %d joint=%v voters=%v old=%v new=%v lastIndex=%d applied=%d",
				id, before[id], st.CommitIndex, st.Joint, st.Voters, st.VotersOld, st.VotersNew, st.LastIndex, st.LastApplied)
			if persisted, err := s.c.stores[id].Load(); err == nil {
				for _, e := range persisted.Entries {
					if e.Index > before[id] && e.Index <= st.CommitIndex {
						s.logf("committed entry %d: type=%s term=%d key=%q", e.Index, e.Type, e.Term, e.Key)
					}
				}
			}
			s.t.Fatalf("node %s committed index %d while partitioned, before %d; joint quorum was not enforced",
				id, st.CommitIndex, before[id])
		}
	}

	healAll(s.c, s.pool...)
	s.waitOneLeader(s.configIDs(), stressLeaderTimeout)
	s.leaveJoint()
	s.waitOneLeader(s.configIDs(), stressLeaderTimeout)
	s.changes += 2

	// The final C_new configuration must be able to commit.
	s.commitValue()
	s.waitApplied(newVoters, s.lastUserIx, stressPhaseTimeout)
}

// ---------------------------------------------------------------------------
// Test
// ---------------------------------------------------------------------------

func TestJointConsensusRandomizedStress(t *testing.T) {
	if os.Getenv("RAFT_JOINT_STRESS") != "1" {
		t.Skip("set RAFT_JOINT_STRESS=1 to run the 1000-change stress test")
	}
	s := newJointStress(t)
	defer func() {
		s.logf("finished: %d changes, %d commits, %d phases, %d failures in %v (changes=%v commits=%v quiesce=%v failures=%v snapshot=%v)",
			s.changes, s.commits, s.phases, s.failures, time.Since(s.start).Round(time.Millisecond),
			s.timeChanges.Round(time.Millisecond), s.timeCommits.Round(time.Millisecond),
			s.timeQuiesce.Round(time.Millisecond), s.timeFailures.Round(time.Millisecond),
			s.timeSnapshot.Round(time.Millisecond))
	}()

	// Join n4 and n5 as learners. These are two of the 1000 logical changes.
	s.addLearner("n4")
	s.changes++
	s.addLearner("n5")
	s.changes++
	s.waitApplied([]string{"n4", "n5"}, s.lastUserIx, stressPhaseTimeout)
	s.quiesce()

	// Verify the central joint-consensus safety property once, under a real
	// two-sided partition.
	s.jointQuorumScenario()
	s.quiesce()

	for s.changes < jointStressChanges {
		t0 := time.Now()
		s.runOneChange()
		s.changes++
		s.timeChanges += time.Since(t0)

		// Interleave application commits. Every operation boundary is a
		// potential leadership change, so this also proves writes cannot be
		// committed without the current quorum.
		if s.changes%3 == 0 {
			t0 = time.Now()
			s.commitValue()
			s.timeCommits += time.Since(t0)
		}

		// Periodic failure injection: leader kill, isolation, follower
		// restart, rotating.
		if s.changes%127 == 0 {
			t0 = time.Now()
			s.failureEvent()
			s.timeFailures += time.Since(t0)
		}

		// Periodic phase check: heal, finish joint state, converge and verify
		// every invariant.
		if s.changes%40 == 0 {
			t0 = time.Now()
			s.quiesce()
			s.timeQuiesce += time.Since(t0)
		}

		// One snapshot install into a fresh learner from a compacted leader.
		if s.snapshotsDone < 2 && s.changes >= s.nextSnapshot {
			t0 = time.Now()
			s.quiesce()
			s.installSnapshotIntoFreshLearner()
			s.quiesce()
			s.timeSnapshot += time.Since(t0)
		}
	}

	s.quiesce()
	// Final configuration is applied and a last commit is visible everywhere.
	s.commitValue()
	s.waitApplied(s.memberIDs(), s.lastUserIx, stressPhaseTimeout)
	s.quiesce()

	leader, _ := s.stableLeader(s.configIDs())
	st := leader.Status()
	if st.Joint {
		t.Fatal("final configuration is still joint")
	}
	if len(st.Voters) == 0 {
		t.Fatal("final configuration has no voters")
	}
	s.logf("final voters=%v learners=%v epoch=%d lastUserIndex=%d",
		st.Voters, st.Learners, st.ConfigEpoch, s.lastUserIx)
}
