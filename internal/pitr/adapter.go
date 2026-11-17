package pitr

import (
	"context"
	"encoding/json"
	"fmt"

	"distributed-db/internal/model"
)

const (
	defaultFeedBatch = 256
	maxFeedBatch     = 4096
)

// EntrySource is the narrow read contract between a committed Raft log (or a
// WAL reader over one) and the archive adapter. It deliberately does not know
// about Raft, the log store type, leadership, or truncation: an integration
// wraps whatever it has and returns a committed, gap-free prefix.
//
// Implementations must return entries with non-decreasing Raft terms in
// ascending, contiguous index order, beginning at fromIndex. Returning fewer
// than limit entries is allowed; returning no entries means "nothing committed
// at or after fromIndex right now". The adapter calls Committed again only
// after the previous batch has been accepted by Writer.Append (which may still
// be buffered until a Seal), so an implementation may expose an unsealed log
// tail without the adapter reordering it.
type EntrySource interface {
	Committed(ctx context.Context, rangeID, fromIndex uint64, limit int) ([]model.Entry, error)
}

// CommitTSResolver maps one committed Raft entry to the commit TSO observed
// for it. The archive requires a TSO to bound restore replay; a resolver may
// return zero for entries that carry no commit timestamp (noop, membership,
// metadata, plain put/delete). It must not invent a timestamp: the planner
// treats TSO 0 as outside the base-to-target replay window.
type CommitTSResolver func(model.Entry) (uint64, error)

// ArchiveAdapter translates committed Raft/WAL entries into LogRecords and
// feeds them to a Writer. It is the concrete integration seam between a
// range's committed log and internal/pitr; it does not tail Raft, follow
// leadership changes, handle log truncation, or discover ranges.
//
// The Writer itself enforces per-range index contiguity, term monotonicity and
// the archive format. The adapter adds the source-side checks: batches from
// EntrySource are validated before any record is buffered, so a gap or a bad
// resolver result is rejected without a partial append.
type ArchiveAdapter struct {
	// Writer receives the translated records. It is required.
	Writer *Writer
	// Source supplies committed entries. It is required.
	Source EntrySource
	// CommitTS extracts the commit TSO of one entry. Nil selects
	// CommitTSFromEntry, which understands the v2 transactional JSON payloads
	// and returns zero for commands that carry no commit timestamp.
	CommitTS CommitTSResolver
	// BatchSize bounds one Source call and one Writer.Append. Zero selects
	// 256; values outside (0,4096] are rejected.
	BatchSize int
}

// FeedReport describes the confirmed progress of one FeedRange call. On error,
// FirstIndex/LastIndex/Entries cover the batches that were appended
// successfully; the failing batch is not included. If Writer.Append fails
// while sealing, it may already have buffered part of the batch, so a caller
// must reopen the archive to derive the durable resume point rather than
// assuming NextIndex is exact.
type FeedReport struct {
	RangeID    uint64
	Entries    int
	FirstIndex uint64
	LastIndex  uint64
	NextIndex  uint64
}

// FeedRange reads committed entries for rangeID from fromIndex and appends
// them until the source returns an empty batch. fromIndex 0 means "start at
// the first index the source returns"; otherwise the first entry must have
// exactly that index, and every later entry must be its contiguous successor.
// Gaps, duplicates and term regressions fail with ErrGap/ErrRecord.
//
// FeedRange does not seal. Call Writer.Seal after a catch-up feed, or rely on
// the writer's size/age trigger for a continuous feed.
func (a *ArchiveAdapter) FeedRange(ctx context.Context, rangeID, fromIndex uint64) (FeedReport, error) {
	report := FeedReport{RangeID: rangeID, NextIndex: fromIndex}
	if a == nil || a.Writer == nil {
		return report, fmt.Errorf("%w: archive adapter has no writer", ErrRecord)
	}
	if a.Source == nil {
		return report, fmt.Errorf("%w: archive adapter has no entry source", ErrRecord)
	}
	if rangeID == 0 {
		return report, fmt.Errorf("%w: archive adapter range id is zero", ErrRecord)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	batch := a.BatchSize
	if batch == 0 {
		batch = defaultFeedBatch
	}
	if batch < 1 || batch > maxFeedBatch {
		return report, fmt.Errorf("%w: adapter batch size %d is outside (0,%d]", ErrRecord, batch, maxFeedBatch)
	}

	next := fromIndex
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		entries, err := a.Source.Committed(ctx, rangeID, next, batch)
		if err != nil {
			return report, fmt.Errorf("pitr: read committed entries for range %d from %d: %w", rangeID, next, err)
		}
		if len(entries) == 0 {
			return report, nil
		}
		if len(entries) > batch {
			return report, fmt.Errorf("pitr: entry source returned %d entries for limit %d", len(entries), batch)
		}
		recs, err := a.records(next, entries)
		if err != nil {
			return report, fmt.Errorf("pitr: feed range %d: %w", rangeID, err)
		}
		if err := a.Writer.Append(ctx, rangeID, recs); err != nil {
			return report, fmt.Errorf("pitr: feed range %d: %w", rangeID, err)
		}
		if report.Entries == 0 {
			report.FirstIndex = entries[0].Index
		}
		report.Entries += len(entries)
		report.LastIndex = entries[len(entries)-1].Index
		next = report.LastIndex + 1
		report.NextIndex = next
	}
}

// AppendEntries translates and appends one already-read batch. It performs the
// same within-batch validation as FeedRange but does not consult the source and
// does not seal. It is the push-style variant for a state-machine apply hook;
// the Writer still enforces that the first index follows the last archived
// index of the range.
func (a *ArchiveAdapter) AppendEntries(ctx context.Context, rangeID uint64, entries []model.Entry) error {
	if a == nil || a.Writer == nil {
		return fmt.Errorf("%w: archive adapter has no writer", ErrRecord)
	}
	if rangeID == 0 {
		return fmt.Errorf("%w: archive adapter range id is zero", ErrRecord)
	}
	if len(entries) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	recs, err := a.records(0, entries)
	if err != nil {
		return err
	}
	return a.Writer.Append(ctx, rangeID, recs)
}

// records validates a source batch and resolves each entry's commit TSO before
// the Writer sees anything. expect 0 disables the first-index check (the
// Writer still validates continuation against its own state).
func (a *ArchiveAdapter) records(expect uint64, entries []model.Entry) ([]LogRecord, error) {
	resolve := a.CommitTS
	if resolve == nil {
		resolve = CommitTSFromEntry
	}
	recs := make([]LogRecord, len(entries))
	next := expect
	var lastTerm uint64
	for i, entry := range entries {
		switch {
		case i > 0 && entry.Index != entries[i-1].Index+1:
			return nil, fmt.Errorf("%w: %w: index %d after %d", ErrGap, ErrIndex, entry.Index, entries[i-1].Index)
		case next != 0 && entry.Index != next:
			if entry.Index < next {
				return nil, fmt.Errorf("%w: %w: index %d is below expected %d", ErrGap, ErrIndex, entry.Index, next)
			}
			return nil, fmt.Errorf("%w: %w: index %d, want %d", ErrGap, ErrIndex, entry.Index, next)
		case lastTerm != 0 && entry.Term < lastTerm:
			return nil, fmt.Errorf("%w: raft term regresses at index %d", ErrRecord, entry.Index)
		}
		tso, err := resolve(entry)
		if err != nil {
			return nil, fmt.Errorf("pitr: resolve commit tso at index %d: %w", entry.Index, err)
		}
		rec := LogRecord{TSO: tso, Entry: entry}
		if err := rec.Validate(); err != nil {
			return nil, err
		}
		recs[i] = rec
		next = entry.Index + 1
		lastTerm = entry.Term
	}
	return recs, nil
}

// CommitTSFromEntry is the default CommitTSResolver. It extracts commit_ts
// from the JSON payloads used by the v2 transactional commands
// (commit, prewrite_commit and txn_batch; a batch uses its largest command
// commit_ts). It returns zero for prewrite, noop, plain put/delete, meta,
// split/merge and membership entries, which carry no single commit timestamp.
//
// A commit, prewrite_commit or txn_batch entry whose payload has no positive
// commit_ts is rejected rather than archived at TSO 0: silently excluding it
// from a restore window would lose an acknowledged commit. A caller with a
// different payload schema supplies its own CommitTSResolver.
func CommitTSFromEntry(entry model.Entry) (uint64, error) {
	switch entry.Type {
	case model.CommandPrewrite, model.CommandRollback,
		model.CommandNoop, model.CommandPut, model.CommandDelete,
		model.CommandMetaAllocTS, model.CommandMetaSetRange,
		model.CommandSplitPrepare, model.CommandMergePrepare,
		model.CommandAddLearner, model.CommandPromoteLearner, model.CommandRemoveVoter:
		return 0, nil
	case model.CommandCommit, model.CommandPrewriteCommit, model.CommandTxnBatch:
	default:
		return 0, nil
	}
	if len(entry.Value) == 0 {
		return 0, fmt.Errorf("%w: %s entry at index %d has an empty payload", ErrRecord, entry.Type, entry.Index)
	}
	var payload struct {
		CommitTS uint64 `json:"commit_ts"`
		Commands []struct {
			CommitTS uint64 `json:"commit_ts"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(entry.Value, &payload); err != nil {
		return 0, fmt.Errorf("%w: decode %s commit_ts at index %d: %v", ErrRecord, entry.Type, entry.Index, err)
	}
	max := payload.CommitTS
	for _, cmd := range payload.Commands {
		if cmd.CommitTS > max {
			max = cmd.CommitTS
		}
	}
	if max == 0 {
		return 0, fmt.Errorf("%w: %s entry at index %d has no positive commit_ts", ErrRecord, entry.Type, entry.Index)
	}
	return max, nil
}
