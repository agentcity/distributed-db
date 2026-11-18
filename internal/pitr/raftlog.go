package pitr

import (
	"context"
	"errors"
	"fmt"
	"io"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

// ErrSource is returned when a Raft log entry source is misconfigured or the
// local store cannot be read safely.
var ErrSource = errors.New("pitr: invalid raft log entry source")

// ErrCompactedPrefix is returned (wrapped in *GapError) when the requested
// starting index is below the first index still retained by the local Raft
// log, because a snapshot or truncation removed the prefix.
var ErrCompactedPrefix = errors.New("pitr: raft log prefix was compacted")

// LocalRaftLog is the read-only slice of a Raft log store needed by this
// adapter. *raft.FileStore implements it through Load; the adapter never calls
// Append, Truncate, SaveSnapshot or any other mutating method.
type LocalRaftLog interface {
	Load() (raft.PersistentState, error)
}

// SnapshotMetaReader is the optional capability used to discover the
// compacted prefix when a store has no retained entries. *raft.FileStore
// implements it through OpenSnapshot; the returned reader is closed without
// being read.
type SnapshotMetaReader interface {
	OpenSnapshot() (raft.SnapshotMeta, io.ReadCloser, error)
}

// RaftLogSourceConfig configures a RaftLogSource.
type RaftLogSourceConfig struct {
	// Log is the local Raft log store. It is read-only for this adapter.
	Log LocalRaftLog
	// RangeID is the range owning this log store. Every Committed call must
	// name the same range id.
	RangeID uint64
	// CommitIndex returns the highest Raft index known to be committed, for
	// example func() uint64 { return node.Status().CommitIndex }. It is
	// required: Load exposes the durable log, including an uncommitted tail,
	// so without a commit frontier the adapter would archive entries that may
	// still be overwritten.
	CommitIndex func() uint64
}

// GapError is a typed index-gap error. Compacted is true when the gap is
// explained by a compacted log prefix, in which case FirstAvailable is the
// first index the local store still holds and errors.Is(err,
// ErrCompactedPrefix) also reports true. The error always matches ErrGap.
type GapError struct {
	RangeID        uint64
	Requested      uint64
	FirstAvailable uint64
	Compacted      bool
}

// Error implements error.
func (e *GapError) Error() string {
	if e == nil {
		return "pitr: nil gap error"
	}
	if e.Compacted {
		return fmt.Sprintf("pitr: range %d raft log prefix compacted: requested index %d, first available %d",
			e.RangeID, e.Requested, e.FirstAvailable)
	}
	return fmt.Sprintf("pitr: range %d raft log gap: requested index %d, next available %d",
		e.RangeID, e.Requested, e.FirstAvailable)
}

// Unwrap makes errors.Is(err, ErrGap) true for every GapError.
func (e *GapError) Unwrap() error { return ErrGap }

// Is makes errors.Is(err, ErrCompactedPrefix) true only for a compacted gap.
func (e *GapError) Is(target error) bool {
	return target == ErrCompactedPrefix && e != nil && e.Compacted
}

// RaftLogSource adapts a local Raft log store to EntrySource. Each Committed
// call takes one Load snapshot of the store, validates its index/term
// ordering, clamps it to the commit frontier and returns the contiguous batch
// in index order.
//
// The adapter is read-only and does not tail the log, follow leadership,
// detect log truncation between calls or publish progress. A caller that
// feeds a Writer must therefore retry from the last archived index and treat
// a compacted prefix as a hard gap: this MVP has no truncation gate and no
// way to recover an index that a snapshot removed before it was archived.
//
// A RaftLogSource is safe for concurrent use if the underlying store is.
type RaftLogSource struct {
	log         LocalRaftLog
	rangeID     uint64
	commitIndex func() uint64
}

// NewRaftLogSource validates cfg and returns a local log entry source.
func NewRaftLogSource(cfg RaftLogSourceConfig) (*RaftLogSource, error) {
	if cfg.Log == nil {
		return nil, fmt.Errorf("%w: nil local raft log", ErrSource)
	}
	if cfg.RangeID == 0 {
		return nil, fmt.Errorf("%w: range id is zero", ErrSource)
	}
	if cfg.CommitIndex == nil {
		return nil, fmt.Errorf("%w: no commit index provider", ErrSource)
	}
	return &RaftLogSource{log: cfg.Log, rangeID: cfg.RangeID, commitIndex: cfg.CommitIndex}, nil
}

// Committed implements EntrySource. fromIndex 0 starts at the first retained
// index. A fromIndex below the first retained index returns *GapError with
// Compacted set and FirstAvailable reported. Entries above the current commit
// index are never returned.
func (s *RaftLogSource) Committed(ctx context.Context, rangeID, fromIndex uint64, limit int) ([]model.Entry, error) {
	if s == nil || s.log == nil {
		return nil, fmt.Errorf("%w: nil raft log source", ErrSource)
	}
	if s.commitIndex == nil {
		return nil, fmt.Errorf("%w: no commit index provider", ErrSource)
	}
	if rangeID == 0 {
		return nil, fmt.Errorf("%w: requested range id is zero", ErrSource)
	}
	if rangeID != s.rangeID {
		return nil, fmt.Errorf("%w: raft log belongs to range %d, requested %d",
			ErrRangeMismatch, s.rangeID, rangeID)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit %d is not positive", ErrSource, limit)
	}
	if fromIndex == ^uint64(0) {
		return nil, fmt.Errorf("%w: from index overflows", ErrSource)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	state, err := s.log.Load()
	if err != nil {
		return nil, fmt.Errorf("pitr: load local raft log for range %d: %w", s.rangeID, err)
	}
	if err := validateLocalEntries(s.rangeID, state.Entries); err != nil {
		return nil, err
	}

	firstAvailable := uint64(1)
	if len(state.Entries) > 0 {
		firstAvailable = state.Entries[0].Index
	} else if meta, ok, err := s.snapshotIndex(); err != nil {
		return nil, err
	} else if ok {
		firstAvailable = meta + 1
	}

	if fromIndex == 0 {
		fromIndex = firstAvailable
	}
	if fromIndex < firstAvailable {
		return nil, &GapError{
			RangeID:        s.rangeID,
			Requested:      fromIndex,
			FirstAvailable: firstAvailable,
			Compacted:      true,
		}
	}

	committed := s.commitIndex()
	if committed == 0 || fromIndex > committed {
		return nil, nil
	}
	if len(state.Entries) == 0 {
		// The commit frontier claims progress but no entry is retained (for
		// example a store whose snapshot metadata is unavailable). There is
		// nothing safe to return.
		return nil, nil
	}
	last := state.Entries[len(state.Entries)-1].Index
	if committed > last {
		committed = last
	}
	if fromIndex > committed {
		return nil, nil
	}

	offset := fromIndex - firstAvailable
	if offset >= uint64(len(state.Entries)) {
		return nil, nil
	}
	start := int(offset)
	end := len(state.Entries)
	if limit < end-start {
		end = start + limit
	}
	out := make([]model.Entry, 0, end-start)
	for _, entry := range state.Entries[start:end] {
		if entry.Index > committed {
			break
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// snapshotIndex returns the snapshot index of the local store, if it exposes
// snapshot metadata. The boolean is false when the store has no such
// capability.
func (s *RaftLogSource) snapshotIndex() (uint64, bool, error) {
	reader, ok := s.log.(SnapshotMetaReader)
	if !ok {
		return 0, false, nil
	}
	meta, rc, err := reader.OpenSnapshot()
	if err != nil {
		return 0, false, fmt.Errorf("pitr: read local raft snapshot metadata for range %d: %w", s.rangeID, err)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if meta.Index == 0 {
		return 0, true, nil
	}
	return meta.Index, true, nil
}

// validateLocalEntries rejects a store view that is not a usable contiguous,
// non-decreasing-term raft log. It is a store integrity check, not a
// committed-prefix check.
func validateLocalEntries(rangeID uint64, entries []model.Entry) error {
	var prev model.Entry
	for i, entry := range entries {
		switch {
		case entry.Index == 0:
			return fmt.Errorf("%w: raft log entry %d has index zero", ErrRecord, i)
		case entry.Term == 0:
			return fmt.Errorf("%w: raft log entry at index %d has zero term", ErrRecord, entry.Index)
		case i > 0 && entry.Index != prev.Index+1:
			return &GapError{
				RangeID:        rangeID,
				Requested:      prev.Index + 1,
				FirstAvailable: entry.Index,
			}
		case i > 0 && entry.Term < prev.Term:
			return fmt.Errorf("%w: raft term regresses at index %d", ErrRecord, entry.Index)
		}
		prev = entry
	}
	return nil
}
