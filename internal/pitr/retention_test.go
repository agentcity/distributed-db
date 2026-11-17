package pitr

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetentionByAgeProtectsNeededSegments(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	for i := uint64(1); i <= 4; i++ {
		mustAppend(t, w, 1, testRec(i, 1, 10*i, "k", []byte("v")))
		mustSeal(t, w)
		clock.Advance(time.Hour)
	}
	before := w.Manifest()
	if len(before.Segments) != 4 {
		t.Fatalf("segments = %d, want 4", len(before.Segments))
	}

	// Every segment is older than the age cutoff, but ProtectTSO=25 means the
	// first two (max TSO 10 and 20) can be reclaimed while 30 and 40 must stay.
	res, err := w.GC(context.Background(), RetentionPolicy{MaxAge: time.Hour, ProtectTSO: 25})
	if err != nil {
		t.Fatalf("GC age: %v", err)
	}
	if len(res.Deleted) != 2 || res.BytesFreed <= 0 {
		t.Fatalf("age GC deleted %d segments, bytes %d", len(res.Deleted), res.BytesFreed)
	}
	m := w.Manifest()
	if len(m.Segments) != 2 {
		t.Fatalf("segments after age GC = %d, want 2", len(m.Segments))
	}
	if m.Segments[0].MaxTSO != 30 || m.Segments[1].MaxTSO != 40 {
		t.Fatalf("protected segments = %+v", m.Segments)
	}
	trim, ok := m.TrimmedFor(1)
	if !ok || trim.ThroughIndex != 2 || trim.MaxTSO != 20 {
		t.Fatalf("trim = %+v, ok=%v", trim, ok)
	}

	r := mustReader(t, dir)
	got := replay(t, r, 1, Target{})
	if len(got) != 2 || got[0].TSO != 30 || got[1].TSO != 40 {
		t.Fatalf("replay after GC = %+v", got)
	}

	// Size pressure must not override ProtectTSO.
	res, err = w.GC(context.Background(), RetentionPolicy{MaxBytes: 1, ProtectTSO: 25})
	if err != nil {
		t.Fatalf("GC protected size: %v", err)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("size GC deleted protected segments: %+v", res.Deleted)
	}

	// Without protection, size pressure can trim the rest.
	res, err = w.GC(context.Background(), RetentionPolicy{MaxBytes: 1, ProtectTSO: 0})
	if err != nil {
		t.Fatalf("GC size: %v", err)
	}
	if len(res.Deleted) != 2 || res.RemainingSegments != 0 || res.RemainingBytes != 0 {
		t.Fatalf("unexpected size GC result: %+v", res)
	}
	m = w.Manifest()
	if len(m.Segments) != 0 {
		t.Fatalf("segments after full GC = %d", len(m.Segments))
	}
	trim, ok = m.TrimmedFor(1)
	if !ok || trim.ThroughIndex != 4 || trim.MaxTSO != 40 {
		t.Fatalf("final trim = %+v, ok=%v", trim, ok)
	}
	r = mustReader(t, dir)
	report, err := r.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify empty archive: %v", err)
	}
	if report.Segments != 0 || report.Records != 0 {
		t.Fatalf("empty archive report = %+v", report)
	}

	// A writer that lost all retained segments continues after the trim
	// frontier, with a fresh chain.
	mustAppend(t, w, 1, testRec(5, 1, 50, "k", []byte("v")))
	mustSeal(t, w)
	m = w.Manifest()
	if len(m.Segments) != 1 || m.Segments[0].StartIndex != 5 || m.Segments[0].PrevSHA256 != "" {
		t.Fatalf("post-GC segment = %+v", m.Segments)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r = mustReader(t, dir)
	if got := replay(t, r, 1, Target{}); len(got) != 1 || got[0].TSO != 50 {
		t.Fatalf("post-GC replay = %+v", got)
	}
}

func TestRetentionBySize(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	for i := uint64(1); i <= 3; i++ {
		mustAppend(t, w, 1, testRec(i, 1, 10*i, "k", []byte("v")))
		mustSeal(t, w)
	}
	m := w.Manifest()
	total := int64(0)
	for _, s := range m.Segments {
		total += s.SizeBytes
	}
	if total <= 1 {
		t.Fatalf("unexpected total size %d", total)
	}
	res, err := w.GC(context.Background(), RetentionPolicy{MaxBytes: total - 1})
	if err != nil {
		t.Fatalf("GC size: %v", err)
	}
	if len(res.Deleted) != 1 {
		t.Fatalf("deleted = %d, want 1", len(res.Deleted))
	}
	if res.RemainingBytes > total-1 {
		t.Fatalf("remaining bytes %d > budget %d", res.RemainingBytes, total-1)
	}
	if res.RemainingSegments != 2 {
		t.Fatalf("remaining segments = %d, want 2", res.RemainingSegments)
	}
	m = w.Manifest()
	if len(m.Segments) != 2 || m.Segments[0].StartIndex != 2 {
		t.Fatalf("retained = %+v", m.Segments)
	}
	if trim, ok := m.TrimmedFor(1); !ok || trim.ThroughIndex != 1 {
		t.Fatalf("trim = %+v, ok=%v", trim, ok)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r := mustReader(t, dir)
	if got := replay(t, r, 1, Target{}); len(got) != 2 || got[0].Entry.Index != 2 {
		t.Fatalf("replay after size GC = %+v", got)
	}
}

func TestRetentionNoOpAndClosed(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 1, testRec(1, 1, 10, "k", nil))
	mustSeal(t, w)
	res, err := w.GC(context.Background(), RetentionPolicy{})
	if err != nil || len(res.Deleted) != 0 {
		t.Fatalf("no-op GC = %+v, %v", res, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := w.GC(context.Background(), RetentionPolicy{MaxAge: time.Hour}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed GC error = %v, want ErrClosed", err)
	}
}
