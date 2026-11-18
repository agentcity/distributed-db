package raft

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"time"

	"distributed-db/internal/model"
)

// proposal tracks one client proposal from enqueue until it is applied or
// abandoned. done is closed exactly once, while holding Node.mu, after err and
// index have been set; every other field is guarded by Node.mu.
type proposal struct {
	done  chan struct{}
	err   error
	index uint64
	ctx   context.Context
	cmd   model.Entry // Type/Key/Value only; Term/Index are assigned by Raft
	// completed is set when done is closed, so callers that raced context
	// cancellation or Stop can tell a finished proposal from a queued one.
	completed bool
	// canceled is set when the waiter gave up before its batch was assigned;
	// the drain loop then drops it without consuming a log index.
	canceled bool
}

const (
	// defaultProposeBatchWindow bounds how long a lone proposal waits for
	// concurrent proposals to coalesce with. It is deliberately tiny: every
	// proposal pays it once.
	defaultProposeBatchWindow = time.Millisecond
	// defaultProposeBatchMax caps one durable append / replication batch.
	defaultProposeBatchMax = 64
)

// Node is a single Raft replica.
//
// All Raft state is guarded by mu. Network calls are always made without
// holding mu so a slow or partitioned peer cannot block the core.
type Node struct {
	cfg    Config
	store  LogStore
	sm     StateMachine
	tr     Transport
	logger *slog.Logger

	mu               sync.Mutex
	term             uint64
	votedFor         string
	entries          []model.Entry
	snapshot         Snapshot
	commitIndex      uint64
	lastApplied      uint64
	leaderNoopIndex  uint64
	role             Role
	leaderID         string
	electionDeadline time.Time
	votes            map[string]bool
	// voters and learners are the mutable Raft membership. voters is C_new
	// (the current voter set outside a joint configuration) and includes this
	// node when it is a voter. votersOld is C_old and is non-empty exactly
	// while the node is in the intermediate joint configuration. cfg.Peers is
	// only the bootstrap input.
	voters    []string
	votersOld []string
	learners  map[string]bool
	// configEpoch is the monotonic version of the configuration. It is
	// embedded in snapshots and persisted with membership when the store
	// supports it. A joint change advances it twice (joint, then final).
	configEpoch uint64
	// configInFlight gates membership changes: one single-change operation at
	// a time, and no new change while joint.
	configInFlight bool
	// autoFinalize marks a joint configuration created by a single-change API
	// that the leader should complete automatically. It is part of the
	// configuration and is persisted with it.
	autoFinalize bool
	// finalInFlight is set after this leader appended C_new and is waiting for
	// it to commit/apply. It prevents duplicate final entries while the joint
	// configuration is still applied.
	finalInFlight bool
	// configCh is closed and replaced whenever the applied configuration
	// changes, waking configuration waiters.
	configCh chan struct{}
	// autoFinalSignal wakes autoFinalizeLoop.
	autoFinalSignal chan struct{}
	nextIndex       map[string]uint64
	matchIndex      map[string]uint64
	lastAck         map[string]time.Time
	triggers        map[string]chan struct{}
	pending         map[uint64]*proposal

	// proposeQueue and proposeSignal implement group commit. Both are guarded
	// by mu. Propose enqueues under mu (no I/O) and then waits without mu; the
	// single proposeLoop goroutine coalesces queued proposals and performs one
	// durable batch append.
	proposeQueue  []*proposal
	proposeSignal chan struct{}

	// applyMu serialises state-machine mutations (Apply) against snapshot
	// creation and installation, so a snapshot can never interleave with an
	// entry application. It is acquired before mu.
	applyMu sync.Mutex

	// snapshotRecvMu serialises the chunked snapshot receiver. snapshotRecv
	// holds the partial uploads keyed by leader id; it is bounded, a per-upload
	// timer removes idle uploads, and every chunk also sweeps stale entries.
	// Both are guarded by snapshotRecvMu.
	snapshotRecvMu       sync.Mutex
	snapshotRecv         map[string]*snapshotChunkRecv
	snapshotChunkTimeout time.Duration

	applyCond *sync.Cond
	stopCh    chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup

	runCtx       context.Context
	cancel       context.CancelFunc
	leaderCtx    context.Context
	leaderCancel context.CancelFunc

	started bool
	stopped bool
}

// New validates cfg, loads the durable state and constructs a follower. The
// node does not run until Start is called.
func New(cfg Config, store LogStore, sm StateMachine, tr Transport) (*Node, error) {
	if store == nil {
		return nil, errors.New("raft: nil LogStore")
	}
	if sm == nil {
		return nil, errors.New("raft: nil StateMachine")
	}
	if tr == nil {
		return nil, errors.New("raft: nil Transport")
	}
	if cfg.ID == "" {
		return nil, errors.New("raft: empty node id")
	}
	if cfg.ElectionTimeoutMin <= 0 || cfg.ElectionTimeoutMax <= 0 {
		return nil, errors.New("raft: election timeouts must be positive")
	}
	if cfg.ElectionTimeoutMax < cfg.ElectionTimeoutMin {
		return nil, errors.New("raft: ElectionTimeoutMax < ElectionTimeoutMin")
	}
	if cfg.HeartbeatInterval <= 0 {
		return nil, errors.New("raft: heartbeat interval must be positive")
	}
	if cfg.ProposeBatchWindow <= 0 {
		cfg.ProposeBatchWindow = defaultProposeBatchWindow
	}
	if cfg.ProposeBatchMax <= 0 {
		cfg.ProposeBatchMax = defaultProposeBatchMax
	}

	state, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("raft: load persistent state: %w", err)
	}
	snap, err := loadAndRestoreSnapshot(store, sm)
	if err != nil {
		return nil, err
	}
	// Entries are contiguous after the snapshot (or from index 1 without one);
	// a compacted store loads only the suffix it still holds.
	firstIndex := snap.Index + 1
	for i, e := range state.Entries {
		if e.Index != firstIndex+uint64(i) {
			return nil, fmt.Errorf("raft: non-contiguous log: entry %d has index %d, want %d", i+1, e.Index, firstIndex+uint64(i))
		}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	config, err := initMembership(cfg, store, snap.Membership)
	if err != nil {
		return nil, err
	}
	if cfg.ForceLearner {
		// Re-adding a range to a node with an old data directory: overwrite
		// the stale on-disk membership before the node can campaign.
		if err := saveConfigState(store, config); err != nil {
			return nil, fmt.Errorf("raft: persist forced learner membership: %w", err)
		}
	}
	learnerSet := make(map[string]bool, len(config.learners))
	for _, l := range config.learners {
		learnerSet[l] = true
	}
	n := &Node{
		cfg:                  cfg,
		store:                store,
		sm:                   sm,
		tr:                   tr,
		logger:               logger,
		term:                 state.Term,
		votedFor:             state.VotedFor,
		entries:              cloneEntries(state.Entries),
		snapshot:             cloneSnapshot(snap),
		commitIndex:          snap.Index,
		lastApplied:          snap.Index,
		role:                 Follower,
		votes:                make(map[string]bool),
		voters:               config.voters,
		votersOld:            config.votersOld,
		learners:             learnerSet,
		configEpoch:          config.epoch,
		configInFlight:       config.joint(),
		autoFinalize:         config.autoFinalize,
		nextIndex:            make(map[string]uint64),
		matchIndex:           make(map[string]uint64),
		lastAck:              make(map[string]time.Time),
		triggers:             make(map[string]chan struct{}),
		pending:              make(map[uint64]*proposal),
		snapshotRecv:         make(map[string]*snapshotChunkRecv),
		snapshotChunkTimeout: snapshotChunkIdleTimeout,
		configCh:             make(chan struct{}),
		autoFinalSignal:      make(chan struct{}, 1),
		stopCh:               make(chan struct{}),
		proposeSignal:        make(chan struct{}, 1),
	}
	n.applyCond = sync.NewCond(&n.mu)
	// An uncommitted config entry already in the log means a configuration
	// change is in progress even if the applied configuration is still old.
	n.configInFlight = n.configInFlight || n.pendingConfigLocked()
	return n, nil
}

// loadAndRestoreSnapshot recovers the durable snapshot and restores the state
// machine. When both the store and the state machine support streaming, the
// payload is restored from the data file without loading it into memory.
// Otherwise it falls back to the []byte SnapshotStore/SnapshotRestorer path.
func loadAndRestoreSnapshot(store LogStore, sm StateMachine) (Snapshot, error) {
	streamStore, hasStream := store.(SnapshotStreamStore)
	if hasStream {
		if restorer, ok := sm.(SnapshotStreamRestorer); ok {
			meta, rc, err := streamStore.OpenSnapshot()
			if err != nil {
				return Snapshot{}, fmt.Errorf("raft: load snapshot: %w", err)
			}
			if rc == nil || meta.Index == 0 {
				if rc != nil {
					_ = rc.Close()
				}
				return Snapshot{}, nil
			}
			if err := restorer.RestoreSnapshotStream(rc); err != nil {
				_ = rc.Close()
				return Snapshot{}, fmt.Errorf("raft: restore state machine from snapshot: %w", err)
			}
			if err := rc.Close(); err != nil {
				return Snapshot{}, fmt.Errorf("raft: close snapshot stream: %w", err)
			}
			return Snapshot{Index: meta.Index, Term: meta.Term, Membership: meta.Membership}, nil
		}
	}

	if snapshotStore, ok := store.(SnapshotStore); ok {
		snap, err := snapshotStore.LoadSnapshot()
		if err != nil {
			return Snapshot{}, fmt.Errorf("raft: load snapshot: %w", err)
		}
		if err := restoreSnapshotData(sm, snap); err != nil {
			return Snapshot{}, err
		}
		return snap, nil
	}

	if hasStream {
		// No []byte loader and no streaming restorer: recover the snapshot
		// position so the log suffix stays contiguous across a restart, even
		// though this state machine cannot rebuild its data from it.
		meta, rc, err := streamStore.OpenSnapshot()
		if err != nil {
			return Snapshot{}, fmt.Errorf("raft: load snapshot: %w", err)
		}
		if rc != nil {
			_ = rc.Close()
		}
		return Snapshot{Index: meta.Index, Term: meta.Term, Membership: meta.Membership}, nil
	}
	return Snapshot{}, nil
}

// restoreSnapshotData restores a []byte snapshot into a state machine that
// implements SnapshotRestorer. A state machine without that capability keeps
// its current state, matching Node.RestoreSnapshot.
func restoreSnapshotData(sm StateMachine, snap Snapshot) error {
	if snap.Index == 0 {
		return nil
	}
	if r, ok := sm.(SnapshotRestorer); ok {
		if err := r.RestoreSnapshot(append([]byte(nil), snap.Data...)); err != nil {
			return fmt.Errorf("raft: restore state machine from snapshot: %w", err)
		}
	}
	return nil
}

// Start launches the background loops and returns immediately. Cancelling ctx
// is equivalent to calling Stop.
func (n *Node) Start(ctx context.Context) error {
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return ErrStopped
	}
	if n.started {
		n.mu.Unlock()
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	n.runCtx = runCtx
	n.cancel = cancel
	n.started = true
	n.resetElectionDeadlineLocked()
	// Add before releasing mu: Stop sets stopped under mu before waiting, so
	// no Add can race with Wait.
	n.wg.Add(5)
	n.mu.Unlock()

	go func() {
		defer n.wg.Done()
		n.electionLoop(runCtx)
	}()
	go func() {
		defer n.wg.Done()
		n.applyLoop()
	}()
	go func() {
		defer n.wg.Done()
		n.proposeLoop()
	}()
	go func() {
		defer n.wg.Done()
		n.autoFinalizeLoop(runCtx)
	}()
	go func() {
		defer n.wg.Done()
		select {
		case <-ctx.Done():
			n.stopOnce.Do(n.stop)
		case <-n.stopCh:
		}
	}()
	return nil
}

// Stop halts all background work and waits for in-flight RPCs to return.
func (n *Node) Stop() {
	n.stopOnce.Do(n.stop)
	n.wg.Wait()
	n.discardAllSnapshotChunks()
}

func (n *Node) stop() {
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	n.stopped = true
	n.role = Follower
	n.leaderID = ""
	if n.cancel != nil {
		n.cancel()
	}
	n.cancelLeaderLocked()
	n.failPendingLocked(ErrStopped)
	n.failQueuedLocked(ErrStopped)
	n.applyCond.Broadcast()
	n.mu.Unlock()
	close(n.stopCh)
}

// Propose appends cmd on the leader and returns only after the entry is
// committed and applied. Term/Index are filled in by Raft.
//
// Propose itself performs no disk I/O: it validates leadership under mu, puts
// the proposal on the group-commit queue and waits. A single drain goroutine
// assigns contiguous indices and appends all proposals collected in the
// batching window with one durable LogStore call.
func (n *Node) Propose(ctx context.Context, cmd model.Entry) (uint64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return 0, ErrStopped
	}
	if n.role != Leader {
		n.mu.Unlock()
		return 0, ErrNotLeader
	}
	p := &proposal{
		done: make(chan struct{}),
		ctx:  ctx,
		cmd:  cloneEntry(cmd),
	}
	n.proposeQueue = append(n.proposeQueue, p)
	select {
	case n.proposeSignal <- struct{}{}:
	default:
	}
	n.mu.Unlock()

	select {
	case <-p.done:
		if p.err != nil {
			return 0, p.err
		}
		return p.index, nil
	case <-n.stopCh:
		n.mu.Lock()
		completed, index, err := p.completed, p.index, p.err
		n.mu.Unlock()
		if completed {
			if err != nil {
				return 0, err
			}
			return index, nil
		}
		// Not durable yet; the drain loop will fail the queued/collected
		// proposal. The caller must not receive an index.
		return 0, ErrStopped
	case <-ctx.Done():
		return n.abandonProposal(p, ctx)
	}
}

// abandonProposal removes one waiter from the pipeline after its context ended.
// If the entry is already durable (pending) it leaves it in the log, matching
// the pre-batching behavior: a timed-out proposal may still commit and apply.
// If the proposal is still queued, it is marked canceled so the drain loop does
// not assign it a log index.
func (n *Node) abandonProposal(p *proposal, ctx context.Context) (uint64, error) {
	waitErr := proposalContextError(ctx)

	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case p.completed:
		if p.err != nil {
			return 0, p.err
		}
		return p.index, nil
	case p.index != 0 && n.pending[p.index] == p:
		delete(n.pending, p.index)
		return 0, waitErr
	default:
		// Still queued, or collected but not yet assigned: the drain loop
		// will drop it.
		p.canceled = true
		return 0, waitErr
	}
}

// proposalContextError maps a waiter's context error to Propose's public
// contract: deadlines become ErrTimeout, other cancellations stay untouched.
func proposalContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ErrTimeout
		}
		return err
	}
	return ErrTimeout
}

// ReadBarrier succeeds only while this node is leader, has heard from a strict
// majority (including itself) within ElectionTimeoutMin, and has applied every
// entry that was known committed when the barrier started. Waiting for the
// apply loop is essential after a failover: a new leader's log already contains
// the previous leader's committed entries, but its local commitIndex and
// lastApplied may still lag.
func (n *Node) ReadBarrier(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
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
	if !n.hasQuorumLocked(time.Now()) {
		n.logger.Warn("raft: leader lost quorum lease", "id", n.cfg.ID, "term", n.term)
		n.stepDownWithErrorLocked("", errLostQuorum)
		n.mu.Unlock()
		return ErrNotLeader
	}
	// A leader must also have committed an entry from its own term (the no-op
	// appended on election) for its commitIndex to include entries inherited
	// from previous terms.
	target := n.commitIndex
	if n.leaderNoopIndex > target {
		target = n.leaderNoopIndex
	}
	barrierTerm := n.term
	n.mu.Unlock()

	for {
		n.mu.Lock()
		if n.stopped {
			n.mu.Unlock()
			return ErrStopped
		}
		if n.role != Leader || n.term != barrierTerm {
			n.mu.Unlock()
			return ErrNotLeader
		}
		if n.lastApplied >= target {
			// The lease may have expired while waiting for apply.
			if !n.hasQuorumLocked(time.Now()) {
				n.stepDownWithErrorLocked("", errLostQuorum)
				n.mu.Unlock()
				return ErrNotLeader
			}
			n.mu.Unlock()
			return nil
		}
		n.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-n.stopCh:
			return ErrStopped
		case <-time.After(time.Millisecond):
		}
	}
}

// Status returns a consistent snapshot of the node's state.
func (n *Node) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	status := Status{
		ID:          n.cfg.ID,
		Role:        n.role,
		Term:        n.term,
		Leader:      n.leaderID,
		CommitIndex: n.commitIndex,
		LastIndex:   n.lastIndexLocked(),
		LastApplied: n.lastApplied,
		Voters:      n.votingMembersLocked(),
		Learners:    n.learnerListLocked(),
		Learner:     n.isLearnerLocked(n.cfg.ID),
		Joint:       n.isJointLocked(),
		ConfigEpoch: n.configEpoch,
	}
	if n.isJointLocked() {
		status.VotersOld = append([]string(nil), n.votersOld...)
		status.VotersNew = append([]string(nil), n.voters...)
	}
	return status
}

// Leader returns the last known leader id, or "" when unknown.
func (n *Node) Leader() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.leaderID
}

// IsLeader reports whether this node currently believes it is the leader.
func (n *Node) IsLeader() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.role == Leader && !n.stopped
}

// Snapshot asks the state machine for a snapshot, persists it through the
// optional SnapshotStore capability and truncates the local log prefix. The
// returned snapshot is a copy owned by the caller.
//
// A StateMachine that does not implement Snapshotter makes this return
// ErrSnapshotUnsupported. A LogStore that does not implement SnapshotStore
// still compacts the in-memory log, but the snapshot is not durable, so a
// restart replays the full log.
func (n *Node) Snapshot() (Snapshot, error) {
	snapshotter, ok := n.sm.(Snapshotter)
	if !ok {
		return Snapshot{}, ErrSnapshotUnsupported
	}
	data, index, err := snapshotter.Snapshot()
	if err != nil {
		return Snapshot{}, fmt.Errorf("raft: state machine snapshot: %w", err)
	}

	n.applyMu.Lock()
	defer n.applyMu.Unlock()

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return Snapshot{}, ErrStopped
	}
	if index == 0 {
		// Nothing has been applied yet, so there is no state to persist.
		n.mu.Unlock()
		return Snapshot{}, nil
	}
	if index > n.lastIndexLocked() {
		n.mu.Unlock()
		return Snapshot{}, fmt.Errorf("raft: snapshot index %d outside log [%d,%d]", index, n.snapshot.Index, n.lastIndexLocked())
	}
	// Config entries are Raft state and are never delivered to the state
	// machine, so its applied index can lag lastApplied. Extend the snapshot
	// through a contiguous run of config entries so the membership envelope
	// covers exactly the configuration the snapshot claims to include.
	index = n.snapshotIndexLocked(index)
	if index <= n.snapshot.Index {
		// Nothing new has been applied since the last snapshot. A snapshot
		// created by SnapshotToStore keeps only metadata in memory, so when the
		// state machine still reports the same index, return the fresh data
		// rather than a metadata-only copy.
		if index == n.snapshot.Index && n.snapshot.Data == nil {
			membership := cloneMembership(n.snapshot.Membership)
			if membership == nil {
				// Legacy or metadata-only snapshot: attach the membership this
				// node currently knows so the returned envelope is complete.
				membership = n.snapshotMembershipLocked()
			}
			snap := Snapshot{Index: index, Term: n.snapshot.Term, Data: append([]byte(nil), data...), Membership: membership}
			n.mu.Unlock()
			return snap, nil
		}
		snap := cloneSnapshot(n.snapshot)
		n.mu.Unlock()
		return snap, nil
	}
	term, ok := n.termAtLocked(index)
	if !ok {
		n.mu.Unlock()
		return Snapshot{}, fmt.Errorf("raft: no log entry at snapshot index %d", index)
	}
	snap := Snapshot{
		Index:      index,
		Term:       term,
		Data:       append([]byte(nil), data...),
		Membership: n.snapshotMembershipLocked(),
	}

	// Persist before the local log loses its prefix: a crash in between must
	// leave a loadable state, never a truncated log without a snapshot.
	if ss, ok := n.store.(SnapshotStore); ok {
		if err := ss.SaveSnapshot(snap); err != nil {
			n.mu.Unlock()
			return Snapshot{}, fmt.Errorf("raft: persist snapshot: %w", err)
		}
		if err := ss.TruncatePrefix(index); err != nil {
			n.mu.Unlock()
			return Snapshot{}, fmt.Errorf("raft: truncate log prefix: %w", err)
		}
	}
	drop := index - n.snapshot.Index
	n.entries = n.entries[drop:]
	n.snapshot = snap
	n.mu.Unlock()
	return cloneSnapshot(snap), nil
}

// SnapshotToStore asks a streaming state machine for a snapshot and persists
// it through a SnapshotStreamStore without materialising the payload in
// memory. It then truncates the local log prefix and records the new snapshot
// metadata in the node, exactly as Snapshot does.
//
// The state machine must implement SnapshotStreamer and the store must
// implement SnapshotStreamStore; otherwise SnapshotToStore returns
// ErrSnapshotStreamUnsupported (Snapshot remains the fallback for
// []byte-producing state machines). The streamed image is staged in a
// temporary file (under DataDir when it is writable, otherwise in the system
// temporary directory) and then copied through the store, so the snapshot is
// written twice but never held in memory. The scratch file is removed on
// return; a crash may leave a raft-snapshot-*.tmp file in DataDir.
//
// The returned metadata can be used by a transport to stream the snapshot in
// chunks through store.OpenSnapshot. The node keeps only Index and Term in
// memory, so legacy SnapshotTransport installs materialise the payload on
// demand.
func (n *Node) SnapshotToStore() (SnapshotMeta, error) {
	streamer, ok := n.sm.(SnapshotStreamer)
	if !ok {
		return SnapshotMeta{}, ErrSnapshotStreamUnsupported
	}
	store, ok := n.store.(SnapshotStreamStore)
	if !ok {
		return SnapshotMeta{}, ErrSnapshotStreamUnsupported
	}

	n.applyMu.Lock()
	defer n.applyMu.Unlock()

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return SnapshotMeta{}, ErrStopped
	}
	if n.lastApplied == 0 {
		// Nothing has been applied yet, so there is no state to persist.
		n.mu.Unlock()
		return SnapshotMeta{}, nil
	}
	if n.lastApplied <= n.snapshot.Index {
		// No new applied state since the last snapshot.
		n.mu.Unlock()
		return currentSnapshotMeta(store)
	}
	// applyMu is held, so the membership cannot change while the state machine
	// streams the payload; capture it now and embed it in the snapshot.
	membership := n.snapshotMembershipLocked()
	n.mu.Unlock()

	scratch, err := n.createSnapshotScratch()
	if err != nil {
		return SnapshotMeta{}, err
	}
	scratchName := scratch.Name()
	defer func() {
		_ = scratch.Close()
		_ = os.Remove(scratchName)
	}()

	index, err := streamer.StreamSnapshot(scratch)
	if err != nil {
		return SnapshotMeta{}, fmt.Errorf("raft: state machine snapshot stream: %w", err)
	}
	if index == 0 {
		return SnapshotMeta{}, errors.New("raft: state machine streamed an empty snapshot")
	}
	if err := scratch.Sync(); err != nil {
		return SnapshotMeta{}, fmt.Errorf("raft: sync snapshot scratch file: %w", err)
	}
	if _, err := scratch.Seek(0, io.SeekStart); err != nil {
		return SnapshotMeta{}, fmt.Errorf("raft: rewind snapshot scratch file: %w", err)
	}

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return SnapshotMeta{}, ErrStopped
	}
	if index > n.lastIndexLocked() {
		n.mu.Unlock()
		return SnapshotMeta{}, fmt.Errorf("raft: snapshot index %d outside log [%d,%d]", index, n.snapshot.Index, n.lastIndexLocked())
	}
	if index <= n.snapshot.Index {
		// The state machine produced a snapshot no newer than the current one;
		// discard the staged copy and report the existing one.
		n.mu.Unlock()
		return currentSnapshotMeta(store)
	}
	term, ok := n.termAtLocked(index)
	n.mu.Unlock()
	if !ok {
		return SnapshotMeta{}, fmt.Errorf("raft: no log entry at snapshot index %d", index)
	}

	// Persist before the local log loses its prefix: a crash in between must
	// leave a loadable state, never a truncated log without a snapshot.
	if err := saveSnapshotFromReader(store, index, term, membership, scratch); err != nil {
		return SnapshotMeta{}, fmt.Errorf("raft: persist streamed snapshot: %w", err)
	}
	meta, rc, err := store.OpenSnapshot()
	if err != nil {
		return SnapshotMeta{}, fmt.Errorf("raft: open persisted snapshot: %w", err)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if meta.Index != index {
		return SnapshotMeta{}, fmt.Errorf("raft: persisted snapshot index %d, want %d", meta.Index, index)
	}
	if err := truncatePrefixForSnapshot(n.store, index); err != nil {
		return SnapshotMeta{}, fmt.Errorf("raft: truncate log prefix: %w", err)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return SnapshotMeta{}, ErrStopped
	}
	if index <= n.snapshot.Index {
		// A concurrent install superseded the snapshot while it was copied.
		return meta, nil
	}
	if t, ok := n.termAtLocked(index); ok && t != term {
		return SnapshotMeta{}, fmt.Errorf("raft: log changed under streamed snapshot: term %d at index %d, want %d", t, index, term)
	}
	if index >= n.lastIndexLocked() {
		n.entries = nil
	} else {
		n.entries = n.entries[index-n.snapshot.Index:]
	}
	n.snapshot = Snapshot{Index: index, Term: term, Membership: cloneMembership(membership)}
	return meta, nil
}

// saveSnapshotFromReader persists a streamed snapshot together with its
// membership envelope. Stores that implement only SnapshotStreamStore still
// work; their snapshots then carry no membership (legacy behaviour).
func saveSnapshotFromReader(store SnapshotStreamStore, index, term uint64, membership *SnapshotMembership, r io.Reader) error {
	if sms, ok := store.(SnapshotMembershipStreamStore); ok {
		return sms.SaveSnapshotFromReaderWithMembership(index, term, membership, r)
	}
	return store.SaveSnapshotFromReader(index, term, r)
}

// currentSnapshotMeta returns the metadata of the store's current snapshot
// without reading its payload.
func currentSnapshotMeta(store SnapshotStreamStore) (SnapshotMeta, error) {
	meta, rc, err := store.OpenSnapshot()
	if err != nil {
		return SnapshotMeta{}, fmt.Errorf("raft: open snapshot: %w", err)
	}
	if rc != nil {
		_ = rc.Close()
	}
	return meta, nil
}

// prefixTruncator is the narrow log-compaction capability used by the
// streaming snapshot paths. SnapshotStreamStore does not require it: a store
// that can persist a snapshot but cannot compact its log still works, it just
// keeps the covered prefix (and must keep Load consistent with the snapshot).
type prefixTruncator interface {
	TruncatePrefix(index uint64) error
}

func truncatePrefixForSnapshot(store LogStore, index uint64) error {
	if pt, ok := store.(prefixTruncator); ok {
		return pt.TruncatePrefix(index)
	}
	return nil
}

// createSnapshotScratch creates a temporary file for staging a snapshot
// stream. It prefers the node's data directory so the file shares a
// filesystem with the WAL, and falls back to the system temporary directory.
func (n *Node) createSnapshotScratch() (*os.File, error) {
	return n.createSnapshotTemp("raft-snapshot-*.tmp")
}

// createSnapshotTemp is createSnapshotScratch with a caller-selected pattern.
// The chunked snapshot receiver uses a distinct prefix so partial uploads are
// easy to spot and never confused with locally staged snapshots.
func (n *Node) createSnapshotTemp(pattern string) (*os.File, error) {
	dirs := make([]string, 0, 2)
	if n.cfg.DataDir != "" {
		dirs = append(dirs, n.cfg.DataDir)
	}
	if dir := os.TempDir(); dir != "" && dir != n.cfg.DataDir {
		dirs = append(dirs, dir)
	}
	var lastErr error
	for _, dir := range dirs {
		f, err := os.CreateTemp(dir, pattern)
		if err == nil {
			return f, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no temporary directory")
	}
	return nil, fmt.Errorf("raft: create snapshot scratch file: %w", lastErr)
}

// RestoreSnapshot installs a snapshot received from a leader. The state
// machine is restored (when it implements SnapshotRestorer), the durable and
// in-memory logs are truncated to the snapshot, commit/applied advance to its
// index, and the snapshot's membership (when present) replaces the local voter
// and learner sets and is persisted through MembershipStore.
//
// A snapshot without membership metadata (a legacy snapshot) leaves the local
// membership untouched. Corrupt membership is rejected before any state is
// changed, and a membership whose config epoch is older than this node's is
// rejected as stale rather than resurrecting removed voters.
func (n *Node) RestoreSnapshot(s Snapshot) error {
	if s.Index == 0 {
		return errors.New("raft: snapshot index must be positive")
	}
	membership, err := normalizeSnapshotMembership(s.Membership)
	if err != nil {
		return err
	}

	n.applyMu.Lock()
	defer n.applyMu.Unlock()

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return ErrStopped
	}
	if s.Index <= n.snapshot.Index {
		// Already at least as up to date; snapshots are idempotent.
		n.mu.Unlock()
		return nil
	}
	if membership != nil && membership.ConfigEpoch < n.configEpoch {
		localEpoch := n.configEpoch
		n.mu.Unlock()
		return fmt.Errorf("%w: snapshot epoch %d, local epoch %d",
			ErrStaleSnapshotMembership, membership.ConfigEpoch, localEpoch)
	}
	// Keep a matching suffix: entries after the snapshot are still valid when
	// this node's log agrees with the leader at the snapshot index.
	keepSuffix := false
	if term, ok := n.termAtLocked(s.Index); ok && term == s.Term {
		keepSuffix = true
	}
	n.mu.Unlock()

	snap := cloneSnapshot(s)
	snap.Membership = cloneMembership(membership)
	if r, ok := n.sm.(SnapshotRestorer); ok {
		if err := r.RestoreSnapshot(append([]byte(nil), snap.Data...)); err != nil {
			return fmt.Errorf("raft: restore state machine: %w", err)
		}
	} else {
		n.logger.Warn("raft: state machine cannot restore snapshots", "id", n.cfg.ID, "index", snap.Index)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return ErrStopped
	}
	if snap.Index <= n.snapshot.Index {
		return nil
	}
	if ss, ok := n.store.(SnapshotStore); ok {
		if err := ss.SaveSnapshot(snap); err != nil {
			return fmt.Errorf("raft: persist installed snapshot: %w", err)
		}
		// Drop the divergent suffix first, then everything the snapshot
		// covers, so a crash between the two steps still reloads safely.
		if !keepSuffix {
			if err := n.store.Truncate(snap.Index + 1); err != nil {
				return fmt.Errorf("raft: truncate log after snapshot: %w", err)
			}
		}
		if err := ss.TruncatePrefix(snap.Index); err != nil {
			return fmt.Errorf("raft: truncate log prefix after snapshot: %w", err)
		}
	}
	oldIndex := n.snapshot.Index
	if keepSuffix {
		n.entries = n.entries[snap.Index-oldIndex:]
	} else {
		n.entries = nil
		n.failPendingFromLocked(snap.Index+1, ErrNotLeader)
	}
	n.snapshot = snap
	if n.commitIndex < snap.Index {
		n.commitIndex = snap.Index
	}
	if n.lastApplied < snap.Index {
		n.lastApplied = snap.Index
	}
	n.applyCond.Broadcast()

	// The snapshot is authoritative for membership: it wins over whatever this
	// node had before, including a stale voter set it never saw change.
	if err := n.applySnapshotMembershipLocked(membership); err != nil {
		return err
	}
	return nil
}

// RestoreSnapshotStream installs a snapshot whose payload is read from r. It
// is the streaming counterpart of RestoreSnapshot and keeps the same
// idempotency, term and log-suffix rules:
//
//   - a snapshot at or below the current snapshot index is ignored and r is
//     not read;
//   - a matching term at meta.Index keeps the local log suffix; otherwise the
//     divergent suffix is dropped;
//   - the state machine is restored before the snapshot is persisted, and
//     commit/applied advance to meta.Index.
//
// The state machine should implement SnapshotStreamRestorer; if it does not,
// Raft logs a warning and still persists the snapshot and truncates the log.
// The store must implement SnapshotStreamStore, otherwise
// ErrSnapshotStreamUnsupported is returned before any data is consumed.
//
// meta.Membership is validated before the stream is consumed. When present it
// becomes this node's voter/learner configuration (and is persisted through
// MembershipStore) exactly as in RestoreSnapshot; a nil membership marks a
// legacy snapshot and leaves the current configuration untouched.
//
// The stream is staged in a temporary file while Size and CRC32 are checked
// (a zero Size or CRC32 means "not supplied" and is not enforced), then
// restored into the state machine and persisted through the store. The caller
// owns r and closes it. A crash or error mid-restore can leave the state
// machine partially updated; the previous durable snapshot is committed only
// after the full payload has been received and verified.
func (n *Node) RestoreSnapshotStream(meta SnapshotMeta, r io.Reader) error {
	if meta.Index == 0 {
		return errors.New("raft: snapshot index must be positive")
	}
	if meta.Size < 0 {
		return fmt.Errorf("raft: invalid snapshot size %d", meta.Size)
	}
	if r == nil {
		return errors.New("raft: nil snapshot stream")
	}
	membership, err := normalizeSnapshotMembership(meta.Membership)
	if err != nil {
		return err
	}
	store, ok := n.store.(SnapshotStreamStore)
	if !ok {
		return ErrSnapshotStreamUnsupported
	}

	n.applyMu.Lock()
	defer n.applyMu.Unlock()

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return ErrStopped
	}
	if meta.Index <= n.snapshot.Index {
		// Already at least as up to date; snapshots are idempotent.
		n.mu.Unlock()
		return nil
	}
	if membership != nil && membership.ConfigEpoch < n.configEpoch {
		localEpoch := n.configEpoch
		n.mu.Unlock()
		return fmt.Errorf("%w: snapshot epoch %d, local epoch %d",
			ErrStaleSnapshotMembership, membership.ConfigEpoch, localEpoch)
	}
	n.mu.Unlock()

	scratch, err := n.createSnapshotScratch()
	if err != nil {
		return err
	}
	scratchName := scratch.Name()
	defer func() {
		_ = scratch.Close()
		_ = os.Remove(scratchName)
	}()

	hasher := crc32.NewIEEE()
	var src io.Reader = r
	if meta.Size > 0 {
		// Read at most one byte past the declared size so an oversized stream
		// is detected without copying it all.
		src = io.LimitReader(r, meta.Size+1)
	}
	size, err := io.Copy(io.MultiWriter(scratch, hasher), src)
	if err != nil {
		return fmt.Errorf("raft: stream snapshot payload: %w", err)
	}
	if meta.Size > 0 && size != meta.Size {
		return fmt.Errorf("raft: snapshot payload size %d, want %d", size, meta.Size)
	}
	if meta.CRC32 != 0 && hasher.Sum32() != meta.CRC32 {
		return fmt.Errorf("raft: snapshot payload CRC32 %08x, want %08x", hasher.Sum32(), meta.CRC32)
	}
	if err := scratch.Sync(); err != nil {
		return fmt.Errorf("raft: sync snapshot scratch file: %w", err)
	}
	if _, err := scratch.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("raft: rewind snapshot scratch file: %w", err)
	}

	if restorer, ok := n.sm.(SnapshotStreamRestorer); ok {
		if err := restorer.RestoreSnapshotStream(scratch); err != nil {
			return fmt.Errorf("raft: restore state machine: %w", err)
		}
	} else {
		n.logger.Warn("raft: state machine cannot restore snapshot streams", "id", n.cfg.ID, "index", meta.Index)
	}

	if _, err := scratch.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("raft: rewind snapshot scratch file: %w", err)
	}
	if err := saveSnapshotFromReader(store, meta.Index, meta.Term, membership, scratch); err != nil {
		return fmt.Errorf("raft: persist installed snapshot: %w", err)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return ErrStopped
	}
	if meta.Index <= n.snapshot.Index {
		return nil
	}
	// Re-evaluate the suffix rule against the log as it is now: entries may
	// have been appended or replaced while the payload was streaming.
	keepSuffix := false
	if term, ok := n.termAtLocked(meta.Index); ok && term == meta.Term {
		keepSuffix = true
	}
	// Drop the divergent suffix first, then everything the snapshot covers,
	// so a crash between the two steps still reloads safely.
	if !keepSuffix {
		if err := n.store.Truncate(meta.Index + 1); err != nil {
			return fmt.Errorf("raft: truncate log after snapshot: %w", err)
		}
	}
	if err := truncatePrefixForSnapshot(n.store, meta.Index); err != nil {
		return fmt.Errorf("raft: truncate log prefix after snapshot: %w", err)
	}
	oldIndex := n.snapshot.Index
	if keepSuffix {
		n.entries = n.entries[meta.Index-oldIndex:]
	} else {
		n.entries = nil
		n.failPendingFromLocked(meta.Index+1, ErrNotLeader)
	}
	n.snapshot = Snapshot{Index: meta.Index, Term: meta.Term, Membership: cloneMembership(membership)}
	if n.commitIndex < meta.Index {
		n.commitIndex = meta.Index
	}
	if n.lastApplied < meta.Index {
		n.lastApplied = meta.Index
	}
	n.applyCond.Broadcast()
	if err := n.applySnapshotMembershipLocked(membership); err != nil {
		return err
	}
	return nil
}

// HandleInstallSnapshot implements the RPC receiver for InstallSnapshot. The
// snapshot is persisted and the state machine restored before success is
// reported.
func (n *Node) HandleInstallSnapshot(ctx context.Context, req *InstallSnapshotRequest) (*InstallSnapshotResponse, error) {
	if req == nil {
		return nil, errors.New("raft: nil InstallSnapshotRequest")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return nil, ErrStopped
	}
	if req.Term < n.term {
		term := n.term
		n.mu.Unlock()
		return &InstallSnapshotResponse{Term: term}, nil
	}
	if req.Term > n.term {
		if err := n.adoptTermLocked(req.Term, req.LeaderID); err != nil {
			n.mu.Unlock()
			return nil, err
		}
	} else {
		if n.role != Follower {
			n.stepDownLocked(req.LeaderID)
		} else {
			n.leaderID = req.LeaderID
		}
		n.resetElectionDeadlineLocked()
	}
	upToDate := req.Snapshot.Index <= n.snapshot.Index || req.Snapshot.Index <= n.commitIndex
	term := n.term
	n.mu.Unlock()

	if upToDate {
		n.mu.Lock()
		last := n.lastIndexLocked()
		term = n.term
		n.mu.Unlock()
		return &InstallSnapshotResponse{Term: term, Success: true, LastIndex: last}, nil
	}
	if err := n.RestoreSnapshot(req.Snapshot); err != nil {
		if errors.Is(err, ErrStaleSnapshotMembership) {
			// The local configuration is newer than the snapshot's
			// membership, so installing it would move the configuration
			// backwards. If the local log already reaches the snapshot index,
			// the prefix is covered and the leader can reconcile the suffix
			// with AppendEntries; reporting failure here would leave the
			// leader re-sending the same stale snapshot forever.
			n.mu.Lock()
			covered := req.Snapshot.Index <= n.lastIndexLocked()
			term := n.term
			n.mu.Unlock()
			if covered {
				return &InstallSnapshotResponse{Term: term, Success: true, LastIndex: req.Snapshot.Index}, nil
			}
		}
		return nil, err
	}
	n.mu.Lock()
	resp := &InstallSnapshotResponse{Term: n.term, Success: true, LastIndex: n.lastIndexLocked()}
	n.mu.Unlock()
	return resp, nil
}

// HandleRequestVote implements the RPC receiver. The term and vote are
// persisted before a vote is granted.
func (n *Node) HandleRequestVote(ctx context.Context, req *RequestVoteRequest) (*RequestVoteResponse, error) {
	if req == nil {
		return nil, errors.New("raft: nil RequestVoteRequest")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return nil, ErrStopped
	}
	// Adopt a higher term from any sender, as standard Raft requires, but only
	// current voters may win a vote. Removed nodes and learners are rejected
	// after the term update so an isolated stale replica cannot win an
	// election; a group that keeps seeing higher terms still converges to the
	// newest term.
	if req.Term < n.term {
		return &RequestVoteResponse{Term: n.term}, nil
	}
	if req.Term > n.term {
		if err := n.adoptTermLocked(req.Term, ""); err != nil {
			return nil, err
		}
	}
	// Elections use the latest configuration in the log, including a config
	// entry that is present but not yet applied (see electoralConfigLocked).
	// A candidate may belong to either voter set while joint (C_old or C_new);
	// the candidate still needs majorities of both sets to win, so this is the
	// standard joint-consensus election rule.
	electorate := n.electoralConfigLocked()
	if !configVotingMember(electorate, req.CandidateID) {
		return &RequestVoteResponse{Term: n.term, VoteGranted: false}, nil
	}
	// A learner never votes. A removed voter, however, must still answer: when
	// this node already applied the final entry of a joint change it is no
	// longer a voter, but peers that have not learned the final commit still
	// need a C_old majority to elect the leader that will deliver it. Their
	// candidate counts a vote only from members of its own electoral
	// configuration, so a removed node's grant cannot manufacture a quorum.
	if !configVotingMember(electorate, n.cfg.ID) && n.isLearnerLocked(n.cfg.ID) {
		return &RequestVoteResponse{Term: n.term, VoteGranted: false}, nil
	}
	lastIndex := n.lastIndexLocked()
	lastTerm := n.lastLogTermLocked()
	upToDate := req.LastLogTerm > lastTerm ||
		(req.LastLogTerm == lastTerm && req.LastLogIndex >= lastIndex)
	canVote := n.votedFor == "" || n.votedFor == req.CandidateID
	if !canVote || !upToDate {
		return &RequestVoteResponse{Term: n.term, VoteGranted: false}, nil
	}

	if n.votedFor != req.CandidateID {
		if err := n.store.SaveMeta(n.term, req.CandidateID); err != nil {
			return nil, fmt.Errorf("raft: persist vote: %w", err)
		}
		n.votedFor = req.CandidateID
	}
	n.resetElectionDeadlineLocked()
	return &RequestVoteResponse{Term: n.term, VoteGranted: true}, nil
}

// HandleAppendEntries implements the RPC receiver. Entries (and any conflict
// truncation) are persisted before success is reported.
func (n *Node) HandleAppendEntries(ctx context.Context, req *AppendEntriesRequest) (*AppendEntriesResponse, error) {
	if req == nil {
		return nil, errors.New("raft: nil AppendEntriesRequest")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return nil, ErrStopped
	}
	if req.Term < n.term {
		return &AppendEntriesResponse{Term: n.term}, nil
	}
	if req.Term > n.term {
		if err := n.adoptTermLocked(req.Term, req.LeaderID); err != nil {
			return nil, err
		}
	} else {
		if n.role != Follower {
			n.stepDownLocked(req.LeaderID)
		} else {
			n.leaderID = req.LeaderID
		}
		n.resetElectionDeadlineLocked()
	}

	prev := req.PrevLogIndex
	last := n.lastIndexLocked()
	if prev < n.snapshot.Index {
		// This node compacted past the leader's prefix; it is already further
		// ahead than the leader thinks. Report the snapshot index so the leader
		// can advance nextIndex and resume with AppendEntries.
		return &AppendEntriesResponse{Term: n.term, Success: true, MatchIndex: n.snapshot.Index}, nil
	}
	if prev > last {
		return &AppendEntriesResponse{Term: n.term, Success: false, ConflictIndex: last + 1}, nil
	}
	if prev > 0 {
		prevTerm, ok := n.termAtLocked(prev)
		if !ok || prevTerm != req.PrevLogTerm {
			return &AppendEntriesResponse{
				Term:          n.term,
				Success:       false,
				ConflictIndex: n.firstIndexOfTermLocked(prevTerm),
			}, nil
		}
	}

	// Reject malformed (non-contiguous) batches rather than corrupting the log.
	for i, e := range req.Entries {
		if e.Index != prev+uint64(i)+1 {
			return &AppendEntriesResponse{
				Term:          n.term,
				Success:       false,
				ConflictIndex: prev + uint64(i) + 1,
			}, nil
		}
	}

	lastNew := prev
	appendFrom := -1
	truncAt := uint64(0)
	for i, e := range req.Entries {
		if e.Index > last {
			appendFrom = i
			break
		}
		if term, ok := n.termAtLocked(e.Index); ok && term == e.Term {
			lastNew = e.Index
			continue
		}
		// Divergent suffix: drop it and append the leader's version.
		truncAt = e.Index
		appendFrom = i
		break
	}
	if appendFrom >= 0 {
		if truncAt > 0 {
			if err := n.store.Truncate(truncAt); err != nil {
				return nil, fmt.Errorf("raft: truncate conflicting suffix: %w", err)
			}
			n.entries = n.entries[:truncAt-n.snapshot.Index-1]
			// If this node was still tracking proposals that lived in the
			// removed suffix, fail them instead of letting them hang.
			n.failPendingFromLocked(truncAt, ErrNotLeader)
		}
		toAppend := cloneEntries(req.Entries[appendFrom:])
		if err := n.store.Append(toAppend...); err != nil {
			return nil, fmt.Errorf("raft: append replicated entries: %w", err)
		}
		n.entries = append(n.entries, toAppend...)
		lastNew = toAppend[len(toAppend)-1].Index
	} else {
		lastNew = prev + uint64(len(req.Entries))
	}

	if req.LeaderCommit > n.commitIndex {
		newCommit := req.LeaderCommit
		if newCommit > lastNew {
			newCommit = lastNew
		}
		if newCommit > n.commitIndex {
			n.commitIndex = newCommit
			n.applyCond.Broadcast()
		}
	}
	return &AppendEntriesResponse{Term: n.term, Success: true, MatchIndex: lastNew}, nil
}

// ---------------------------------------------------------------------------
// Background loops
// ---------------------------------------------------------------------------

// proposeLoop is the single leader-side group-commit drain goroutine. It waits
// for queued proposals, coalesces them for at most ProposeBatchWindow (or until
// ProposeBatchMax is reached) and hands the batch to commitProposalBatch.
//
// It runs for the whole node lifetime rather than per leadership term: while
// this node is not leader it simply fails anything that was queued before a
// step-down.
func (n *Node) proposeLoop() {
	for {
		select {
		case <-n.stopCh:
			n.mu.Lock()
			n.failQueuedLocked(ErrStopped)
			n.mu.Unlock()
			return
		case <-n.proposeSignal:
		}

		batch := n.collectProposals()
		if len(batch) > 0 {
			n.commitProposalBatch(batch)
		}
	}
}

// collectProposals drains queued proposals and keeps the batching window open
// for ProposeBatchWindow, adding proposals that arrive meanwhile. n.mu is never
// held during the window, so replication and the apply loop keep running; it is
// only taken briefly to move items off the queue.
func (n *Node) collectProposals() []*proposal {
	max := n.cfg.ProposeBatchMax
	batch := make([]*proposal, 0, max)

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return batch
	}
	if n.role != Leader {
		n.failQueuedLocked(ErrNotLeader)
		n.mu.Unlock()
		return batch
	}
	batch = n.drainQueueLocked(batch)
	n.mu.Unlock()

	if len(batch) >= max {
		return batch
	}

	timer := time.NewTimer(n.cfg.ProposeBatchWindow)
	defer timer.Stop()
	for {
		select {
		case <-n.stopCh:
			n.mu.Lock()
			n.failQueuedLocked(ErrStopped)
			n.mu.Unlock()
			return batch
		case <-n.proposeSignal:
			// More proposals arrived; grow the batch. The timer is not
			// reset, so continuous arrivals cannot extend latency forever.
			n.mu.Lock()
			if n.stopped {
				n.mu.Unlock()
				return batch
			}
			if n.role != Leader {
				n.failQueuedLocked(ErrNotLeader)
				n.mu.Unlock()
				return batch
			}
			batch = n.drainQueueLocked(batch)
			full := len(batch) >= max
			n.mu.Unlock()
			if full {
				return batch
			}
		case <-timer.C:
			n.mu.Lock()
			batch = n.drainQueueLocked(batch)
			n.mu.Unlock()
			return batch
		}
	}
}

// drainQueueLocked moves up to ProposeBatchMax queued proposals into batch.
// Caller must hold mu.
func (n *Node) drainQueueLocked(batch []*proposal) []*proposal {
	max := n.cfg.ProposeBatchMax
	for len(n.proposeQueue) > 0 && len(batch) < max {
		batch = append(batch, n.proposeQueue[0])
		n.proposeQueue[0] = nil
		n.proposeQueue = n.proposeQueue[1:]
	}
	if len(n.proposeQueue) == 0 {
		n.proposeQueue = nil
	}
	return batch
}

// commitProposalBatch assigns contiguous term/index pairs, performs one durable
// append and registers every waiter before replication is signalled.
//
// Locking: n.mu is held across the store call. That keeps log assignment and
// persistence ordered as a single writer with HandleAppendEntries, the leader
// no-op append and the snapshot/truncation paths, all of which already call the
// LogStore while holding n.mu. The batching window is deliberately outside the
// lock, and no path acquires a store lock before n.mu, so this cannot deadlock
// with the store or with Propose waiters (which never hold n.mu while waiting).
func (n *Node) commitProposalBatch(batch []*proposal) {
	n.mu.Lock()
	defer n.mu.Unlock()

	failAll := func(err error) {
		for _, p := range batch {
			n.completeLocked(p, err)
		}
	}
	if n.stopped {
		failAll(ErrStopped)
		return
	}
	if n.role != Leader {
		failAll(ErrNotLeader)
		return
	}

	// Drop proposals whose waiters already gave up, so they do not consume log
	// indices. Propose cannot change this state while mu is held.
	active := batch[:0]
	for _, p := range batch {
		if p.completed {
			continue
		}
		if p.canceled || p.ctx.Err() != nil {
			n.completeLocked(p, proposalContextError(p.ctx))
			continue
		}
		active = append(active, p)
	}
	batch = active
	if len(batch) == 0 {
		return
	}

	entries := make([]model.Entry, len(batch))
	index := n.lastIndexLocked() + 1
	for i, p := range batch {
		e := cloneEntry(p.cmd)
		e.Term = n.term
		e.Index = index + uint64(i)
		entries[i] = e
	}

	if bs, ok := n.store.(BatchAppender); ok {
		if err := bs.AppendBatch(entries); err != nil {
			failAll(fmt.Errorf("raft: append proposal batch: %w", err))
			return
		}
		for i, p := range batch {
			p.index = entries[i].Index
			n.pending[p.index] = p
		}
		n.entries = append(n.entries, entries...)
		if batchHasJointConfig(entries) {
			// New voters in the pending joint configuration must receive the
			// entry before it can commit; start their replicators now. The
			// configuration change gate stays closed until the final entry is
			// applied, even before the joint entry commits.
			n.configInFlight = true
			n.syncReplicatorsLocked()
		}
		n.signalAllLocked()
		n.maybeCommitLocked()
		return
	}

	// Fallback for stores without the batching capability: one Append per
	// entry, preserving the old per-proposal failure semantics. A failed
	// append fails only its own waiter and does not consume an index.
	for _, p := range batch {
		e := cloneEntry(p.cmd)
		e.Term = n.term
		e.Index = n.lastIndexLocked() + 1
		if err := n.store.Append(e); err != nil {
			n.completeLocked(p, fmt.Errorf("raft: append proposal: %w", err))
			continue
		}
		p.index = e.Index
		n.pending[e.Index] = p
		n.entries = append(n.entries, e)
		if e.Type == CommandJointConfig {
			n.configInFlight = true
			n.syncReplicatorsLocked()
		}
	}
	n.signalAllLocked()
	n.maybeCommitLocked()
}

// batchHasJointConfig reports whether entries contain a joint config entry.
func batchHasJointConfig(entries []model.Entry) bool {
	for _, e := range entries {
		if e.Type == CommandJointConfig {
			return true
		}
	}
	return false
}

func (n *Node) electionLoop(ctx context.Context) {
	interval := n.cfg.ElectionTimeoutMin / 4
	if interval < 2*time.Millisecond {
		interval = 2 * time.Millisecond
	}
	if interval > 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stopCh:
			return
		case <-ticker.C:
		}

		n.mu.Lock()
		if n.stopped {
			n.mu.Unlock()
			return
		}
		now := time.Now()
		switch n.role {
		case Leader:
			if !n.hasQuorumLocked(now) {
				n.logger.Warn("raft: leader lost quorum, stepping down", "id", n.cfg.ID, "term", n.term)
				n.stepDownWithErrorLocked("", errLostQuorum)
			}
		case Follower, Candidate:
			if !now.Before(n.electionDeadline) {
				n.mu.Unlock()
				n.beginElection()
				continue
			}
		}
		n.mu.Unlock()
	}
}

func (n *Node) beginElection() {
	n.mu.Lock()
	if n.stopped || n.role == Leader {
		n.mu.Unlock()
		return
	}
	if !n.selfIsElectoralVoterLocked() {
		// A learner (or a node removed from the configuration) never
		// campaigns. Keep the deadline moving so the election loop does not
		// spin on an expired timer.
		n.resetElectionDeadlineLocked()
		n.mu.Unlock()
		return
	}
	oldTerm, oldVotedFor := n.term, n.votedFor
	newTerm := n.term + 1
	if err := n.store.SaveMeta(newTerm, n.cfg.ID); err != nil {
		n.logger.Error("raft: persist candidacy failed", "id", n.cfg.ID, "error", err)
		n.term, n.votedFor = oldTerm, oldVotedFor
		n.mu.Unlock()
		return
	}
	n.term = newTerm
	n.votedFor = n.cfg.ID
	n.role = Candidate
	n.leaderID = ""
	n.votes = map[string]bool{n.cfg.ID: true}
	n.cancelLeaderLocked()
	n.resetElectionDeadlineLocked()

	req := &RequestVoteRequest{
		Term:         newTerm,
		CandidateID:  n.cfg.ID,
		LastLogIndex: n.lastIndexLocked(),
		LastLogTerm:  n.lastLogTermLocked(),
	}
	peers := n.electoralVotersExceptSelfLocked()
	n.wg.Add(len(peers))
	n.mu.Unlock()

	for _, peer := range peers {
		go func(peer string) {
			defer n.wg.Done()
			n.requestVote(peer, req, newTerm)
		}(peer)
	}

	// A single-node cluster (or one that already has a quorum via previous
	// grants) becomes leader immediately.
	n.mu.Lock()
	if n.role == Candidate && n.term == newTerm && n.voteQuorumLocked() {
		n.becomeLeaderLocked()
	}
	n.mu.Unlock()
}

func (n *Node) requestVote(peer string, req *RequestVoteRequest, electionTerm uint64) {
	ctx, cancel := n.rpcContext(n.voteTimeout())
	defer cancel()
	resp, err := n.tr.RequestVote(ctx, peer, req)
	if err != nil || resp == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped || n.role != Candidate || n.term != electionTerm || !n.selfIsElectoralVoterLocked() {
		return
	}
	if resp.Term > n.term {
		if err := n.adoptTermLocked(resp.Term, ""); err != nil {
			n.logger.Error("raft: adopt higher term failed", "id", n.cfg.ID, "error", err)
		}
		return
	}
	if !resp.VoteGranted {
		return
	}
	n.votes[peer] = true
	if n.voteQuorumLocked() {
		n.becomeLeaderLocked()
	}
}

// becomeLeaderLocked transitions to leader, appends the mandatory no-op entry
// and starts one replicator per peer. Caller must hold mu.
func (n *Node) becomeLeaderLocked() {
	if n.stopped || n.role != Candidate || !n.selfIsElectoralVoterLocked() {
		return
	}
	n.role = Leader
	n.leaderID = n.cfg.ID
	n.leaderNoopIndex = 0
	// A joint configuration is still an in-progress change: keep the gate
	// closed and let the finalizer complete it (when AutoFinalize is set).
	n.configInFlight = n.isJointLocked() || n.pendingConfigLocked()
	n.finalInFlight = false
	voters := n.votes
	n.votes = nil
	n.cancelLeaderLocked()

	leaderCtx, cancel := context.WithCancel(context.Background())
	n.leaderCtx = leaderCtx
	n.leaderCancel = cancel

	now := time.Now()
	targets := n.replicationTargetsLocked()
	n.nextIndex = make(map[string]uint64, len(targets))
	n.matchIndex = make(map[string]uint64, len(targets))
	n.lastAck = make(map[string]time.Time, len(targets))
	n.triggers = make(map[string]chan struct{}, len(targets))
	for _, p := range targets {
		// Only the peers that actually granted their vote are known to have
		// been reachable during the election. The election quorum provides a
		// short lease for ReadBarrier; other peers must heartbeat first.
		if voters[p] {
			n.lastAck[p] = now
		}
	}

	noop := model.Entry{
		Term:  n.term,
		Index: n.lastIndexLocked() + 1,
		Type:  model.CommandNoop,
	}
	if err := n.store.Append(noop); err != nil {
		n.logger.Error("raft: append leader no-op failed", "id", n.cfg.ID, "error", err)
		n.role = Follower
		n.leaderID = ""
		n.cancelLeaderLocked()
		n.failPendingLocked(ErrNotLeader)
		n.failQueuedLocked(ErrNotLeader)
		n.resetElectionDeadlineLocked()
		return
	}
	n.entries = append(n.entries, noop)
	n.leaderNoopIndex = noop.Index

	// syncReplicatorsLocked starts one loop per voter and learner, including
	// learners that were added before this node became leader.
	n.syncReplicatorsLocked()

	n.logger.Info("raft: became leader", "id", n.cfg.ID, "term", n.term, "last_index", n.lastIndexLocked())
	n.maybeCommitLocked()
	n.signalAllLocked()
	// If this node became leader in the middle of a single-change operation,
	// finish it: the joint target is fixed, and the final entry may have been
	// lost with the previous leader.
	n.signalAutoFinalLocked()
}

func (n *Node) replicateLoop(ctx context.Context, peer string, trigger <-chan struct{}, term uint64) {
	ticker := time.NewTicker(n.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stopCh:
			return
		case <-ticker.C:
		case <-trigger:
		}
		if !n.isLeaderFor(term) || !n.replicationTarget(peer) {
			return
		}
		for {
			retry := n.sendAppendEntries(ctx, peer, term)
			if !retry || !n.isLeaderFor(term) || !n.replicationTarget(peer) {
				break
			}
		}
	}
}

// sendAppendEntries sends one AppendEntries batch, or an InstallSnapshot when
// the follower is missing a compacted prefix. It returns true when an immediate
// retry is useful (more entries to send, a snapshot to install or a conflict to
// back up through).
func (n *Node) sendAppendEntries(leaderCtx context.Context, peer string, term uint64) bool {
	n.mu.Lock()
	if n.stopped || n.role != Leader || n.term != term {
		n.mu.Unlock()
		return false
	}
	last := n.lastIndexLocked()
	next := n.nextIndex[peer]
	if next < 1 {
		next = 1
	}
	if next > last+1 {
		next = last + 1
	}
	snapIndex := n.snapshot.Index
	if snapIndex > 0 && next <= snapIndex {
		// The entries the follower needs were compacted away.
		n.mu.Unlock()
		return n.sendInstallSnapshot(leaderCtx, peer, term)
	}
	prev := next - 1
	var prevTerm uint64
	if prev > 0 {
		prevTerm, _ = n.termAtLocked(prev)
	}
	req := &AppendEntriesRequest{
		Term:         term,
		LeaderID:     n.cfg.ID,
		PrevLogIndex: prev,
		PrevLogTerm:  prevTerm,
		Entries:      cloneEntries(n.entries[next-n.snapshot.Index-1:]),
		LeaderCommit: n.commitIndex,
	}
	n.mu.Unlock()

	rpcCtx, cancel := context.WithTimeout(leaderCtx, n.appendTimeout())
	defer cancel()
	resp, err := n.tr.AppendEntries(rpcCtx, peer, req)
	if err != nil || resp == nil {
		return false
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped || n.role != Leader || n.term != term || !n.isReplicationTargetLocked(peer) {
		return false
	}
	if resp.Term > n.term {
		if err := n.adoptTermLocked(resp.Term, ""); err != nil {
			n.logger.Error("raft: adopt higher term failed", "id", n.cfg.ID, "error", err)
		}
		return false
	}
	if resp.Success {
		match := prev + uint64(len(req.Entries))
		// A compacted follower reports its snapshot index, which can be ahead
		// of the batch the leader just sent.
		if resp.MatchIndex > match {
			match = resp.MatchIndex
		}
		if match > n.matchIndex[peer] {
			n.matchIndex[peer] = match
		}
		n.nextIndex[peer] = match + 1
		n.lastAck[peer] = time.Now()
		n.maybeCommitLocked()
		return n.nextIndex[peer] <= n.lastIndexLocked()
	}

	newNext := resp.ConflictIndex
	if newNext == 0 {
		newNext = prev
	}
	if newNext < 1 {
		newNext = 1
	}
	if newNext >= next {
		if next > 1 {
			newNext = next - 1
		} else {
			newNext = 1
		}
	}
	// A conflict at or below the compacted prefix means the follower may
	// already hold the snapshot boundary: a follower reports the snapshot
	// index when the conflicting term also covers that snapshot. Probe with
	// AppendEntries from snapshot.Index+1 first instead of repeating
	// InstallSnapshot, which the follower would answer as "already covered"
	// and the leader would loop on forever. Only when that boundary probe
	// itself fails does the follower actually need the payload.
	if snapIndex > 0 && newNext <= snapIndex {
		if prev == snapIndex {
			newNext = snapIndex
		} else {
			newNext = snapIndex + 1
		}
	}
	oldNext := n.nextIndex[peer]
	n.nextIndex[peer] = newNext
	return newNext < oldNext
}

// sendInstallSnapshot ships the current snapshot to a follower whose nextIndex
// falls inside the compacted prefix.
//
// When the transport supports chunked transfers and the log store exposes the
// snapshot as a stream, the payload is read in SnapshotChunkSize pieces and
// never materialised in memory. If either capability is missing, the legacy
// unary InstallSnapshot path is used. A failure after the first chunk is not
// retried through the legacy path: the next attempt starts a fresh chunked
// upload instead of allocating the whole snapshot.
func (n *Node) sendInstallSnapshot(leaderCtx context.Context, peer string, term uint64) bool {
	n.mu.Lock()
	if n.stopped || n.role != Leader || n.term != term {
		n.mu.Unlock()
		return false
	}
	snap := cloneSnapshot(n.snapshot)
	if snap.Index == 0 {
		n.mu.Unlock()
		return false
	}
	chunkTr, hasChunk := n.tr.(SnapshotChunkTransport)
	legacyTr, hasLegacy := n.tr.(SnapshotTransport)
	streamStore, hasStreamStore := n.store.(SnapshotStreamStore)
	n.mu.Unlock()

	if hasChunk && hasStreamStore {
		if ok, attempted := n.sendInstallSnapshotChunks(leaderCtx, peer, term, snap, streamStore, chunkTr); attempted {
			return ok
		}
	}
	if !hasLegacy {
		n.logger.Warn("raft: transport cannot install snapshots", "id", n.cfg.ID, "peer", peer)
		return false
	}
	req := &InstallSnapshotRequest{Term: term, LeaderID: n.cfg.ID, Snapshot: snap}

	if snap.Data == nil {
		// SnapshotToStore keeps only Index/Term in memory. The legacy
		// InstallSnapshot RPC still carries the whole payload as []byte, so
		// materialise it from the store on demand.
		data, err := n.materializeSnapshot(snap)
		if err != nil {
			n.logger.Warn("raft: cannot materialise streamed snapshot for install", "id", n.cfg.ID, "peer", peer, "error", err)
			return false
		}
		snap.Data = data
		req = &InstallSnapshotRequest{Term: term, LeaderID: n.cfg.ID, Snapshot: snap}
	}

	rpcCtx, cancel := context.WithTimeout(leaderCtx, n.appendTimeout())
	defer cancel()
	resp, err := legacyTr.InstallSnapshot(rpcCtx, peer, req)
	if err != nil || resp == nil {
		return false
	}

	n.mu.Lock()
	if n.stopped || n.role != Leader || n.term != term {
		n.mu.Unlock()
		return false
	}
	if resp.Term > n.term {
		if err := n.adoptTermLocked(resp.Term, ""); err != nil {
			n.logger.Error("raft: adopt higher term failed", "id", n.cfg.ID, "error", err)
		}
		n.mu.Unlock()
		return false
	}
	n.mu.Unlock()
	if !resp.Success {
		return false
	}
	return n.finishSnapshotInstall(peer, term, snap.Index, resp.LastIndex)
}

// materializeSnapshot reads a snapshot's payload from a streaming store so it
// can be sent through the legacy []byte InstallSnapshot RPC. It verifies the
// size and CRC before returning the bytes and does not mutate the node.
func (n *Node) materializeSnapshot(snap Snapshot) ([]byte, error) {
	store, ok := n.store.(SnapshotStreamStore)
	if !ok {
		return nil, errors.New("raft: snapshot data is not held in memory")
	}
	meta, rc, err := store.OpenSnapshot()
	if err != nil {
		return nil, err
	}
	if rc == nil {
		return nil, errors.New("raft: no stored snapshot")
	}
	defer rc.Close()
	if meta.Index != snap.Index || meta.Term != snap.Term {
		return nil, fmt.Errorf("raft: stored snapshot {%d,%d} does not match {%d,%d}", meta.Index, meta.Term, snap.Index, snap.Term)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != meta.Size || crc32.ChecksumIEEE(data) != meta.CRC32 {
		return nil, fmt.Errorf("raft: snapshot data at index %d is corrupt", meta.Index)
	}
	return data, nil
}

func (n *Node) applyLoop() {
	for {
		n.mu.Lock()
		for !n.stopped && n.lastApplied >= n.commitIndex {
			n.applyCond.Wait()
		}
		if n.stopped {
			n.mu.Unlock()
			return
		}
		n.mu.Unlock()

		// Serialise with snapshot creation/installation. applyMu is taken
		// before mu everywhere; it is never held while waiting on applyCond.
		n.applyMu.Lock()
		n.mu.Lock()
		if n.stopped {
			n.mu.Unlock()
			n.applyMu.Unlock()
			return
		}
		if n.lastApplied >= n.commitIndex {
			n.mu.Unlock()
			n.applyMu.Unlock()
			continue
		}
		if n.lastApplied < n.snapshot.Index {
			n.lastApplied = n.snapshot.Index
			n.mu.Unlock()
			n.applyMu.Unlock()
			continue
		}
		index := n.lastApplied + 1
		if index > n.lastIndexLocked() {
			// Defensive: a commit index beyond the log would otherwise panic.
			n.mu.Unlock()
			n.applyMu.Unlock()
			select {
			case <-n.stopCh:
				return
			case <-time.After(time.Millisecond):
			}
			continue
		}
		entry := cloneEntry(n.entries[index-n.snapshot.Index-1])
		n.mu.Unlock()

		var err error
		if isConfigCommand(entry.Type) {
			// Membership is Raft state, not application state: apply it here
			// so voters/learners update deterministically on every node and
			// never reach the user state machine.
			err = n.applyConfigEntry(entry)
		} else {
			err = n.sm.Apply(entry)
		}

		n.mu.Lock()
		n.lastApplied = index
		if isConfigCommand(entry.Type) {
			// Wake configuration waiters only after lastApplied covers the
			// entry, so they cannot observe a config ahead of the applied index.
			n.notifyConfigLocked()
		}
		if err != nil {
			n.logger.Error("raft: state machine apply failed", "id", n.cfg.ID, "index", index, "error", err)
		}
		if p := n.pending[index]; p != nil {
			delete(n.pending, index)
			n.completeLocked(p, err)
		}
		n.mu.Unlock()
		n.applyMu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// State helpers (caller holds n.mu)
// ---------------------------------------------------------------------------

func (n *Node) lastIndexLocked() uint64 {
	return n.snapshot.Index + uint64(len(n.entries))
}

// snapshotIndexLocked extends a state-machine snapshot index through the
// applied log entries that are Raft config commands. The state machine never
// sees config entries, so its reported applied index may be older than
// lastApplied; the membership embedded in the snapshot belongs to lastApplied.
// The index is only advanced while every intervening entry is a config command.
// Entries already covered by the current snapshot (i <= n.snapshot.Index) are
// skipped: the snapshot index was itself only advanced across config entries,
// so the compacted gap contains no state-machine mutations. An entry beyond the
// snapshot that is not a config command stops the extension. Caller holds mu.
func (n *Node) snapshotIndexLocked(index uint64) uint64 {
	if index == 0 || n.lastApplied <= index {
		return index
	}
	for i := index + 1; i <= n.lastApplied; i++ {
		if i <= n.snapshot.Index {
			// Covered by the current snapshot; it contains only Raft config
			// entries the state machine never observed.
			continue
		}
		e := n.entries[i-n.snapshot.Index-1]
		if !isConfigCommand(e.Type) {
			return index
		}
	}
	return n.lastApplied
}

func (n *Node) firstIndexLocked() uint64 {
	return n.snapshot.Index + 1
}

// termAtLocked returns the term of the entry at index. Index 0 has term 0; the
// compacted prefix is represented by the snapshot term.
func (n *Node) termAtLocked(index uint64) (uint64, bool) {
	if index == 0 {
		return 0, true
	}
	if n.snapshot.Index > 0 && index == n.snapshot.Index {
		return n.snapshot.Term, true
	}
	if index <= n.snapshot.Index || index > n.lastIndexLocked() {
		return 0, false
	}
	return n.entries[index-n.snapshot.Index-1].Term, true
}

func (n *Node) lastLogTermLocked() uint64 {
	if len(n.entries) > 0 {
		return n.entries[len(n.entries)-1].Term
	}
	return n.snapshot.Term
}

func (n *Node) firstIndexOfTermLocked(term uint64) uint64 {
	i := len(n.entries)
	for i > 0 && n.entries[i-1].Term == term {
		i--
	}
	if i == 0 {
		if n.snapshot.Index > 0 && n.snapshot.Term == term {
			return n.snapshot.Index
		}
		return n.snapshot.Index + 1
	}
	return n.snapshot.Index + uint64(i) + 1
}

func (n *Node) resetElectionDeadlineLocked() {
	minD := n.cfg.ElectionTimeoutMin
	maxD := n.cfg.ElectionTimeoutMax
	spread := maxD - minD
	d := minD
	if spread > 0 {
		d += time.Duration(rand.Int63n(int64(spread) + 1))
	}
	n.electionDeadline = time.Now().Add(d)
}

func (n *Node) cancelLeaderLocked() {
	if n.leaderCancel != nil {
		n.leaderCancel()
		n.leaderCancel = nil
		n.leaderCtx = nil
	}
}

func (n *Node) stepDownLocked(leaderID string) {
	n.stepDownWithErrorLocked(leaderID, ErrNotLeader)
}

func (n *Node) stepDownWithErrorLocked(leaderID string, pendingErr error) {
	if n.role == Leader {
		n.cancelLeaderLocked()
	}
	n.role = Follower
	n.leaderID = leaderID
	n.votes = nil
	n.configInFlight = false
	n.finalInFlight = false
	// Pending proposals can no longer be completed on this node. Failing them
	// here prevents a caller using a background context from hanging forever.
	n.failPendingLocked(pendingErr)
	n.failQueuedLocked(pendingErr)
	n.resetElectionDeadlineLocked()
}

// adoptTermLocked persists the new term (and an empty vote) before mutating the
// in-memory state. Caller must hold mu.
func (n *Node) adoptTermLocked(term uint64, leaderID string) error {
	if term <= n.term {
		return nil
	}
	if err := n.store.SaveMeta(term, ""); err != nil {
		return fmt.Errorf("raft: persist term: %w", err)
	}
	n.term = term
	n.votedFor = ""
	n.cancelLeaderLocked()
	n.role = Follower
	n.leaderID = leaderID
	n.votes = nil
	n.configInFlight = false
	n.finalInFlight = false
	n.failPendingLocked(ErrNotLeader)
	n.failQueuedLocked(ErrNotLeader)
	n.resetElectionDeadlineLocked()
	return nil
}

// failPendingLocked completes every pending proposal with err. Caller must
// hold mu. Exactly-once completion is guaranteed by deleting the entry before
// calling completeLocked, which also ignores an already-completed proposal.
func (n *Node) failPendingLocked(err error) {
	for index, p := range n.pending {
		delete(n.pending, index)
		n.completeLocked(p, err)
	}
}

// failPendingFromLocked is failPendingLocked for proposals at or above
// fromIndex (used when a conflicting log suffix is truncated).
func (n *Node) failPendingFromLocked(fromIndex uint64, err error) {
	for index, p := range n.pending {
		if index < fromIndex {
			continue
		}
		delete(n.pending, index)
		n.completeLocked(p, err)
	}
}

// failQueuedLocked completes proposals that have not been assigned a log index
// yet. Caller must hold mu.
func (n *Node) failQueuedLocked(err error) {
	for _, p := range n.proposeQueue {
		if p.ctx != nil && p.ctx.Err() != nil {
			n.completeLocked(p, proposalContextError(p.ctx))
			continue
		}
		n.completeLocked(p, err)
	}
	n.proposeQueue = nil
}

// completeLocked publishes a proposal result and closes its done channel
// exactly once. Caller must hold mu.
func (n *Node) completeLocked(p *proposal, err error) {
	if p.completed {
		return
	}
	p.err = err
	p.completed = true
	close(p.done)
}

func (n *Node) hasQuorumLocked(now time.Time) bool {
	return n.leaseQuorumLocked(now)
}

// maybeCommitLocked advances commitIndex to the highest entry from the current
// term stored on a quorum of the configuration that governs it. Entries from
// older terms are never committed directly; they are committed by induction
// once a current-term entry is committed. Caller holds mu.
//
// The governing configuration includes a config entry at the candidate index
// (so C_old,new itself needs both majorities and a later entry appended while
// it is uncommitted cannot commit under the old configuration alone). A pending
// final entry does not take effect here until it is applied, so leaving the
// joint state always requires majorities of both C_old and C_new.
func (n *Node) maybeCommitLocked() {
	last := n.lastIndexLocked()
	if last <= n.commitIndex {
		return
	}
	st := n.commitConfigThroughLocked(n.configStateLocked(), n.commitIndex)
	newCommit := n.commitIndex
	for index := n.commitIndex + 1; index <= last; index++ {
		// A config entry takes effect for its own commit (except a final
		// entry, which stays joint until applied).
		if index > n.snapshot.Index {
			st = nextCommitConfig(st, n.entries[index-n.snapshot.Index-1])
		}
		if term, ok := n.termAtLocked(index); ok && term == n.term &&
			n.commitQuorumInConfigLocked(st, index) {
			newCommit = index
		}
	}
	if newCommit > n.commitIndex {
		n.commitIndex = newCommit
		n.applyCond.Broadcast()
	}
}

func (n *Node) signalAllLocked() {
	for _, ch := range n.triggers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (n *Node) isLeaderFor(term uint64) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return !n.stopped && n.role == Leader && n.term == term
}

func (n *Node) voteTimeout() time.Duration {
	d := n.cfg.ElectionTimeoutMin
	if d < 50*time.Millisecond {
		d = 50 * time.Millisecond
	}
	return d
}

func (n *Node) appendTimeout() time.Duration {
	d := 3 * n.cfg.HeartbeatInterval
	if d < 100*time.Millisecond {
		d = 100 * time.Millisecond
	}
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

func (n *Node) rpcContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	base := n.runCtx
	if base == nil {
		base = context.Background()
	}
	return context.WithTimeout(base, timeout)
}
