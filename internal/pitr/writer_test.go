package pitr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/model"
)

func TestAppendReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)

	recs := []LogRecord{
		testRec(1, 1, 100, "a", []byte("one")),
		testRec(2, 1, 110, "b", []byte("two")),
		testRec(3, 2, 120, "c", []byte("three")),
	}
	mustAppend(t, w, 7, recs...)
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r := mustReader(t, dir)
	m := r.Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest Validate: %v", err)
	}
	if len(m.Segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(m.Segments))
	}
	seg := m.Segments[0]
	if seg.RangeID != 7 || seg.StartIndex != 1 || seg.EndIndex != 3 || seg.EntryCount != 3 {
		t.Fatalf("unexpected segment descriptor: %+v", seg)
	}
	if seg.StartTerm != 1 || seg.EndTerm != 2 || seg.MinTSO != 100 || seg.MaxTSO != 120 {
		t.Fatalf("unexpected segment bounds: %+v", seg)
	}
	if seg.WriterEpoch != 7 {
		t.Fatalf("writer epoch = %d, want 7", seg.WriterEpoch)
	}
	if seg.PrevSHA256 != "" {
		t.Fatalf("first segment prev hash = %q", seg.PrevSHA256)
	}
	if !strings.HasSuffix(seg.Path, ".seg") {
		t.Fatalf("segment path %q has no .seg suffix", seg.Path)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(seg.Path))); err != nil {
		t.Fatalf("segment file missing: %v", err)
	}

	got, err := r.ReadSegment(context.Background(), seg)
	if err != nil {
		t.Fatalf("ReadSegment: %v", err)
	}
	if !reflect.DeepEqual(got.Records, recs) {
		t.Fatalf("ReadSegment records = %+v, want %+v", got.Records, recs)
	}
	if got.Header.RangeID != 7 || got.Header.ClusterID != "cluster-a" {
		t.Fatalf("header = %+v", got.Header)
	}

	if got := replay(t, r, 7, Target{TSO: 115}); len(got) != 2 || got[1].TSO != 110 {
		t.Fatalf("target TSO 115 returned %+v", got)
	}
	if got := replay(t, r, 7, Target{Index: 2}); len(got) != 2 || got[1].Entry.Index != 2 {
		t.Fatalf("target index 2 returned %+v", got)
	}
	if got := replay(t, r, 7, Target{TSO: 115, Index: 2}); len(got) != 2 {
		t.Fatalf("combined target returned %+v", got)
	}
	if got := replay(t, r, 8, Target{}); len(got) != 0 {
		t.Fatalf("unknown range returned %+v", got)
	}

	report, err := r.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.Segments != 1 || report.Records != 3 || report.PayloadBytes == 0 ||
		report.Ranges != 1 || report.MinTSO != 100 || report.MaxTSO != 120 {
		t.Fatalf("unexpected verify report: %+v", report)
	}

	// Reopen and append after the sealed segment: the new segment must chain.
	w2, err := NewWriter(WriterConfig{
		Dir: dir, ClusterID: "cluster-a", SegmentMaxBytes: 1 << 20,
		SegmentMaxAge: time.Hour, Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("reopen writer: %v", err)
	}
	defer func() { _ = w2.Close() }()
	mustAppend(t, w2, 7, testRec(4, 2, 130, "d", []byte("four")))
	mustSeal(t, w2)
	if err := w2.Close(); err != nil {
		t.Fatalf("close reopened writer: %v", err)
	}

	r2 := mustReader(t, dir)
	segs := r2.ListSegments()
	if len(segs) != 2 {
		t.Fatalf("segments after reopen = %d, want 2", len(segs))
	}
	if segs[1].PrevSHA256 != segs[0].SHA256 {
		t.Fatalf("segment chain broken: prev %q, want %q", segs[1].PrevSHA256, segs[0].SHA256)
	}
	all := replay(t, r2, 7, Target{})
	if len(all) != 4 || all[3].Entry.Index != 4 {
		t.Fatalf("replayed records = %+v", all)
	}
	if _, err := r2.Verify(context.Background()); err != nil {
		t.Fatalf("Verify after reopen: %v", err)
	}
}

func TestCrashBetweenTempAndRename(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 1, testRec(1, 1, 100, "a", []byte("one")))
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r := mustReader(t, dir)
	first := r.ListSegments()[0]

	// Build a valid next segment but leave it as a temp file, exactly what a
	// process crash after fsync and before rename leaves behind.
	next := testRec(2, 1, 110, "b", []byte("two"))
	payload, err := encodeLogRecordPayload(next)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	h := SegmentHeader{
		ClusterID: "cluster-a", RangeID: 1, Incarnation: 1,
		StartIndex: 2, EndIndex: 2, StartTerm: 1, EndTerm: 1, EntryCount: 1,
		MinTSO: 110, MaxTSO: 110, WriterEpoch: 7, PayloadBytes: uint64(len(payload)),
		SealedAt: clock.Now(), PrevSHA256: first.SHA256,
	}
	data, sha, err := encodeSegment(h, []LogRecord{next})
	if err != nil {
		t.Fatalf("encode segment: %v", err)
	}
	rangeDir := filepath.Join(dir, SegmentsDir, rangeDirName(1))
	tmpName := filepath.Join(rangeDir, ".tmp-crash")
	writeRawFile(t, tmpName, data)
	// Also simulate the narrower crash after rename and before manifest
	// publication: a valid but unreferenced segment file.
	orphan := filepath.Join(dir, filepath.FromSlash(segmentRelPath(1, 2, 2, sha)))
	writeRawFile(t, orphan, data)

	w2, err := NewWriter(WriterConfig{
		Dir: dir, ClusterID: "cluster-a", SegmentMaxBytes: 1 << 20,
		SegmentMaxAge: time.Hour, Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("NewWriter after simulated crash: %v", err)
	}
	defer func() { _ = w2.Close() }()
	mustNotExist(t, tmpName)
	mustNotExist(t, orphan)
	if got := w2.Manifest().Segments; len(got) != 1 {
		t.Fatalf("manifest segments after crash recovery = %d, want 1", len(got))
	}

	// A graceful failure at the rename point must not publish a manifest entry,
	// and a retry must succeed.
	mustAppend(t, w2, 1, next)
	sentinel := errors.New("simulated crash before rename")
	w2.hooks.beforeRename = func(_, _ string) error { return sentinel }
	if err := w2.Seal(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Seal error = %v, want sentinel", err)
	}
	if got := w2.Manifest().Segments; len(got) != 1 {
		t.Fatalf("failed seal changed manifest: %d segments", len(got))
	}
	if err := w2.Seal(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("second Seal error = %v, want sentinel", err)
	}
	w2.hooks.beforeRename = nil
	mustSeal(t, w2)
	if got := w2.Manifest().Segments; len(got) != 2 {
		t.Fatalf("retry did not publish segment: %d", len(got))
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := replay(t, mustReader(t, dir), 1, Target{}); len(got) != 2 {
		t.Fatalf("replayed after crash test = %+v", got)
	}
}

func TestAppendRejectsNonContiguousOrInvalid(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)

	if err := w.Append(context.Background(), 1, []LogRecord{testRec(5, 1, 10, "a", nil)}); err != nil {
		t.Fatalf("first append at arbitrary index: %v", err)
	}
	err := w.Append(context.Background(), 1, []LogRecord{testRec(7, 1, 20, "b", nil)})
	if !errors.Is(err, ErrGap) || !errors.Is(err, ErrIndex) {
		t.Fatalf("gap error = %v, want ErrGap/ErrIndex", err)
	}
	err = w.Append(context.Background(), 1, []LogRecord{testRec(6, 1, 20, "b", nil), testRec(8, 1, 30, "c", nil)})
	if !errors.Is(err, ErrGap) {
		t.Fatalf("batch gap error = %v, want ErrGap", err)
	}
	// The rejected batch must not have buffered its first record.
	mustAppend(t, w, 1, testRec(6, 1, 20, "b", nil))
	mustSeal(t, w)
	if got := replay(t, mustReader(t, dir), 1, Target{}); len(got) != 2 {
		t.Fatalf("records after rejected batch = %+v", got)
	}

	bad := testRec(7, 0, 30, "c", nil)
	if err := w.Append(context.Background(), 1, []LogRecord{bad}); !errors.Is(err, ErrRecord) {
		t.Fatalf("zero term error = %v, want ErrRecord", err)
	}
	bad = testRec(7, 1, 30, "c", nil)
	bad.Entry.Type = ""
	if err := w.Append(context.Background(), 1, []LogRecord{bad}); !errors.Is(err, ErrRecord) {
		t.Fatalf("empty type error = %v, want ErrRecord", err)
	}
	if err := w.Append(context.Background(), 0, []LogRecord{testRec(7, 1, 30, "c", nil)}); !errors.Is(err, ErrRecord) {
		t.Fatalf("zero range error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Append(context.Background(), 1, []LogRecord{testRec(7, 1, 30, "c", nil)}); !errors.Is(err, ErrClosed) {
		t.Fatalf("append after close error = %v, want ErrClosed", err)
	}
}

func TestTermRegressionRejected(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 1, testRec(1, 3, 10, "a", nil))
	err := w.Append(context.Background(), 1, []LogRecord{testRec(2, 2, 20, "b", nil)})
	if !errors.Is(err, ErrRecord) {
		t.Fatalf("term regression error = %v, want ErrRecord", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAppendAcceptsMetadataEntry(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	rec := LogRecord{TSO: 0, Entry: model.Entry{
		Term: 1, Index: 1, Type: model.CommandNoop,
	}}
	mustAppend(t, w, 1, rec)
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r := mustReader(t, dir)
	got := replay(t, r, 1, Target{})
	if len(got) != 1 || got[0].TSO != 0 || got[0].Entry.Type != model.CommandNoop {
		t.Fatalf("metadata replay = %+v", got)
	}
}

func TestWriterSealsBySizeAndAge(t *testing.T) {
	t.Run("size", func(t *testing.T) {
		dir := t.TempDir()
		clock := newTestClock()
		w := newArchiveWriter(t, dir, clock, 1)
		mustAppend(t, w, 1,
			testRec(1, 1, 10, "a", nil),
			testRec(2, 1, 20, "b", nil),
			testRec(3, 1, 30, "c", nil))
		if got := len(w.Manifest().Segments); got != 3 {
			t.Fatalf("segments after size-triggered seals = %d, want 3", got)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := replay(t, mustReader(t, dir), 1, Target{}); len(got) != 3 {
			t.Fatalf("size-sealed replay = %+v", got)
		}
	})

	t.Run("age", func(t *testing.T) {
		dir := t.TempDir()
		clock := newTestClock()
		w := newArchiveWriter(t, dir, clock, 1<<20)
		mustAppend(t, w, 1, testRec(1, 1, 10, "a", nil))
		clock.Advance(2 * time.Hour)
		mustAppend(t, w, 1, testRec(2, 1, 20, "b", nil))
		if got := len(w.Manifest().Segments); got != 1 {
			t.Fatalf("segments after age-triggered seal = %d, want 1", got)
		}
		mustSeal(t, w)
		if got := len(w.Manifest().Segments); got != 2 {
			t.Fatalf("segments after explicit seal = %d, want 2", got)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := replay(t, mustReader(t, dir), 1, Target{}); len(got) != 2 {
			t.Fatalf("age-sealed replay = %+v", got)
		}
	})
}
