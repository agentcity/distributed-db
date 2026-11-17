package kvserver

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"distributed-db/internal/raft"
)

// --- real in-process cluster harness ---------------------------------------

type decommissionTestCluster struct {
	nodes map[string]*Server
	addrs []string
}

// startDecommissionTestCluster starts count single-range KV servers that are
// peers of each other. It returns the live Server handles because the
// decommission control seam is in-process for these tests.
func startDecommissionTestCluster(t *testing.T, count int) *decommissionTestCluster {
	t.Helper()
	addrs := make([]string, count)
	for i := range addrs {
		addrs[i] = testFreeAddr(t)
	}
	cluster := &decommissionTestCluster{
		nodes: make(map[string]*Server, count),
		addrs: addrs,
	}
	for i, addr := range addrs {
		peers := make([]string, 0, count-1)
		for j, peer := range addrs {
			if i != j {
				peers = append(peers, peer)
			}
		}
		srv, err := New(Config{
			ID:       addr,
			RaftAddr: addr,
			DataDir:  t.TempDir(),
			Ranges: []RangeConfig{{
				ID:    1,
				Peers: peers,
				Epoch: 1,
			}},
			ElectionTimeoutMin: 100 * time.Millisecond,
			ElectionTimeoutMax: 200 * time.Millisecond,
			HeartbeatInterval:  20 * time.Millisecond,
			CacheSize:          8 << 20,
			MemTableSize:       4 << 20,
			Logger:             testLogger(),
		})
		if err != nil {
			t.Fatalf("new %s: %v", addr, err)
		}
		if err := srv.Start(context.Background()); err != nil {
			t.Fatalf("start %s: %v", addr, err)
		}
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Stop(stopCtx)
		})
		cluster.nodes[addr] = srv
	}
	return cluster
}

func waitDecommissionLeader(t *testing.T, cluster *decommissionTestCluster) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, addr := range cluster.addrs {
			g, err := cluster.nodes[addr].GroupForID(1)
			if err != nil {
				continue
			}
			if g.Status().Role == raft.Leader {
				return addr
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("range 1 did not elect a leader")
	return ""
}

// localDecommissionControl maps the transport-neutral decommission seam onto
// the existing raft.Node methods of in-process Servers.
type localDecommissionControl struct {
	nodes map[string]*Server
}

func (c *localDecommissionControl) node(node string) (*Server, error) {
	srv, ok := c.nodes[node]
	if !ok {
		return nil, fmt.Errorf("unknown node %q", node)
	}
	return srv, nil
}

func (c *localDecommissionControl) Status(ctx context.Context, rangeID uint64, node string) (DecommissionReplica, error) {
	if err := ctx.Err(); err != nil {
		return DecommissionReplica{}, err
	}
	srv, err := c.node(node)
	if err != nil {
		return DecommissionReplica{}, err
	}
	g, err := srv.GroupForID(rangeID)
	if err != nil {
		return DecommissionReplica{}, err
	}
	st := g.Status()
	return DecommissionReplica{
		ID:          st.ID,
		Role:        st.Role,
		Leader:      st.Leader,
		Term:        st.Term,
		CommitIndex: st.CommitIndex,
		LastIndex:   st.LastIndex,
		LastApplied: st.LastApplied,
		Voters:      st.Voters,
		Learners:    st.Learners,
		Learner:     st.Learner,
		ConfigEpoch: st.ConfigEpoch,
	}, nil
}

func (c *localDecommissionControl) TransferLeadership(ctx context.Context, rangeID uint64, leader, target string) error {
	srv, err := c.node(leader)
	if err != nil {
		return err
	}
	g, err := srv.GroupForID(rangeID)
	if err != nil {
		return err
	}
	return g.TransferLeadership(ctx, target)
}

func (c *localDecommissionControl) PromoteLearner(ctx context.Context, rangeID uint64, leader, target string) error {
	srv, err := c.node(leader)
	if err != nil {
		return err
	}
	g, err := srv.GroupForID(rangeID)
	if err != nil {
		return err
	}
	return g.PromoteLearner(ctx, target)
}

func (c *localDecommissionControl) RemoveVoter(ctx context.Context, rangeID uint64, leader, target string) error {
	srv, err := c.node(leader)
	if err != nil {
		return err
	}
	g, err := srv.GroupForID(rangeID)
	if err != nil {
		return err
	}
	return g.RemoveVoter(ctx, target)
}

// recordingDecommissionMeta is an idempotent test updater; it records every
// update so tests can assert the callback ordering and post-removal state.
type recordingDecommissionMeta struct {
	updates []DecommissionMetaUpdate
	err     error
}

func (m *recordingDecommissionMeta) UpdateRangeMap(ctx context.Context, update DecommissionMetaUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.err != nil {
		return m.err
	}
	update.Voters = cloneStrings(update.Voters)
	update.Learners = cloneStrings(update.Learners)
	m.updates = append(m.updates, update)
	return nil
}

// --- fake control for deterministic lag/quorum/idempotency cases -----------

type fakeDecommissionControl struct {
	leader      string
	voters      []string
	learners    []string
	statuses    map[string]DecommissionReplica
	unavailable map[string]bool

	addLearnerCalls  int
	promoteCalls     int
	removeVoterCalls int
	transferCalls    int
}

func newFakeDecommissionControl(leader string, voters, learners []string) *fakeDecommissionControl {
	f := &fakeDecommissionControl{
		leader:      leader,
		voters:      cloneStrings(voters),
		learners:    cloneStrings(learners),
		statuses:    make(map[string]DecommissionReplica),
		unavailable: make(map[string]bool),
	}
	for _, node := range append(append([]string(nil), voters...), learners...) {
		f.statuses[node] = DecommissionReplica{
			ID:          node,
			Term:        1,
			CommitIndex: 100,
			LastIndex:   100,
			LastApplied: 100,
		}
	}
	return f
}

func (f *fakeDecommissionControl) Status(ctx context.Context, _ uint64, node string) (DecommissionReplica, error) {
	if err := ctx.Err(); err != nil {
		return DecommissionReplica{}, err
	}
	if f.unavailable[node] {
		return DecommissionReplica{}, fmt.Errorf("node %s unavailable", node)
	}
	st, ok := f.statuses[node]
	if !ok {
		return DecommissionReplica{}, fmt.Errorf("node %s unknown", node)
	}
	st.ID = node
	st.Voters = cloneStrings(f.voters)
	st.Learners = cloneStrings(f.learners)
	st.Learner = containsString(f.learners, node)
	if node == f.leader {
		st.Role = raft.Leader
		st.Leader = node
	} else {
		st.Role = raft.Follower
		st.Leader = f.leader
	}
	return st, nil
}

func (f *fakeDecommissionControl) TransferLeadership(ctx context.Context, _ uint64, leader, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.transferCalls++
	if leader != f.leader {
		return raft.ErrNotLeader
	}
	if !containsString(f.voters, target) {
		return raft.ErrUnknownTarget
	}
	f.leader = target
	return nil
}

func (f *fakeDecommissionControl) PromoteLearner(ctx context.Context, _ uint64, _ string, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.promoteCalls++
	if containsString(f.voters, target) {
		return raft.ErrAlreadyVoter
	}
	if !containsString(f.learners, target) {
		return raft.ErrNotLearner
	}
	f.learners = withoutString(f.learners, target)
	f.voters = append(f.voters, target)
	return nil
}

func (f *fakeDecommissionControl) RemoveVoter(ctx context.Context, _ uint64, _ string, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.removeVoterCalls++
	if !containsString(f.voters, target) {
		return raft.ErrNotVoter
	}
	f.voters = withoutString(f.voters, target)
	f.learners = withoutString(f.learners, target)
	return nil
}

// --- real Raft integration tests ------------------------------------------

func TestDecommissionLeaderTransfersThenRemoves(t *testing.T) {
	cluster := startDecommissionTestCluster(t, 3)
	leader := waitDecommissionLeader(t, cluster)

	meta := &recordingDecommissionMeta{}
	control := &localDecommissionControl{nodes: cluster.nodes}
	d, err := NewDecommissioner(control, meta, DecommissionOptions{
		Logger:         testLogger(),
		CatchUpTimeout: 5 * time.Second,
		StepTimeout:    10 * time.Second,
		PollInterval:   10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDecommissioner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req := DecommissionRequest{RangeID: 1, Target: leader, Replicas: cluster.addrs, Epoch: 7}

	res, err := d.Decommission(ctx, req)
	if err != nil {
		t.Fatalf("Decommission(leader %s): %v", leader, err)
	}
	if !res.Transferred {
		t.Fatal("target was the leader but Transferred is false")
	}
	if res.Promoted {
		t.Fatal("voter target must not be promoted")
	}
	if res.AlreadyRemoved {
		t.Fatal("first call reported AlreadyRemoved")
	}
	if res.Leader == leader {
		t.Fatalf("leader is still the removed target %s", leader)
	}
	if len(res.Voters) != 2 || containsString(res.Voters, leader) {
		t.Fatalf("post-decommission voters = %v, want 2 voters without %s", res.Voters, leader)
	}

	// Verify the actual Raft membership on the new leader, not only the result.
	g, err := cluster.nodes[res.Leader].GroupForID(1)
	if err != nil {
		t.Fatalf("new leader group: %v", err)
	}
	st := g.Status()
	if len(st.Voters) != 2 || containsString(st.Voters, leader) {
		t.Fatalf("new leader voters = %v, want 2 without %s", st.Voters, leader)
	}

	if len(meta.updates) != 1 {
		t.Fatalf("meta updates = %d, want 1", len(meta.updates))
	}
	update := meta.updates[0]
	if !update.Removed || update.AlreadyRemoved {
		t.Fatalf("meta update flags = %+v, want Removed=true AlreadyRemoved=false", update)
	}
	if update.Leader != res.Leader || update.Epoch != 7 {
		t.Fatalf("meta update leader/epoch = %q/%d, want %q/7", update.Leader, update.Epoch, res.Leader)
	}
	if containsString(update.Voters, leader) || len(update.Voters) != 2 {
		t.Fatalf("meta update voters = %v, want 2 without %s", update.Voters, leader)
	}

	// Idempotent retry: no second Raft mutation, but meta is refreshed.
	res2, err := d.Decommission(ctx, req)
	if err != nil {
		t.Fatalf("idempotent Decommission: %v", err)
	}
	if !res2.AlreadyRemoved {
		t.Fatalf("second call = %+v, want AlreadyRemoved", res2)
	}
	if res2.Transferred || res2.Promoted {
		t.Fatalf("second call repeated mutations: %+v", res2)
	}
	if containsString(res2.Voters, leader) || len(res2.Voters) != 2 {
		t.Fatalf("second-call voters = %v, want 2 without %s", res2.Voters, leader)
	}
	if len(meta.updates) != 2 {
		t.Fatalf("meta updates after retry = %d, want 2", len(meta.updates))
	}
	if !meta.updates[1].AlreadyRemoved || !meta.updates[1].Removed {
		t.Fatalf("retry meta update = %+v, want AlreadyRemoved and Removed", meta.updates[1])
	}
}

func TestDecommissionRejectsLastVoter(t *testing.T) {
	cluster := startDecommissionTestCluster(t, 1)
	leader := waitDecommissionLeader(t, cluster)

	meta := &recordingDecommissionMeta{}
	d, err := NewDecommissioner(&localDecommissionControl{nodes: cluster.nodes}, meta, DecommissionOptions{
		Logger:       testLogger(),
		StepTimeout:  2 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDecommissioner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := d.Decommission(ctx, DecommissionRequest{RangeID: 1, Target: leader, Replicas: cluster.addrs})
	if !errors.Is(err, ErrDecommissionLastVoter) {
		t.Fatalf("Decommission(last voter) = %v, want ErrDecommissionLastVoter", err)
	}
	var de *DecommissionError
	if !errors.As(err, &de) || de.Phase != DecommissionPhasePreflight {
		t.Fatalf("error = %#v, want DecommissionError in preflight", err)
	}
	if res.AlreadyRemoved || res.Transferred || res.Promoted {
		t.Fatalf("result after last-voter rejection = %+v, want no mutations", res)
	}
	if len(meta.updates) != 0 {
		t.Fatalf("meta updates = %d, want 0", len(meta.updates))
	}
}

// --- deterministic orchestration tests ------------------------------------

func TestDecommissionLaggingLearnerNotPromotedOrRemoved(t *testing.T) {
	control := newFakeDecommissionControl("n1", []string{"n1", "n2", "n3"}, []string{"n4"})
	// The learner has barely started: far behind the leader's last index and
	// commit index. It must never be promoted just to remove it.
	control.statuses["n4"] = DecommissionReplica{
		ID: "n4", Term: 1, CommitIndex: 1, LastIndex: 1, LastApplied: 1,
	}
	meta := &recordingDecommissionMeta{}
	var progress []DecommissionProgress
	d, err := NewDecommissioner(control, meta, DecommissionOptions{
		Logger:         testLogger(),
		CatchUpTimeout: 40 * time.Millisecond,
		StepTimeout:    time.Second,
		PollInterval:   5 * time.Millisecond,
		MaxLagEntries:  4,
		Progress: func(p DecommissionProgress) {
			progress = append(progress, p)
		},
	})
	if err != nil {
		t.Fatalf("NewDecommissioner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := d.Decommission(ctx, DecommissionRequest{
		RangeID: 1, Target: "n4", Replicas: []string{"n1", "n2", "n3", "n4"},
	})
	if !errors.Is(err, ErrDecommissionCatchUp) {
		t.Fatalf("Decommission(lagging learner) = %v, want ErrDecommissionCatchUp", err)
	}
	var de *DecommissionError
	if !errors.As(err, &de) || de.Phase != DecommissionPhaseCatchUp {
		t.Fatalf("error = %#v, want DecommissionError in catch_up", err)
	}
	if control.promoteCalls != 0 {
		t.Fatalf("PromoteLearner calls = %d, want 0", control.promoteCalls)
	}
	if control.removeVoterCalls != 0 {
		t.Fatalf("RemoveVoter calls = %d, want 0", control.removeVoterCalls)
	}
	if len(meta.updates) != 0 {
		t.Fatalf("meta updates = %d, want 0", len(meta.updates))
	}
	if res.Promoted {
		t.Fatal("lagging learner was reported as promoted")
	}
	if !containsString(control.learners, "n4") {
		t.Fatal("lagging learner was removed from the learner set")
	}
	if len(progress) == 0 {
		t.Fatal("no progress was reported")
	}
	last := progress[len(progress)-1]
	if last.Err == nil || last.Phase != DecommissionPhaseCatchUp {
		t.Fatalf("final progress = %+v, want a CatchUp error", last)
	}
}

func TestDecommissionCaughtUpLearnerPromotedThenRemoved(t *testing.T) {
	control := newFakeDecommissionControl("n1", []string{"n1", "n2", "n3"}, []string{"n4"})
	meta := &recordingDecommissionMeta{}
	d, err := NewDecommissioner(control, meta, DecommissionOptions{
		Logger:         testLogger(),
		CatchUpTimeout: time.Second,
		StepTimeout:    time.Second,
		PollInterval:   5 * time.Millisecond,
		MaxLagEntries:  4,
	})
	if err != nil {
		t.Fatalf("NewDecommissioner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := d.Decommission(ctx, DecommissionRequest{
		RangeID: 1, Target: "n4", Replicas: []string{"n1", "n2", "n3", "n4"},
	})
	if err != nil {
		t.Fatalf("Decommission(caught-up learner): %v", err)
	}
	if !res.Promoted {
		t.Fatal("caught-up learner was not promoted")
	}
	if control.promoteCalls != 1 || control.removeVoterCalls != 1 {
		t.Fatalf("promote/remove calls = %d/%d, want 1/1", control.promoteCalls, control.removeVoterCalls)
	}
	if containsString(res.Voters, "n4") || len(res.Voters) != 3 {
		t.Fatalf("final voters = %v, want the original 3 voters", res.Voters)
	}
	if containsString(control.voters, "n4") || containsString(control.learners, "n4") {
		t.Fatalf("fake membership still contains n4: voters=%v learners=%v", control.voters, control.learners)
	}
	if len(meta.updates) != 1 || !meta.updates[0].Removed || meta.updates[0].AlreadyRemoved {
		t.Fatalf("meta updates = %+v, want one Removed update", meta.updates)
	}
}

func TestDecommissionIdempotentRetry(t *testing.T) {
	control := newFakeDecommissionControl("n1", []string{"n1", "n2", "n3"}, nil)
	meta := &recordingDecommissionMeta{}
	var phases []DecommissionPhase
	d, err := NewDecommissioner(control, meta, DecommissionOptions{
		Logger:         testLogger(),
		CatchUpTimeout: time.Second,
		StepTimeout:    time.Second,
		PollInterval:   5 * time.Millisecond,
		Progress: func(p DecommissionProgress) {
			phases = append(phases, p.Phase)
		},
	})
	if err != nil {
		t.Fatalf("NewDecommissioner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := DecommissionRequest{RangeID: 1, Target: "n2", Replicas: []string{"n1", "n2", "n3"}}

	res, err := d.Decommission(ctx, req)
	if err != nil {
		t.Fatalf("first Decommission: %v", err)
	}
	if res.AlreadyRemoved {
		t.Fatal("first call reported AlreadyRemoved")
	}
	if control.removeVoterCalls != 1 || len(meta.updates) != 1 {
		t.Fatalf("first call remove/meta = %d/%d, want 1/1", control.removeVoterCalls, len(meta.updates))
	}

	res2, err := d.Decommission(ctx, req)
	if err != nil {
		t.Fatalf("second Decommission: %v", err)
	}
	if !res2.AlreadyRemoved {
		t.Fatalf("second call = %+v, want AlreadyRemoved", res2)
	}
	if control.removeVoterCalls != 1 {
		t.Fatalf("RemoveVoter calls = %d after retry, want 1", control.removeVoterCalls)
	}
	if len(meta.updates) != 2 {
		t.Fatalf("meta updates = %d, want 2", len(meta.updates))
	}
	if !meta.updates[1].AlreadyRemoved || !meta.updates[1].Removed {
		t.Fatalf("retry update = %+v, want AlreadyRemoved and Removed", meta.updates[1])
	}
	if containsString(res2.Voters, "n2") || len(res2.Voters) != 2 {
		t.Fatalf("retry voters = %v, want 2 without n2", res2.Voters)
	}
	sawDone := false
	for _, phase := range phases {
		if phase == DecommissionPhaseDone {
			sawDone = true
			break
		}
	}
	if !sawDone {
		t.Fatalf("progress phases = %v, want a done phase", phases)
	}
}

func TestDecommissionMetaFailureRetry(t *testing.T) {
	control := newFakeDecommissionControl("n1", []string{"n1", "n2", "n3"}, nil)
	meta := &recordingDecommissionMeta{err: errors.New("meta unavailable")}
	d, err := NewDecommissioner(control, meta, DecommissionOptions{
		Logger:         testLogger(),
		CatchUpTimeout: time.Second,
		StepTimeout:    time.Second,
		PollInterval:   5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDecommissioner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := DecommissionRequest{RangeID: 1, Target: "n2", Replicas: []string{"n1", "n2", "n3"}}

	_, err = d.Decommission(ctx, req)
	if !errors.Is(err, ErrDecommissionMeta) {
		t.Fatalf("Decommission(meta failure) = %v, want ErrDecommissionMeta", err)
	}
	var de *DecommissionError
	if !errors.As(err, &de) || de.Phase != DecommissionPhaseMeta {
		t.Fatalf("error = %#v, want DecommissionError in meta", err)
	}
	if control.removeVoterCalls != 1 {
		t.Fatalf("RemoveVoter calls = %d, want 1 before meta failure", control.removeVoterCalls)
	}
	if containsString(control.voters, "n2") {
		t.Fatal("target was not removed from raft before the meta failure")
	}

	// A retry must not repeat the Raft change; it must only converge meta.
	meta.err = nil
	res, err := d.Decommission(ctx, req)
	if err != nil {
		t.Fatalf("retry Decommission: %v", err)
	}
	if !res.AlreadyRemoved {
		t.Fatalf("retry result = %+v, want AlreadyRemoved", res)
	}
	if control.removeVoterCalls != 1 {
		t.Fatalf("RemoveVoter calls = %d after retry, want 1", control.removeVoterCalls)
	}
	if len(meta.updates) != 1 || !meta.updates[0].AlreadyRemoved {
		t.Fatalf("meta updates after retry = %+v, want one AlreadyRemoved update", meta.updates)
	}
}

func TestDecommissionContextTimeout(t *testing.T) {
	control := newFakeDecommissionControl("n1", []string{"n1", "n2", "n3"}, []string{"n4"})
	control.statuses["n4"] = DecommissionReplica{
		ID: "n4", Term: 1, CommitIndex: 1, LastIndex: 1, LastApplied: 1,
	}
	meta := &recordingDecommissionMeta{}
	d, err := NewDecommissioner(control, meta, DecommissionOptions{
		Logger:         testLogger(),
		CatchUpTimeout: 10 * time.Second,
		StepTimeout:    time.Second,
		PollInterval:   5 * time.Millisecond,
		MaxLagEntries:  4,
	})
	if err != nil {
		t.Fatalf("NewDecommissioner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err = d.Decommission(ctx, DecommissionRequest{
		RangeID: 1, Target: "n4", Replicas: []string{"n1", "n2", "n3", "n4"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Decommission(timeout) = %v, want context.DeadlineExceeded", err)
	}
	if !errors.Is(err, ErrDecommissionTimeout) {
		t.Fatalf("Decommission(timeout) = %v, want ErrDecommissionTimeout", err)
	}
	var de *DecommissionError
	if !errors.As(err, &de) || de.Phase != DecommissionPhaseCatchUp {
		t.Fatalf("error = %#v, want DecommissionError in catch_up", err)
	}
	if control.promoteCalls != 0 || control.removeVoterCalls != 0 || len(meta.updates) != 0 {
		t.Fatalf("timeout performed mutations: promote=%d remove=%d meta=%d",
			control.promoteCalls, control.removeVoterCalls, len(meta.updates))
	}
}

func TestDecommissionRejectsQuorumLoss(t *testing.T) {
	control := newFakeDecommissionControl("n1", []string{"n1", "n2", "n3"}, nil)
	control.unavailable["n2"] = true
	meta := &recordingDecommissionMeta{}
	d, err := NewDecommissioner(control, meta, DecommissionOptions{
		Logger:       testLogger(),
		StepTimeout:  time.Second,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDecommissioner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = d.Decommission(ctx, DecommissionRequest{
		RangeID: 1, Target: "n3", Replicas: []string{"n1", "n2", "n3"},
	})
	if !errors.Is(err, ErrDecommissionQuorumLoss) {
		t.Fatalf("Decommission(quorum loss) = %v, want ErrDecommissionQuorumLoss", err)
	}
	var de *DecommissionError
	if !errors.As(err, &de) || de.Phase != DecommissionPhasePreflight {
		t.Fatalf("error = %#v, want DecommissionError in preflight", err)
	}
	if control.transferCalls != 0 || control.promoteCalls != 0 || control.removeVoterCalls != 0 {
		t.Fatalf("quorum-loss preflight mutated: transfer=%d promote=%d remove=%d",
			control.transferCalls, control.promoteCalls, control.removeVoterCalls)
	}
	if len(meta.updates) != 0 {
		t.Fatalf("meta updates = %d, want 0", len(meta.updates))
	}
}
