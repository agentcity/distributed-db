package raft

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"distributed-db/internal/model"
)

// proposal tracks one client proposal until it is applied or abandoned.
type proposal struct {
	done chan struct{}
	err  error
}

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
	commitIndex      uint64
	lastApplied      uint64
	leaderNoopIndex  uint64
	role             Role
	leaderID         string
	electionDeadline time.Time
	votes            map[string]bool
	nextIndex        map[string]uint64
	matchIndex       map[string]uint64
	lastAck          map[string]time.Time
	triggers         map[string]chan struct{}
	pending          map[uint64]*proposal

	applyCond *sync.Cond
	stopCh    chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup

	runCtx       context.Context
	cancel       context.CancelFunc
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

	state, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("raft: load persistent state: %w", err)
	}
	for i, e := range state.Entries {
		if e.Index != uint64(i)+1 {
			return nil, fmt.Errorf("raft: non-contiguous log: entry %d has index %d", i+1, e.Index)
		}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	n := &Node{
		cfg:        cfg,
		store:      store,
		sm:         sm,
		tr:         tr,
		logger:     logger,
		term:       state.Term,
		votedFor:   state.VotedFor,
		entries:    cloneEntries(state.Entries),
		role:       Follower,
		votes:      make(map[string]bool),
		nextIndex:  make(map[string]uint64),
		matchIndex: make(map[string]uint64),
		lastAck:    make(map[string]time.Time),
		triggers:   make(map[string]chan struct{}),
		pending:    make(map[uint64]*proposal),
		stopCh:     make(chan struct{}),
	}
	n.applyCond = sync.NewCond(&n.mu)
	return n, nil
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
	n.wg.Add(3)
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
	n.applyCond.Broadcast()
	n.mu.Unlock()
	close(n.stopCh)
}

// Propose appends cmd on the leader and returns only after the entry is
// committed and applied. Term/Index are filled in by Raft.
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
	index := n.lastIndexLocked() + 1
	entry := cloneEntry(model.Entry{
		Term:  n.term,
		Index: index,
		Type:  cmd.Type,
		Key:   cmd.Key,
		Value: cmd.Value,
	})
	if err := n.store.Append(entry); err != nil {
		n.mu.Unlock()
		return 0, fmt.Errorf("raft: append proposal: %w", err)
	}
	n.entries = append(n.entries, entry)
	p := &proposal{done: make(chan struct{})}
	n.pending[index] = p
	n.signalAllLocked()
	n.maybeCommitLocked()
	n.mu.Unlock()

	select {
	case <-p.done:
		if p.err != nil {
			return 0, p.err
		}
		return index, nil
	case <-n.stopCh:
		n.mu.Lock()
		stillPending := n.pending[index] == p
		n.mu.Unlock()
		if stillPending {
			return 0, ErrStopped
		}
		if p.err != nil {
			return 0, p.err
		}
		return index, nil
	case <-ctx.Done():
		n.mu.Lock()
		if n.pending[index] == p {
			delete(n.pending, index)
			n.mu.Unlock()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 0, ErrTimeout
			}
			return 0, ctx.Err()
		}
		err := p.err
		n.mu.Unlock()
		if err != nil {
			return 0, err
		}
		return index, nil
	}
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
	return Status{
		ID:          n.cfg.ID,
		Role:        n.role,
		Term:        n.term,
		Leader:      n.leaderID,
		CommitIndex: n.commitIndex,
		LastIndex:   uint64(len(n.entries)),
		LastApplied: n.lastApplied,
	}
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
	if req.Term < n.term {
		return &RequestVoteResponse{Term: n.term}, nil
	}
	if req.Term > n.term {
		if err := n.adoptTermLocked(req.Term, ""); err != nil {
			return nil, err
		}
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
	if prev > last {
		return &AppendEntriesResponse{Term: n.term, Success: false, ConflictIndex: last + 1}, nil
	}
	if prev > 0 && n.entries[prev-1].Term != req.PrevLogTerm {
		return &AppendEntriesResponse{
			Term:          n.term,
			Success:       false,
			ConflictIndex: n.firstIndexOfTermLocked(n.entries[prev-1].Term),
		}, nil
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
		if n.entries[e.Index-1].Term == e.Term {
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
			n.entries = n.entries[:truncAt-1]
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
	peers := append([]string(nil), n.cfg.Peers...)
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
	if n.role == Candidate && n.term == newTerm && len(n.votes) >= n.majority() {
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
	if n.stopped || n.role != Candidate || n.term != electionTerm {
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
	if len(n.votes) >= n.majority() {
		n.becomeLeaderLocked()
	}
}

// becomeLeaderLocked transitions to leader, appends the mandatory no-op entry
// and starts one replicator per peer. Caller must hold mu.
func (n *Node) becomeLeaderLocked() {
	if n.stopped || n.role != Candidate {
		return
	}
	n.role = Leader
	n.leaderID = n.cfg.ID
	n.leaderNoopIndex = 0
	voters := n.votes
	n.votes = nil
	n.cancelLeaderLocked()

	leaderCtx, cancel := context.WithCancel(context.Background())
	n.leaderCancel = cancel

	now := time.Now()
	n.nextIndex = make(map[string]uint64, len(n.cfg.Peers))
	n.matchIndex = make(map[string]uint64, len(n.cfg.Peers))
	n.lastAck = make(map[string]time.Time, len(n.cfg.Peers))
	n.triggers = make(map[string]chan struct{}, len(n.cfg.Peers))
	for _, p := range n.cfg.Peers {
		n.nextIndex[p] = n.lastIndexLocked() + 1
		n.matchIndex[p] = 0
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
		n.resetElectionDeadlineLocked()
		return
	}
	n.entries = append(n.entries, noop)
	n.leaderNoopIndex = noop.Index

	for _, peer := range n.cfg.Peers {
		trigger := make(chan struct{}, 1)
		n.triggers[peer] = trigger
		n.wg.Add(1)
		go func(peer string, trigger <-chan struct{}, term uint64) {
			defer n.wg.Done()
			n.replicateLoop(leaderCtx, peer, trigger, term)
		}(peer, trigger, n.term)
	}

	n.logger.Info("raft: became leader", "id", n.cfg.ID, "term", n.term, "last_index", n.lastIndexLocked())
	n.maybeCommitLocked()
	n.signalAllLocked()
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
		if !n.isLeaderFor(term) {
			return
		}
		for {
			retry := n.sendAppendEntries(ctx, peer, term)
			if !retry || !n.isLeaderFor(term) {
				break
			}
		}
	}
}

// sendAppendEntries sends one AppendEntries batch. It returns true when an
// immediate retry is useful (more entries to send or a conflict to back up
// through).
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
	prev := next - 1
	var prevTerm uint64
	if prev > 0 {
		prevTerm = n.entries[prev-1].Term
	}
	req := &AppendEntriesRequest{
		Term:         term,
		LeaderID:     n.cfg.ID,
		PrevLogIndex: prev,
		PrevLogTerm:  prevTerm,
		Entries:      cloneEntries(n.entries[next-1:]),
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
	if n.stopped || n.role != Leader || n.term != term {
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
	oldNext := n.nextIndex[peer]
	n.nextIndex[peer] = newNext
	return newNext < oldNext
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
		index := n.lastApplied + 1
		if index > n.lastIndexLocked() {
			// Defensive: a commit index beyond the log would otherwise panic.
			n.mu.Unlock()
			select {
			case <-n.stopCh:
				return
			case <-time.After(time.Millisecond):
			}
			continue
		}
		entry := cloneEntry(n.entries[index-1])
		n.mu.Unlock()

		err := n.sm.Apply(entry)

		n.mu.Lock()
		n.lastApplied = index
		if err != nil {
			n.logger.Error("raft: state machine apply failed", "id", n.cfg.ID, "index", index, "error", err)
		}
		if p := n.pending[index]; p != nil {
			p.err = err
			delete(n.pending, index)
			close(p.done)
		}
		n.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// State helpers (caller holds n.mu)
// ---------------------------------------------------------------------------

func (n *Node) majority() int {
	return (len(n.cfg.Peers)+1)/2 + 1
}

func (n *Node) lastIndexLocked() uint64 {
	return uint64(len(n.entries))
}

func (n *Node) lastLogTermLocked() uint64 {
	if len(n.entries) == 0 {
		return 0
	}
	return n.entries[len(n.entries)-1].Term
}

func (n *Node) firstIndexOfTermLocked(term uint64) uint64 {
	i := len(n.entries)
	for i > 0 && n.entries[i-1].Term == term {
		i--
	}
	return uint64(i) + 1
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
	// Pending proposals can no longer be completed on this node. Failing them
	// here prevents a caller using a background context from hanging forever.
	n.failPendingLocked(pendingErr)
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
	n.failPendingLocked(ErrNotLeader)
	n.resetElectionDeadlineLocked()
	return nil
}

// failPendingLocked completes every pending proposal with err. Caller must
// hold mu. Exactly-once completion is guaranteed by deleting the entry before
// closing its done channel.
func (n *Node) failPendingLocked(err error) {
	for index, p := range n.pending {
		p.err = err
		delete(n.pending, index)
		close(p.done)
	}
}

// failPendingFromLocked is failPendingLocked for proposals at or above
// fromIndex (used when a conflicting log suffix is truncated).
func (n *Node) failPendingFromLocked(fromIndex uint64, err error) {
	for index, p := range n.pending {
		if index < fromIndex {
			continue
		}
		p.err = err
		delete(n.pending, index)
		close(p.done)
	}
}

func (n *Node) hasQuorumLocked(now time.Time) bool {
	count := 1 // self
	for _, peer := range n.cfg.Peers {
		if ts, ok := n.lastAck[peer]; ok && now.Sub(ts) <= n.cfg.ElectionTimeoutMin {
			count++
		}
	}
	return count >= n.majority()
}

// maybeCommitLocked advances commitIndex to the highest entry from the current
// term stored on a strict majority. Entries from older terms are never
// committed directly; they are committed by induction once a current-term
// entry is committed. Caller must hold mu.
func (n *Node) maybeCommitLocked() {
	last := n.lastIndexLocked()
	for index := last; index > n.commitIndex; index-- {
		// Skip foreign-term entries and keep scanning: a lower entry from the
		// current term may already have a majority even if a higher one does
		// not. Only current-term entries are eligible for direct commit.
		if n.entries[index-1].Term != n.term {
			continue
		}
		count := 1 // self
		for _, peer := range n.cfg.Peers {
			if n.matchIndex[peer] >= index {
				count++
			}
		}
		if count >= n.majority() {
			n.commitIndex = index
			n.applyCond.Broadcast()
			return
		}
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
