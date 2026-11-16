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

	// Meta control-plane commands. Payload lives in Entry.Value as JSON.
	CommandMetaAllocTS  CommandType = "meta_alloc_ts"
	CommandMetaSetRange CommandType = "meta_set_ranges"
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
