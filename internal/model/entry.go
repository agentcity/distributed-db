// Package model holds types shared by the Raft core, the state machine and
// the transport adapters. It must not import any other project package.
package model

// CommandType is the kind of a replicated command.
type CommandType string

const (
	// CommandNoop is appended by a new leader to commit entries from previous
	// terms and to establish a read barrier.
	CommandNoop CommandType = "noop"
	// CommandPut stores Value under Key.
	CommandPut CommandType = "put"
	// CommandDelete removes Key.
	CommandDelete CommandType = "delete"

	// V2 transactional commands. The payload lives in Entry.Value as JSON.
	CommandPrewrite CommandType = "prewrite"
	CommandCommit   CommandType = "commit"
	CommandRollback CommandType = "rollback"
	// CommandPrewriteCommit applies both phases in one Raft entry for a
	// single-range transaction (1PC).
	CommandPrewriteCommit CommandType = "prewrite_commit"
	// CommandTxnBatch applies several independent CommandPrewriteCommit
	// payloads in one Raft entry. Entry.Value is an opaque, state-machine
	// defined batch: {"id":<batchID>,"commands":[...]}. The per-command
	// outcome is not part of the replicated state; it is reported separately
	// to the proposer.
	CommandTxnBatch CommandType = "txn_batch"

	// Meta control-plane commands. Payload lives in Entry.Value as JSON.
	CommandMetaAllocTS  CommandType = "meta_alloc_ts"
	CommandMetaSetRange CommandType = "meta_set_ranges"

	// CommandSplitPrepare freezes a range and records the split key on every
	// replica before the coordinator creates the new group.
	CommandSplitPrepare CommandType = "split_prepare"
	// CommandMergePrepare freezes two adjacent ranges before data movement.
	CommandMergePrepare CommandType = "merge_prepare"

	// Raft membership changes. These entries are consumed by the Raft node
	// itself, not by the application state machine. Entry.Key carries the
	// peer id; Entry.Value is empty.
	//
	// Membership is changed one server at a time: AddLearner adds a
	// non-voting replica, PromoteLearner turns that learner into a voter and
	// RemoveVoter removes a voter (or learner). There is deliberately no joint
	// consensus, so two changes must never overlap.
	CommandAddLearner     CommandType = "config_add_learner"
	CommandPromoteLearner CommandType = "config_promote_learner"
	CommandRemoveVoter    CommandType = "config_remove_voter"
)

// Entry is a single replicated log entry. Index is 1-based. Value is nil for
// delete and noop commands.
type Entry struct {
	Term  uint64      `json:"term"`
	Index uint64      `json:"index"`
	Type  CommandType `json:"type"`
	Key   string      `json:"key,omitempty"`
	Value []byte      `json:"value,omitempty"`
}
