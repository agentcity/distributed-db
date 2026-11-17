package pitr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"distributed-db/internal/model"
)

// sliceEntrySource is the test feed: a committed, in-memory log per range.
// It deliberately implements only the narrow EntrySource contract.
type sliceEntrySource struct {
	entries map[uint64][]model.Entry
	err     error
	calls   int
}

func (s *sliceEntrySource) Committed(ctx context.Context, rangeID, fromIndex uint64, limit int) ([]model.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	var out []model.Entry
	for _, entry := range s.entries[rangeID] {
		if entry.Index < fromIndex {
			continue
		}
		out = append(out, entry)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func jsonEntry(index, term uint64, typ model.CommandType, payload string) model.Entry {
	return model.Entry{
		Term:  term,
		Index: index,
		Type:  typ,
		Key:   fmt.Sprintf("k%d", index),
		Value: []byte(payload),
	}
}

func putEntry(index, term uint64, key string, value []byte) model.Entry {
	return model.Entry{Term: term, Index: index, Type: model.CommandPut, Key: key, Value: value}
}

func TestArchiveAdapterFeedsCommittedEntries(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)

	source := &sliceEntrySource{entries: map[uint64][]model.Entry{
		1: {
			putEntry(1, 1, "a", []byte("old")),
			jsonEntry(2, 1, model.CommandPrewriteCommit, `{"start_ts":100,"commit_ts":120}`),
			jsonEntry(3, 2, model.CommandTxnBatch, `{"id":7,"commands":[{"commit_ts":130},{"commit_ts":150}]}`),
			jsonEntry(4, 2, model.CommandNoop, ``),
		},
	}}
	adapter := &ArchiveAdapter{Writer: w, Source: source, BatchSize: 2}
	report, err := adapter.FeedRange(context.Background(), 1, 0)
	if err != nil {
		t.Fatalf("FeedRange: %v", err)
	}
	if report.Entries != 4 || report.FirstIndex != 1 || report.LastIndex != 4 || report.NextIndex != 5 {
		t.Fatalf("feed report = %+v", report)
	}
	if source.calls != 3 {
		t.Fatalf("source calls = %d, want 3 (two full batches plus the empty tail)", source.calls)
	}

	empty, err := adapter.FeedRange(context.Background(), 1, report.NextIndex)
	if err != nil {
		t.Fatalf("empty FeedRange: %v", err)
	}
	if empty.Entries != 0 || empty.NextIndex != 5 {
		t.Fatalf("empty feed report = %+v", empty)
	}

	// Push-style entry point appends one already-read batch. It leaves records
	// buffered, so the caller still seals explicitly.
	if err := adapter.AppendEntries(context.Background(), 1, []model.Entry{
		putEntry(5, 2, "b", []byte("new")),
	}); err != nil {
		t.Fatalf("AppendEntries: %v", err)
	}
	if err := w.Seal(context.Background()); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := replay(t, mustReader(t, dir), 1, Target{})
	wantTSO := []uint64{0, 120, 150, 0, 0}
	if len(got) != len(wantTSO) {
		t.Fatalf("replayed %d records, want %d: %+v", len(got), len(wantTSO), got)
	}
	for i, rec := range got {
		if rec.TSO != wantTSO[i] {
			t.Fatalf("record %d tso = %d, want %d", i, rec.TSO, wantTSO[i])
		}
		if rec.Entry.Index != uint64(i+1) {
			t.Fatalf("record %d index = %d", i, rec.Entry.Index)
		}
	}
	if got[2].Entry.Type != model.CommandTxnBatch || got[2].TSO != 150 {
		t.Fatalf("txn_batch record = %+v, want max command commit_ts 150", got[2])
	}

	m := mustReader(t, dir).Manifest()
	if len(m.Segments) != 1 || m.Segments[0].MinTSO != 0 || m.Segments[0].MaxTSO != 150 {
		t.Fatalf("segment bounds = %+v", m.Segments)
	}
}

func TestArchiveAdapterRejectsGapThenResumes(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)

	source := &sliceEntrySource{entries: map[uint64][]model.Entry{
		1: {
			putEntry(1, 1, "a", []byte("one")),
			putEntry(3, 1, "c", []byte("three")),
		},
	}}
	adapter := &ArchiveAdapter{Writer: w, Source: source, BatchSize: 1}
	report, err := adapter.FeedRange(context.Background(), 1, 0)
	if !errors.Is(err, ErrGap) || !errors.Is(err, ErrIndex) {
		t.Fatalf("gap error = %v, want ErrGap/ErrIndex", err)
	}
	if report.LastIndex != 1 || report.NextIndex != 2 || report.Entries != 1 {
		t.Fatalf("partial report = %+v, want index 1 confirmed", report)
	}

	source.entries[1] = []model.Entry{
		putEntry(1, 1, "a", []byte("one")),
		putEntry(2, 1, "b", []byte("two")),
		putEntry(3, 1, "c", []byte("three")),
	}
	resumed, err := adapter.FeedRange(context.Background(), 1, report.NextIndex)
	if err != nil {
		t.Fatalf("resumed FeedRange: %v", err)
	}
	if resumed.Entries != 2 || resumed.LastIndex != 3 {
		t.Fatalf("resumed report = %+v", resumed)
	}
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := replay(t, mustReader(t, dir), 1, Target{}); len(got) != 3 {
		t.Fatalf("replayed = %+v", got)
	}
}

func TestArchiveAdapterRejectsBadBatches(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)

	adapter := &ArchiveAdapter{Writer: w, Source: &sliceEntrySource{}}
	if _, err := adapter.FeedRange(context.Background(), 0, 1); !errors.Is(err, ErrRecord) {
		t.Fatalf("zero range error = %v", err)
	}
	adapter.Source = nil
	if _, err := adapter.FeedRange(context.Background(), 1, 1); !errors.Is(err, ErrRecord) {
		t.Fatalf("nil source error = %v", err)
	}
	adapter.Source = &sliceEntrySource{}
	adapter.BatchSize = maxFeedBatch + 1
	if _, err := adapter.FeedRange(context.Background(), 1, 1); !errors.Is(err, ErrRecord) {
		t.Fatalf("batch size error = %v", err)
	}
	adapter.BatchSize = 0
	if err := adapter.AppendEntries(context.Background(), 1, nil); err != nil {
		t.Fatalf("empty AppendEntries: %v", err)
	}
}

func TestCommitTSFromEntry(t *testing.T) {
	cases := []struct {
		name    string
		entry   model.Entry
		want    uint64
		wantErr bool
	}{
		{name: "plain put", entry: putEntry(1, 1, "a", []byte{0xff, 0x00}), want: 0},
		{name: "noop", entry: model.Entry{Index: 2, Term: 1, Type: model.CommandNoop}, want: 0},
		{name: "prewrite", entry: jsonEntry(3, 1, model.CommandPrewrite, `{"start_ts":10}`), want: 0},
		{name: "commit", entry: jsonEntry(4, 1, model.CommandCommit, `{"start_ts":10,"commit_ts":11}`), want: 11},
		{name: "prewrite commit", entry: jsonEntry(5, 1, model.CommandPrewriteCommit, `{"start_ts":10,"commit_ts":12}`), want: 12},
		{name: "txn batch max", entry: jsonEntry(6, 1, model.CommandTxnBatch, `{"id":1,"commands":[{"commit_ts":20},{"commit_ts":30},{"commit_ts":25}]}`), want: 30},
		{name: "txn batch missing", entry: jsonEntry(7, 1, model.CommandTxnBatch, `{"id":1,"commands":[{}]}`), wantErr: true},
		{name: "commit empty", entry: jsonEntry(8, 1, model.CommandCommit, ``), wantErr: true},
		{name: "commit malformed", entry: jsonEntry(9, 1, model.CommandCommit, `{`), wantErr: true},
		{name: "unknown type", entry: model.Entry{Index: 10, Term: 1, Type: "custom", Value: []byte(`{`)}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CommitTSFromEntry(tc.entry)
			if tc.wantErr {
				if !errors.Is(err, ErrRecord) {
					t.Fatalf("error = %v, want ErrRecord", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CommitTSFromEntry: %v", err)
			}
			if got != tc.want {
				t.Fatalf("tso = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRestoreToTSIncludesExpectedRecords(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)

	source := &sliceEntrySource{entries: map[uint64][]model.Entry{
		1: {
			jsonEntry(1, 1, model.CommandPrewriteCommit, `{"start_ts":100,"commit_ts":120}`),
			jsonEntry(2, 1, model.CommandPrewriteCommit, `{"start_ts":100,"commit_ts":160}`),
		},
		2: {
			jsonEntry(1, 1, model.CommandPrewriteCommit, `{"start_ts":100,"commit_ts":110}`),
			jsonEntry(2, 1, model.CommandPrewriteCommit, `{"start_ts":100,"commit_ts":150}`),
			jsonEntry(3, 1, model.CommandPrewriteCommit, `{"start_ts":100,"commit_ts":200}`),
		},
	}}
	adapter := &ArchiveAdapter{Writer: w, Source: source}
	for _, rangeID := range []uint64{1, 2} {
		if _, err := adapter.FeedRange(context.Background(), rangeID, 0); err != nil {
			t.Fatalf("FeedRange(%d): %v", rangeID, err)
		}
	}
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	base := testBase(100,
		baseRange(2, []byte("m"), nil),
		baseRange(1, nil, []byte("m")),
	)
	planner, err := NewPlanner(mustReader(t, dir))
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}

	var got []PlannedRecord
	sink := RestoreSinkFunc(func(ctx context.Context, rec PlannedRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		got = append(got, rec)
		return nil
	})
	res, err := planner.RestoreToTS(context.Background(), PlanRequest{Base: base, TargetTSO: 160}, sink)
	if err != nil {
		t.Fatalf("RestoreToTS: %v", err)
	}
	if res.Applied != 4 || res.BaseTSO != 100 || res.TargetTSO != 160 {
		t.Fatalf("restore result = %+v", res)
	}
	if !reflect.DeepEqual(got, res.Plan.Records) {
		t.Fatalf("sink records differ from plan:\n%+v\n%+v", got, res.Plan.Records)
	}
	want := []struct {
		rangeID uint64
		index   uint64
		tso     uint64
	}{
		{1, 1, 120},
		{1, 2, 160},
		{2, 1, 110},
		{2, 2, 150},
	}
	for i, w := range want {
		r := got[i]
		if r.Seq != uint64(i+1) || r.RangeID != w.rangeID || r.Entry.Index != w.index || r.TSO != w.tso {
			t.Fatalf("record %d = %+v, want range %d index %d tso %d", i, r, w.rangeID, w.index, w.tso)
		}
	}

	// The same immutable inputs and sink ordering are deterministic.
	again, err := planner.RestoreToTS(context.Background(), PlanRequest{Base: base, TargetTSO: 160}, sink)
	if err != nil {
		t.Fatalf("second RestoreToTS: %v", err)
	}
	if again.Plan.Hash != res.Plan.Hash || !reflect.DeepEqual(again.Plan.Records, res.Plan.Records) {
		t.Fatalf("restore plan is not deterministic")
	}

	// A base-only target replays nothing.
	calls := 0
	baseOnly, err := planner.RestoreToTS(context.Background(), PlanRequest{Base: base, TargetTSO: 100},
		RestoreSinkFunc(func(context.Context, PlannedRecord) error { calls++; return nil }))
	if err != nil {
		t.Fatalf("base-only RestoreToTS: %v", err)
	}
	if baseOnly.Applied != 0 || calls != 0 {
		t.Fatalf("base-only applied = %d, sink calls = %d", baseOnly.Applied, calls)
	}

	// A target above the newest observed archive TSO fails closed before the
	// sink sees a record.
	before := len(got)
	if _, err := planner.RestoreToTS(context.Background(), PlanRequest{Base: base, TargetTSO: 250}, sink); !errors.Is(err, ErrArchiveIncomplete) {
		t.Fatalf("uncovered target error = %v, want ErrArchiveIncomplete", err)
	}
	if len(got) != before {
		t.Fatalf("uncovered target reached the sink: %d new records", len(got)-before)
	}
	allowed, err := planner.RestoreToTS(context.Background(),
		PlanRequest{Base: base, TargetTSO: 250, AllowUnprovenTarget: true}, sink)
	if err != nil {
		t.Fatalf("AllowUnprovenTarget RestoreToTS: %v", err)
	}
	if allowed.Applied != 5 {
		t.Fatalf("unproven target applied = %d, want 5", allowed.Applied)
	}
}

func TestRestoreToTSHandlesNonMonotonicTSO(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 1,
		testRec(1, 1, 120, "a", []byte("a")),
		testRec(2, 1, 200, "b", []byte("b")),
		testRec(3, 1, 150, "c", []byte("c")))
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	planner, err := NewPlanner(mustReader(t, dir))
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	var got []PlannedRecord
	res, err := planner.RestoreToTS(context.Background(),
		PlanRequest{Base: testBase(100, baseRange(1, nil, nil)), TargetTSO: 170},
		RestoreSinkFunc(func(_ context.Context, rec PlannedRecord) error {
			got = append(got, rec)
			return nil
		}))
	if err != nil {
		t.Fatalf("RestoreToTS with non-monotonic TSO: %v", err)
	}
	if res.Applied != 2 || len(got) != 2 {
		t.Fatalf("applied = %d, records = %+v, want indices 1 and 3", res.Applied, got)
	}
	if got[0].Entry.Index != 1 || got[1].Entry.Index != 3 {
		t.Fatalf("records = %+v, want index 1 then 3", got)
	}
	if got[1].TSO != 150 {
		t.Fatalf("record 3 tso = %d, want 150", got[1].TSO)
	}
}

func TestRestoreToTSCoverageRequiresEveryNeededRange(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 2, testRec(1, 1, 200, "b", []byte("b")))
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	planner, _ := NewPlanner(mustReader(t, dir))
	base := testBase(100,
		baseRange(1, nil, []byte("m")),
		baseRange(2, []byte("m"), nil),
	)
	calls := 0
	_, err := planner.RestoreToTS(context.Background(), PlanRequest{Base: base, TargetTSO: 150},
		RestoreSinkFunc(func(context.Context, PlannedRecord) error { calls++; return nil }))
	if !errors.Is(err, ErrArchiveIncomplete) {
		t.Fatalf("error = %v, want ErrArchiveIncomplete for a needed range with no archive evidence", err)
	}
	if calls != 0 {
		t.Fatalf("sink calls = %d, want 0", calls)
	}
}

func TestRestoreToTSRejectsGapsBeforeSink(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 1, testRec(1, 1, 110, "a", nil))
	mustSeal(t, w)
	mustAppend(t, w, 1, testRec(2, 1, 130, "a", nil))
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r := mustReader(t, dir)
	segs := r.ListSegments()
	if len(segs) != 2 {
		t.Fatalf("segments = %d", len(segs))
	}
	if err := os.Remove(segmentFile(t, dir, segs[1])); err != nil {
		t.Fatalf("remove segment: %v", err)
	}
	planner, _ := NewPlanner(r)
	call := false
	_, err := planner.RestoreToTS(context.Background(),
		PlanRequest{Base: testBase(100, baseRange(1, nil, nil)), TargetTSO: 140},
		RestoreSinkFunc(func(context.Context, PlannedRecord) error { call = true; return nil }))
	if !errors.Is(err, ErrGap) || !errors.Is(err, ErrMissing) {
		t.Fatalf("gap error = %v, want ErrGap/ErrMissing", err)
	}
	if call {
		t.Fatal("restore sink was called for a plan with a gap")
	}
}

func TestRestoreToTSNilSinkAndSinkError(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 1, testRec(1, 1, 120, "a", nil), testRec(2, 1, 130, "b", nil))
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	planner, _ := NewPlanner(mustReader(t, dir))
	base := testBase(100, baseRange(1, nil, nil))
	if _, err := planner.RestoreToTS(context.Background(), PlanRequest{Base: base, TargetTSO: 130}, nil); err == nil {
		t.Fatal("RestoreToTS accepted a nil sink")
	}

	sentinel := errors.New("sink stopped")
	calls := 0
	res, err := planner.RestoreToTS(context.Background(), PlanRequest{Base: base, TargetTSO: 130},
		RestoreSinkFunc(func(context.Context, PlannedRecord) error {
			calls++
			if calls == 2 {
				return sentinel
			}
			return nil
		}))
	if !errors.Is(err, sentinel) {
		t.Fatalf("sink error = %v, want sentinel", err)
	}
	if res == nil || res.Applied != 1 || res.Plan == nil {
		t.Fatalf("partial result = %+v", res)
	}
}
