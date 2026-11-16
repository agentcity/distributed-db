// Package raft implements the Raft consensus core used by every shard.
//
// The package depends only on the standard library and internal/model. The
// durable log, the state machine and the peer-to-peer transport are injected
// through interfaces so that the core stays testable and transport agnostic.
package raft

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"distributed-db/internal/model"
)

// Role is the Raft state of a node.
type Role string

const (
	// Follower passively replicates the leader's log.
	Follower Role = "follower"
	// Candidate is soliciting votes for a new term.
	Candidate Role = "candidate"
	// Leader replicates entries and serves linearizable reads.
	Leader Role = "leader"
)

// Config configures a Node.
type Config struct {
	ID                 string        // unique id == raft address
	Peers              []string      // other members, ids are addresses
	DataDir            string        // WAL directory (used by callers to build the store)
	ElectionTimeoutMin time.Duration // e.g. 300ms
	ElectionTimeoutMax time.Duration // e.g. 600ms
	HeartbeatInterval  time.Duration // e.g. 100ms
	Logger             *slog.Logger

	// ProposeBatchWindow is how long the leader waits for more concurrent
	// proposals before committing a batch to the LogStore. It bounds the
	// added latency of a lone proposal. Zero or negative selects the default
	// (1ms); the window never delays a batch that is already full.
	ProposeBatchWindow time.Duration
	// ProposeBatchMax is the maximum number of proposals coalesced into one
	// durable append. Zero or negative selects the default (64).
	ProposeBatchMax int
}

// Status is a point-in-time view of a node.
type Status struct {
	ID          string
	Role        Role
	Term        uint64
	Leader      string
	CommitIndex uint64
	LastIndex   uint64
	LastApplied uint64
}

// RequestVoteRequest is sent by a candidate to its peers.
type RequestVoteRequest struct {
	Term         uint64
	CandidateID  string
	LastLogIndex uint64
	LastLogTerm  uint64
}

// RequestVoteResponse is the reply to a RequestVoteRequest.
type RequestVoteResponse struct {
	Term        uint64
	VoteGranted bool
}

// AppendEntriesRequest is sent by the leader to replicate entries and as a
// heartbeat.
type AppendEntriesRequest struct {
	Term         uint64
	LeaderID     string
	PrevLogIndex uint64
	PrevLogTerm  uint64
	Entries      []model.Entry
	LeaderCommit uint64
}

// AppendEntriesResponse is the reply to an AppendEntriesRequest.
type AppendEntriesResponse struct {
	Term          uint64
	Success       bool
	MatchIndex    uint64
	ConflictIndex uint64
}

// Transport delivers Raft RPCs to peers. Implementations must be safe for
// concurrent use.
type Transport interface {
	RequestVote(ctx context.Context, peer string, req *RequestVoteRequest) (*RequestVoteResponse, error)
	AppendEntries(ctx context.Context, peer string, req *AppendEntriesRequest) (*AppendEntriesResponse, error)
}

// StateMachine consumes committed log entries in index order.
type StateMachine interface {
	Apply(entry model.Entry) error
}

// Snapshot is a compact image of the state machine at Index. Term is the term
// of the log entry at Index, which lets Raft compare the snapshot with a
// follower's log during InstallSnapshot.
type Snapshot struct {
	Index uint64
	Term  uint64
	Data  []byte
}

// Snapshotter is the optional StateMachine capability used by Node.Snapshot.
// index is the last log index reflected in data. A state machine that does not
// implement Snapshotter makes Node.Snapshot return ErrSnapshotUnsupported.
type Snapshotter interface {
	Snapshot() (data []byte, index uint64, err error)
}

// SnapshotRestorer is the optional StateMachine capability used when a node
// installs a snapshot (either received from a leader or reloaded from disk). A
// state machine that does not implement it keeps its current state; Raft still
// persists and truncates the log, but the caller is responsible for restoring
// the data through some other channel.
type SnapshotRestorer interface {
	RestoreSnapshot(data []byte) error
}

// SnapshotStore is the optional LogStore capability used for local log
// compaction. Stores that do not implement it keep working: Node.Snapshot then
// compacts only the in-memory log and does not persist a snapshot. TruncatePrefix
// removes all entries with Index <= index from the durable log. LoadSnapshot
// returns the zero Snapshot and a nil error when no snapshot has been saved.
type SnapshotStore interface {
	SaveSnapshot(snapshot Snapshot) error
	LoadSnapshot() (Snapshot, error)
	TruncatePrefix(index uint64) error
}

// InstallSnapshotRequest is sent by a leader whose follower is missing a
// compacted log prefix.
type InstallSnapshotRequest struct {
	Term     uint64
	LeaderID string
	Snapshot Snapshot
}

// InstallSnapshotResponse is the reply to an InstallSnapshotRequest. LastIndex
// is the follower's last log index after a successful install.
type InstallSnapshotResponse struct {
	Term      uint64
	Success   bool
	LastIndex uint64
}

// SnapshotTransport is the optional Transport capability used to ship
// snapshots. A Transport that does not implement it cannot be used to bring a
// lagging follower up to date after log compaction; the leader keeps trying
// AppendEntries instead.
type SnapshotTransport interface {
	InstallSnapshot(ctx context.Context, peer string, req *InstallSnapshotRequest) (*InstallSnapshotResponse, error)
}

// PersistentState is everything Raft must recover after a restart.
type PersistentState struct {
	Term     uint64
	VotedFor string
	Entries  []model.Entry
}

// LogStore is durable: SaveMeta and Append must fsync before returning.
// Entries are 1-based and contiguous. Truncate removes Index >= fromIndex.
type LogStore interface {
	Load() (PersistentState, error)
	SaveMeta(term uint64, votedFor string) error
	Append(entries ...model.Entry) error
	Truncate(fromIndex uint64) error
}

// BatchAppender is the optional LogStore capability used for group commit. A
// store that implements it can persist a contiguous batch of entries with one
// durable write (one fsync for FileStore) instead of one append per proposal.
//
// AppendBatch is all-or-nothing: on error the durable log must be unchanged
// and the batch must not be retried by the caller; the caller fails every
// proposal in that batch. Stores that do not implement BatchAppender keep
// working through LogStore.Append, which Node falls back to one entry at a
// time.
type BatchAppender interface {
	AppendBatch(entries []model.Entry) error
}

var (
	// ErrNotLeader is returned when an operation requires leadership.
	ErrNotLeader = errors.New("raft: not leader")
	// ErrTimeout is returned when a proposal could not be committed in time.
	ErrTimeout = errors.New("raft: proposal timed out")
	// ErrStopped is returned when a node is used after Stop.
	ErrStopped = errors.New("raft: stopped")
	// ErrSnapshotUnsupported is returned when the configured StateMachine
	// cannot produce snapshots.
	ErrSnapshotUnsupported = errors.New("raft: snapshots unsupported by state machine")
)

// errLostQuorum classifies a proposal that was accepted while this node was
// leader but can no longer be completed because the leader lost its quorum
// lease. It matches both ErrNotLeader (the node stepped down) and ErrTimeout
// (the entry will not commit), so callers using errors.Is either way work.
var errLostQuorum = errors.Join(ErrNotLeader, ErrTimeout)

// cloneEntry returns a deep copy so callers cannot mutate the log or the state
// machine through shared Value slices.
func cloneEntry(e model.Entry) model.Entry {
	c := e
	if e.Value != nil {
		c.Value = append([]byte(nil), e.Value...)
	}
	return c
}

func cloneEntries(entries []model.Entry) []model.Entry {
	if entries == nil {
		return nil
	}
	out := make([]model.Entry, len(entries))
	for i, e := range entries {
		out[i] = cloneEntry(e)
	}
	return out
}

func cloneSnapshot(s Snapshot) Snapshot {
	c := s
	if s.Data != nil {
		c.Data = append([]byte(nil), s.Data...)
	}
	return c
}
