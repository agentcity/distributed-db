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

var (
	// ErrNotLeader is returned when an operation requires leadership.
	ErrNotLeader = errors.New("raft: not leader")
	// ErrTimeout is returned when a proposal could not be committed in time.
	ErrTimeout = errors.New("raft: proposal timed out")
	// ErrStopped is returned when a node is used after Stop.
	ErrStopped = errors.New("raft: stopped")
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
