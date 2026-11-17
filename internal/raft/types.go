// Package raft implements the Raft consensus core used by every shard.
//
// The package depends only on the standard library and internal/model. The
// durable log, the state machine and the peer-to-peer transport are injected
// through interfaces so that the core stays testable and transport agnostic.
//
// Membership uses joint consensus. A change first appends and commits a joint
// configuration C_old,new, then a final configuration C_new. While joined, both
// voter sets must reach a majority for elections, quorum leases and commit, so
// the old and new configurations cannot diverge and no second change may start.
// AddLearner, PromoteLearner and RemoveVoter keep their single-change API but
// are implemented as enter-joint followed by leave-joint. EnterJoint and
// LeaveJoint expose the two phases directly for callers that need to hold the
// joint configuration across several operations. Learners replicate but never
// vote or count towards a quorum. Voter/learner sets are persisted through the
// optional MembershipStore/JointMembershipStore capabilities (FileStore
// implements both) and are also rebuilt deterministically by replaying config
// entries.
//
// The current configuration is also embedded in every snapshot, together with a
// monotonically increasing config epoch. A node that was partitioned while
// membership changed may have to install a snapshot after its log prefix was
// compacted; the membership in that snapshot is authoritative, so the node
// cannot keep a stale voter set that no longer exists in the committed log.
// Snapshots written by older versions carry no membership: loading one falls
// back to the persisted MembershipStore record or the static Config. Restoring
// a snapshot whose membership config epoch is lower than this node's is
// rejected as stale. Joint snapshots additionally carry the old voter set and
// the auto-finalize intent, so a restart in the joint state can complete the
// pending change.
package raft

import (
	"context"
	"errors"
	"io"
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

	// Learner starts the node as a non-voting member: Peers are the current
	// voters and the local ID is a learner until a PromoteLearner config entry
	// is applied. It is used when a KV server creates a new range replica
	// before the leader has registered it as a learner. A learner never
	// campaigns and never grants votes.
	Learner bool
	// ForceLearner replaces any persisted membership with "Peers are voters
	// and the local node is a learner", and persists that immediately. It is
	// used by AddReplica when a group is restarted from an old data directory:
	// the stale on-disk voter state must not let the node campaign before the
	// leader has re-added it. Normal restarts must not set it.
	ForceLearner bool

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
	Voters      []string
	Learners    []string
	Learner     bool // the local node is currently a learner
	// Joint reports whether the node is in the intermediate C_old,new
	// configuration. While joint, VotersOld and VotersNew hold the two voter
	// sets and a quorum is required in both. Voters is their union.
	Joint bool
	// VotersOld is C_old while joint; nil otherwise.
	VotersOld []string
	// VotersNew is C_new while joint; nil otherwise. In the final state
	// (Joint == false) Voters itself is C_new.
	VotersNew []string
	// ConfigEpoch is the version of the voter/learner configuration. It starts
	// at 1 and increases with every effective membership change; a value of 0
	// only appears on a zero Status. A joint change advances the epoch twice:
	// once when C_old,new is applied and once when C_new is applied.
	ConfigEpoch uint64
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

// TimeoutNowRequest asks a follower to start an election immediately. It is
// sent by a leader that is transferring leadership; Term is the leader's term
// and LeaderID identifies the sender.
type TimeoutNowRequest struct {
	Term     uint64
	LeaderID string
}

// TimeoutNowResponse is the reply to a TimeoutNowRequest. Term is the
// follower's term at validation time; Success reports whether the follower
// accepted the request and started an election.
type TimeoutNowResponse struct {
	Term    uint64
	Success bool
}

// TimeoutNowTransport is the optional Transport capability used by
// Node.TransferLeadership. A transport that does not implement it makes
// TransferLeadership return ErrLeaderTransferUnsupported instead of moving
// leadership; normal Raft replication and elections are unaffected.
type TimeoutNowTransport interface {
	TimeoutNow(ctx context.Context, peer string, req *TimeoutNowRequest) (*TimeoutNowResponse, error)
}

// StateMachine consumes committed log entries in index order. Raft membership
// commands (config_add_learner, config_promote_learner, config_remove_voter,
// config_joint, config_final) are consumed by Raft itself and are not delivered
// to the state machine, so the indices it sees may have gaps; "in index order"
// means increasing, not necessarily contiguous.
type StateMachine interface {
	Apply(entry model.Entry) error
}

// SnapshotMembership is the Raft voter/learner configuration captured in a
// snapshot. ConfigEpoch is the monotonically increasing version of that
// configuration (it starts at 1 and advances with each effective membership
// change). A nil *SnapshotMembership means the snapshot predates membership
// metadata; an empty/nil Voters slice inside a non-nil value is corrupt and is
// rejected.
//
// Voters is C_new (the current voter set outside a joint configuration). When
// VotersOld is non-empty the snapshot captured the intermediate joint
// configuration C_old,new: elections and commits then need a majority of both
// VotersOld and Voters. AutoFinalize records that the joint configuration came
// from a single-change API call and should be completed to Voters automatically
// by the next leader; it is meaningful only while joint.
//
// Voters includes the node that produced the snapshot when it was a voter.
// Learners are non-voting members. All slices are sorted and de-duplicated when
// a snapshot is created or validated, and learners never overlap a voter.
type SnapshotMembership struct {
	Voters       []string `json:"voters"`
	VotersOld    []string `json:"voters_old,omitempty"`
	Learners     []string `json:"learners,omitempty"`
	AutoFinalize bool     `json:"auto_finalize,omitempty"`
	ConfigEpoch  uint64   `json:"config_epoch"`
}

// Joint reports whether the snapshot captured an intermediate C_old,new
// configuration. A nil envelope is never joint.
func (m *SnapshotMembership) Joint() bool {
	return m != nil && len(m.VotersOld) > 0
}

// Snapshot is a compact image of the state machine at Index. Term is the term
// of the log entry at Index, which lets Raft compare the snapshot with a
// follower's log during InstallSnapshot.
//
// Membership is the Raft voter/learner configuration in effect when the
// snapshot was taken, with the config epoch that produced it. It is nil only
// for a snapshot written by an older version (or by a caller that does not know
// the membership); installing such a snapshot keeps the node's current
// membership. A transport that forwards a Snapshot must preserve Membership:
// dropping it silently downgrades the install to the legacy fallback.
type Snapshot struct {
	Index      uint64
	Term       uint64
	Data       []byte
	Membership *SnapshotMembership
}

// SnapshotMeta describes snapshot data stored by a SnapshotStreamStore. It is
// the metadata a transport needs to ship a snapshot in chunks: the log
// position it covers and enough information to verify the received bytes.
//
// Size is the payload length in bytes and CRC32 is the IEEE CRC-32 (the same
// polynomial as crc32.ChecksumIEEE) of the payload. File is an
// implementation-defined locator for diagnostics; callers must use
// OpenSnapshot rather than opening it directly. For FileStore it is the data
// path relative to the store directory (for example
// "snapshot/data-42.bin").
//
// Membership carries the same optional membership envelope as Snapshot. It is
// nil for metadata written before membership was added to snapshots.
type SnapshotMeta struct {
	Index      uint64
	Term       uint64
	Size       int64
	CRC32      uint32
	File       string
	Membership *SnapshotMembership
}

// Snapshotter is the optional StateMachine capability used by Node.Snapshot.
// index is the last log index reflected in data. A state machine that does not
// implement Snapshotter makes Node.Snapshot return ErrSnapshotUnsupported.
type Snapshotter interface {
	Snapshot() (data []byte, index uint64, err error)
}

// SnapshotStreamer is the optional StateMachine capability used by
// Node.SnapshotToStore. StreamSnapshot writes the snapshot image to w without
// materialising it in memory and returns the last log index the image covers.
//
// The node calls StreamSnapshot while holding its apply mutex, so no Apply can
// run concurrently and the returned index is normally the node's last applied
// index. A state machine that only implements Snapshotter keeps using
// Node.Snapshot.
type SnapshotStreamer interface {
	StreamSnapshot(io.Writer) (uint64, error)
}

// SnapshotRestorer is the optional StateMachine capability used when a node
// installs a snapshot (either received from a leader or reloaded from disk). A
// state machine that does not implement it keeps its current state; Raft still
// persists and truncates the log, but the caller is responsible for restoring
// the data through some other channel.
type SnapshotRestorer interface {
	RestoreSnapshot(data []byte) error
}

// SnapshotStreamRestorer is the optional StateMachine capability used by
// Node.RestoreSnapshotStream. RestoreSnapshotStream consumes a snapshot image
// from r without requiring the whole payload in memory. The caller owns r and
// closes it after RestoreSnapshotStream returns.
type SnapshotStreamRestorer interface {
	RestoreSnapshotStream(io.Reader) error
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

// SnapshotStreamStore is the optional LogStore capability used to persist and
// reopen snapshot data without materialising it in memory. It is deliberately
// transport agnostic: a leader can OpenSnapshot and read it in arbitrary-sized
// chunks, while a follower can feed SaveSnapshotFromReader from any stream.
//
// SaveSnapshotFromReader durably stores the bytes read from r as the snapshot
// for index/term. It must be atomic with respect to the previously stored
// snapshot: if reading or writing fails, or a newer snapshot is already
// present, the previous snapshot must remain intact and loadable. A store may
// ignore a snapshot that is not newer than the current one; in that case it
// need not consume r.
//
// OpenSnapshot returns the metadata of the current snapshot and a reader over
// its data. The caller owns the reader and must close it. When no snapshot has
// been saved it returns the zero SnapshotMeta, a nil reader and a nil error.
// OpenSnapshot does not checksum the payload; callers that need integrity
// checks compare the bytes they read with Size and CRC32 (FileStore checks the
// file length when opening but not its CRC).
type SnapshotStreamStore interface {
	SaveSnapshotFromReader(index, term uint64, r io.Reader) error
	OpenSnapshot() (SnapshotMeta, io.ReadCloser, error)
}

// SnapshotMembershipStreamStore is the optional extension of
// SnapshotStreamStore used to persist the membership envelope together with a
// streamed snapshot. Node uses it when available so that SnapshotToStore and
// the streaming restore path keep membership durable; a store that only
// implements SnapshotStreamStore still works, but the snapshot it writes
// carries no membership (equivalent to a legacy snapshot). FileStore implements
// this interface.
//
// SaveSnapshotFromReaderWithMembership must obey the same atomicity and
// newer-snapshot rules as SaveSnapshotFromReader. A nil membership stores a
// legacy snapshot without membership metadata.
type SnapshotMembershipStreamStore interface {
	SnapshotStreamStore
	SaveSnapshotFromReaderWithMembership(index, term uint64, membership *SnapshotMembership, r io.Reader) error
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

// InstallSnapshotChunkRequest is one chunk of a chunked snapshot install. The
// leader reads the snapshot from a SnapshotStreamStore in SnapshotChunkSize
// pieces and sends sequential offsets; the final request has Done set and an
// empty Data at offset == total payload size. Size and CRC32 describe the whole
// payload and are repeated on every chunk (zero means not supplied).
type InstallSnapshotChunkRequest struct {
	Term         uint64
	LeaderID     string
	Index        uint64
	SnapshotTerm uint64
	Offset       uint64
	Data         []byte
	Done         bool
	Size         int64
	CRC32        uint32
	// Membership is the snapshot's optional membership envelope. Leaders repeat
	// it on every chunk (including the final Done marker) so the receiver can
	// validate that all chunks belong to the same snapshot; nil means the
	// snapshot is legacy/unknown and the receiver keeps its current membership.
	Membership *SnapshotMembership
}

// InstallSnapshotChunkResponse is the reply to one chunk. Term is the
// follower's current term, Success reports whether the chunk was accepted,
// LastIndex is set when the final chunk completed the install and Error
// describes a rejected chunk. NextOffset is the number of payload bytes
// accepted so far, i.e. the offset the receiver expects next; the leader uses
// it to resume a chunked upload after a lost response or a transient failure.
type InstallSnapshotChunkResponse struct {
	Term       uint64
	Success    bool
	LastIndex  uint64
	Error      string
	NextOffset int64
}

// SnapshotChunkTransport is the optional Transport capability used to stream
// snapshots in bounded chunks. When both the transport and the node's log
// store support streaming, Node sends snapshots through it instead of
// materialising the payload for SnapshotTransport. A transport that only
// implements SnapshotTransport keeps the legacy unary path.
type SnapshotChunkTransport interface {
	InstallSnapshotChunk(ctx context.Context, peer string, req *InstallSnapshotChunkRequest) (*InstallSnapshotChunkResponse, error)
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

// MembershipStore is the optional LogStore capability used to persist the Raft
// voter/learner set. It is separate from the log: a config entry is replayed
// after a restart only when it is still in the log (it may have been compacted
// into a snapshot), so the current membership must be durable on its own.
//
// LoadMembership returns the last saved membership. A store that has never
// saved one returns nil, nil (a nil voter slice means "not persisted", not an
// empty configuration). Voters include the local node. Implementations must
// keep old files loadable: a missing or legacy state simply yields nil, nil.
type MembershipStore interface {
	SaveMembership(voters, learners []string) error
	LoadMembership() (voters, learners []string, err error)
}

// MembershipEpochStore is the optional extension of MembershipStore that also
// persists the membership config epoch. Node uses it when available so an
// installed snapshot's epoch survives a restart; a store that only implements
// MembershipStore still works, and an epoch of 0 is then normalized to the
// initial epoch (1). FileStore implements this interface.
//
// LoadMembershipEpoch returns nil, nil, 0, nil when no membership has ever been
// saved. A record written by an older version has epoch 0 and is reported as
// such.
type MembershipEpochStore interface {
	MembershipStore
	SaveMembershipEpoch(voters, learners []string, epoch uint64) error
	LoadMembershipEpoch() (voters, learners []string, epoch uint64, err error)
}

// JointMembershipStore is the optional extension of MembershipEpochStore that
// can persist an intermediate joint configuration. Node uses it when available
// so a restart while joint (or while a single-change API is completing) keeps
// C_old and the auto-finalize intent. A store that only implements
// MembershipStore/MembershipEpochStore still works, but a joint configuration
// is then persisted as its C_new voter set only: the node may restart in the
// final configuration or, if the joint entry is still in the log, replay it.
// FileStore implements this interface.
//
// LoadJointMembership returns nil, nil, nil, false, 0, nil when no membership
// has ever been saved. voters is C_new; votersOld is non-empty exactly while
// the persisted configuration is joint. Implementations must keep older
// records loadable.
type JointMembershipStore interface {
	MembershipEpochStore
	SaveJointMembership(votersOld, voters, learners []string, autoFinalize bool, epoch uint64) error
	LoadJointMembership() (votersOld, voters, learners []string, autoFinalize bool, epoch uint64, err error)
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
	// ErrSnapshotStreamUnsupported is returned when the streaming snapshot API
	// is used with a StateMachine or LogStore that lacks the corresponding
	// optional capability.
	ErrSnapshotStreamUnsupported = errors.New("raft: snapshot streaming unsupported")
	// ErrUnknownTarget is returned when a leadership transfer names a node
	// that is not one of this leader's peers.
	ErrUnknownTarget = errors.New("raft: unknown leadership transfer target")
	// ErrTargetNotCaughtUp is returned when a leadership transfer target has
	// not replicated the leader's log within the bounded catch-up wait.
	ErrTargetNotCaughtUp = errors.New("raft: leadership transfer target not caught up")
	// ErrLeaderTransferUnsupported is returned when the configured Transport
	// cannot deliver TimeoutNow messages.
	ErrLeaderTransferUnsupported = errors.New("raft: leadership transfer unsupported by transport")
	// ErrStaleTimeoutNow is returned when a TimeoutNow request carries a term
	// or leader that no longer matches this follower's state.
	ErrStaleTimeoutNow = errors.New("raft: stale TimeoutNow request")

	// ErrAlreadyVoter is returned when AddLearner names a current voter.
	ErrAlreadyVoter = errors.New("raft: peer is already a voter")
	// ErrAlreadyLearner is returned when AddLearner names a current learner.
	ErrAlreadyLearner = errors.New("raft: peer is already a learner")
	// ErrNotLearner is returned when PromoteLearner names a peer that is not a
	// learner.
	ErrNotLearner = errors.New("raft: peer is not a learner")
	// ErrNotVoter is returned when RemoveVoter names a peer that is not a
	// voter.
	ErrNotVoter = errors.New("raft: peer is not a voter")
	// ErrUnsafeConfigChange is returned when a membership change would leave
	// the group without voters (for example removing the last voter).
	ErrUnsafeConfigChange = errors.New("raft: unsafe membership change")
	// ErrConfigChangeInProgress is returned when a membership change is
	// already being applied. Changes are strictly one at a time, and no other
	// configuration change may start while the group is joint.
	ErrConfigChangeInProgress = errors.New("raft: membership change already in progress")
	// ErrNoJointConfig is returned by LeaveJoint when the node is not in a
	// joint configuration.
	ErrNoJointConfig = errors.New("raft: no joint configuration")
	// ErrStaleConfigChange is returned when a configuration entry or snapshot
	// carries an epoch older than the applied configuration, or when applying
	// it would regress or conflict with the current configuration. Stale
	// entries are never applied.
	ErrStaleConfigChange = errors.New("raft: stale configuration")
	// ErrInvalidConfigEntry is returned for a malformed joint or final
	// configuration entry (missing voters, duplicate/empty ids, learners that
	// overlap voters, a missing epoch, or a missing old voter set on a joint
	// entry).
	ErrInvalidConfigEntry = errors.New("raft: invalid configuration entry")
	// ErrInvalidSnapshotMembership is returned when a snapshot carries a
	// membership envelope that is structurally invalid (no voters, an empty
	// id, duplicates, a voter that is also a learner, or no config epoch).
	ErrInvalidSnapshotMembership = errors.New("raft: invalid snapshot membership")
	// ErrStaleSnapshotMembership is returned when a snapshot's membership
	// config epoch is older than this node's current epoch. A committed
	// snapshot can never legitimately be older than the configuration this
	// node has already applied; accepting it could resurrect removed voters.
	ErrStaleSnapshotMembership = errors.New("raft: stale snapshot membership")
)

// errLostQuorum classifies a proposal that was accepted while this node was
// leader but can no longer be completed because the leader lost its quorum
// lease. It matches both ErrNotLeader (the node stepped down) and ErrTimeout
// (the entry will not commit), so callers using errors.Is either way work.
var errLostQuorum = errors.Join(ErrNotLeader, ErrTimeout)

// isConfigCommand reports whether the entry is a Raft membership change that
// the node applies itself instead of forwarding to the state machine.
func isConfigCommand(t model.CommandType) bool {
	switch t {
	case model.CommandAddLearner, model.CommandPromoteLearner, model.CommandRemoveVoter,
		CommandJointConfig, CommandFinalConfig:
		return true
	default:
		return false
	}
}

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
	c.Membership = cloneMembership(s.Membership)
	return c
}

// cloneMembership returns a deep copy of a snapshot membership envelope. A nil
// envelope stays nil so callers can distinguish a legacy snapshot from an empty
// (invalid) configuration.
func cloneMembership(m *SnapshotMembership) *SnapshotMembership {
	if m == nil {
		return nil
	}
	c := &SnapshotMembership{ConfigEpoch: m.ConfigEpoch, AutoFinalize: m.AutoFinalize}
	if m.Voters != nil {
		c.Voters = append([]string(nil), m.Voters...)
	}
	if m.VotersOld != nil {
		c.VotersOld = append([]string(nil), m.VotersOld...)
	}
	if m.Learners != nil {
		c.Learners = append([]string(nil), m.Learners...)
	}
	return c
}

// equalStrings reports whether two sorted string slices are identical. A nil
// slice and an empty slice are considered equal.
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

// membershipEqual reports whether two membership envelopes are semantically
// identical. Both nil envelopes are equal; nil and non-nil are not.
func membershipEqual(a, b *SnapshotMembership) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.ConfigEpoch == b.ConfigEpoch &&
		a.AutoFinalize == b.AutoFinalize &&
		equalStrings(a.Voters, b.Voters) &&
		equalStrings(a.VotersOld, b.VotersOld) &&
		equalStrings(a.Learners, b.Learners)
}
