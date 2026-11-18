package pitr

import (
	"context"
	"errors"
	"testing"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

func newTestRaftLog(t *testing.T) *raft.FileStore {
	t.Helper()
	store, err := raft.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("FileStore.Close: %v", err)
		}
	})
	return store
}

func raftEntries(from, to uint64) []model.Entry {
	entries := make([]model.Entry, 0, to-from+1)
	for i := from; i <= to; i++ {
		entries = append(entries, model.Entry{
			Term:  1 + (i-1)/2,
			Index: i,
			Type:  model.CommandPut,
			Key:   "k",
			Value: []byte{byte(i)},
		})
	}
	return entries
}

func mustNewRaftSource(t *testing.T, log LocalRaftLog, rangeID, commit uint64) *RaftLogSource {
	t.Helper()
	src, err := NewRaftLogSource(RaftLogSourceConfig{
		Log:         log,
		RangeID:     rangeID,
		CommitIndex: func() uint64 { return commit },
	})
	if err != nil {
		t.Fatalf("NewRaftLogSource: %v", err)
	}
	return src
}

func TestRaftLogSourceReadsCommittedPrefix(t *testing.T) {
	ctx := context.Background()
	log := newTestRaftLog(t)
	if err := log.Append(raftEntries(1, 4)...); err != nil {
		t.Fatalf("Append: %v", err)
	}
	src := mustNewRaftSource(t, log, 7, 4)

	got, err := src.Committed(ctx, 7, 0, 2)
	if err != nil {
		t.Fatalf("Committed from first index: %v", err)
	}
	if len(got) != 2 || got[0].Index != 1 || got[1].Index != 2 {
		t.Fatalf("first batch = %+v, want indices 1,2", got)
	}
	got, err = src.Committed(ctx, 7, 3, 10)
	if err != nil {
		t.Fatalf("Committed from 3: %v", err)
	}
	if len(got) != 2 || got[0].Index != 3 || got[1].Index != 4 {
		t.Fatalf("second batch = %+v, want indices 3,4", got)
	}
	got, err = src.Committed(ctx, 7, 5, 10)
	if err != nil {
		t.Fatalf("Committed at tail: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("tail batch = %+v, want empty", got)
	}

	// Only a committed prefix may be archived: the durable store still holds
	// indices 3 and 4, but the raft commit index covers only 1 and 2.
	limited := mustNewRaftSource(t, log, 7, 2)
	got, err = limited.Committed(ctx, 7, 0, 10)
	if err != nil {
		t.Fatalf("Committed with commit=2: %v", err)
	}
	if len(got) != 2 || got[0].Index != 1 || got[1].Index != 2 {
		t.Fatalf("committed batch = %+v, want only 1,2", got)
	}
	got, err = limited.Committed(ctx, 7, 3, 10)
	if err != nil {
		t.Fatalf("Committed beyond commit: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("uncommitted batch = %+v, want empty", got)
	}

	// The adapter is read-only: a Load after the calls still sees the log.
	state, err := log.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Entries) != 4 {
		t.Fatalf("log mutated by source: %d entries", len(state.Entries))
	}
}

func TestRaftLogSourceRejectsMismatchedRangeAndBadConfig(t *testing.T) {
	log := newTestRaftLog(t)
	if err := log.Append(raftEntries(1, 1)...); err != nil {
		t.Fatalf("Append: %v", err)
	}
	src := mustNewRaftSource(t, log, 7, 1)
	if _, err := src.Committed(context.Background(), 8, 0, 10); !errors.Is(err, ErrRangeMismatch) {
		t.Fatalf("range mismatch error = %v, want ErrRangeMismatch", err)
	}
	if _, err := src.Committed(context.Background(), 7, 0, 0); !errors.Is(err, ErrSource) {
		t.Fatalf("zero limit error = %v, want ErrSource", err)
	}
	if _, err := src.Committed(nil, 7, 0, 10); err != nil { //nolint:staticcheck // nil context is normalized
		t.Fatalf("Committed with nil context: %v", err)
	}

	cases := []struct {
		name string
		cfg  RaftLogSourceConfig
	}{
		{name: "nil log", cfg: RaftLogSourceConfig{RangeID: 1, CommitIndex: func() uint64 { return 0 }}},
		{name: "zero range", cfg: RaftLogSourceConfig{Log: log, CommitIndex: func() uint64 { return 0 }}},
		{name: "nil commit", cfg: RaftLogSourceConfig{Log: log, RangeID: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRaftLogSource(tc.cfg); !errors.Is(err, ErrSource) {
				t.Fatalf("NewRaftLogSource error = %v, want ErrSource", err)
			}
		})
	}
}

func TestRaftLogSourceCompactedPrefixGap(t *testing.T) {
	ctx := context.Background()
	log := newTestRaftLog(t)
	if err := log.Append(raftEntries(1, 4)...); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := log.SaveSnapshot(raft.Snapshot{Index: 2, Term: 1}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := log.TruncatePrefix(2); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	src := mustNewRaftSource(t, log, 7, 4)

	_, err := src.Committed(ctx, 7, 1, 10)
	var gap *GapError
	if !errors.As(err, &gap) {
		t.Fatalf("compacted read error = %v, want *GapError", err)
	}
	if !errors.Is(err, ErrGap) || !errors.Is(err, ErrCompactedPrefix) {
		t.Fatalf("error = %v, want ErrGap and ErrCompactedPrefix", err)
	}
	if gap.FirstAvailable != 3 || gap.Requested != 1 || gap.RangeID != 7 || !gap.Compacted {
		t.Fatalf("gap = %+v, want range 7 requested 1 first available 3 compacted", gap)
	}

	// fromIndex 0 means "start at the first retained entry", not a gap.
	got, err := src.Committed(ctx, 7, 0, 10)
	if err != nil {
		t.Fatalf("Committed from 0 after compaction: %v", err)
	}
	if len(got) != 2 || got[0].Index != 3 || got[1].Index != 4 {
		t.Fatalf("post-compaction batch = %+v, want 3,4", got)
	}
	// Exactly the first available index is not a gap.
	got, err = src.Committed(ctx, 7, 3, 1)
	if err != nil {
		t.Fatalf("Committed at first available: %v", err)
	}
	if len(got) != 1 || got[0].Index != 3 {
		t.Fatalf("first-available batch = %+v, want index 3", got)
	}

	// Compact the entire retained log. Snapshot metadata must still report the
	// first available index instead of silently returning an empty tail.
	if err := log.SaveSnapshot(raft.Snapshot{Index: 4, Term: 1}); err != nil {
		t.Fatalf("SaveSnapshot 4: %v", err)
	}
	if err := log.TruncatePrefix(4); err != nil {
		t.Fatalf("TruncatePrefix 4: %v", err)
	}
	_, err = src.Committed(ctx, 7, 1, 10)
	if !errors.As(err, &gap) {
		t.Fatalf("fully compacted read error = %v, want *GapError", err)
	}
	if gap.FirstAvailable != 5 || !gap.Compacted {
		t.Fatalf("fully compacted gap = %+v, want first available 5", gap)
	}
	got, err = src.Committed(ctx, 7, 5, 10)
	if err != nil {
		t.Fatalf("Committed at empty tail: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty tail batch = %+v", got)
	}
}

func TestRaftLogSourceRejectsRegressingTerm(t *testing.T) {
	log := newTestRaftLog(t)
	if err := log.Append(
		model.Entry{Term: 5, Index: 1, Type: model.CommandPut, Key: "a"},
		model.Entry{Term: 4, Index: 2, Type: model.CommandPut, Key: "b"},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	src := mustNewRaftSource(t, log, 7, 2)
	if _, err := src.Committed(context.Background(), 7, 0, 10); !errors.Is(err, ErrRecord) {
		t.Fatalf("term regression error = %v, want ErrRecord", err)
	}
}

type emptyLocalLog struct{}

func (emptyLocalLog) Load() (raft.PersistentState, error) { return raft.PersistentState{}, nil }

func TestRaftLogSourceEmptyLogWithCommitFrontier(t *testing.T) {
	src := mustNewRaftSource(t, emptyLocalLog{}, 1, 4)
	got, err := src.Committed(context.Background(), 1, 1, 10)
	if err != nil {
		t.Fatalf("Committed on empty log: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty log batch = %+v", got)
	}
}

func TestRaftLogSourceFeedsArchiveAdapter(t *testing.T) {
	ctx := context.Background()
	log := newTestRaftLog(t)
	if err := log.Append(raftEntries(1, 3)...); err != nil {
		t.Fatalf("Append: %v", err)
	}
	src := mustNewRaftSource(t, log, 7, 3)

	dir := t.TempDir()
	w := newArchiveWriter(t, dir, newTestClock(), 1<<20)
	adapter := &ArchiveAdapter{Writer: w, Source: src, BatchSize: 2}
	report, err := adapter.FeedRange(ctx, 7, 0)
	if err != nil {
		t.Fatalf("FeedRange: %v", err)
	}
	if report.Entries != 3 || report.FirstIndex != 1 || report.LastIndex != 3 || report.NextIndex != 4 {
		t.Fatalf("feed report = %+v", report)
	}
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := replay(t, mustReader(t, dir), 7, Target{})
	if len(got) != 3 || got[0].Entry.Index != 1 || got[2].Entry.Index != 3 {
		t.Fatalf("archived records = %+v", got)
	}
}
