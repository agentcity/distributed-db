package pitr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func buildThreeSegmentArchive(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	for i := uint64(1); i <= 3; i++ {
		mustAppend(t, w, 1, testRec(i, 1, 100+i, "k", []byte("v")))
		mustSeal(t, w)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dir
}

func TestCorruptedSegmentRejected(t *testing.T) {
	t.Run("flipped payload byte", func(t *testing.T) {
		dir := t.TempDir()
		clock := newTestClock()
		w := newArchiveWriter(t, dir, clock, 1<<20)
		mustAppend(t, w, 1,
			testRec(1, 1, 100, "a", []byte("one")),
			testRec(2, 1, 110, "b", []byte("two")))
		mustSeal(t, w)
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		r := mustReader(t, dir)
		info := r.ListSegments()[0]
		p := segmentFile(t, dir, info)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		data[len(data)/2] ^= 0xff
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		err = r.ReplayRange(context.Background(), 1, Target{}, func(LogRecord) error { return nil })
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt replay error = %v, want ErrCorrupt", err)
		}
		if _, err := r.Verify(context.Background()); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Verify error = %v, want ErrCorrupt", err)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		dir := t.TempDir()
		clock := newTestClock()
		w := newArchiveWriter(t, dir, clock, 1<<20)
		mustAppend(t, w, 1, testRec(1, 1, 100, "a", []byte("one")))
		mustSeal(t, w)
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		r := mustReader(t, dir)
		info := r.ListSegments()[0]
		p := segmentFile(t, dir, info)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if err := os.WriteFile(p, data[:len(data)-1], 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := r.Verify(context.Background()); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Verify error = %v, want ErrCorrupt", err)
		}
	})

	t.Run("manifest json", func(t *testing.T) {
		dir := t.TempDir()
		clock := newTestClock()
		w := newArchiveWriter(t, dir, clock, 1<<20)
		mustAppend(t, w, 1, testRec(1, 1, 100, "a", nil))
		mustSeal(t, w)
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		p := filepath.Join(dir, ManifestFile)
		if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := OpenReader(dir); !errors.Is(err, ErrManifest) {
			t.Fatalf("OpenReader error = %v, want ErrManifest", err)
		}
	})
}

func TestGapDetection(t *testing.T) {
	dir := buildThreeSegmentArchive(t)
	r := mustReader(t, dir)
	segs := r.ListSegments()
	if len(segs) != 3 {
		t.Fatalf("segments = %d, want 3", len(segs))
	}
	middle := segmentFile(t, dir, segs[1])
	if err := os.Remove(middle); err != nil {
		t.Fatalf("remove middle: %v", err)
	}
	err := r.ReplayRange(context.Background(), 1, Target{}, func(LogRecord) error { return nil })
	if !errors.Is(err, ErrGap) || !errors.Is(err, ErrMissing) {
		t.Fatalf("replay error = %v, want ErrGap/ErrMissing", err)
	}
	if _, err := r.Verify(context.Background()); !errors.Is(err, ErrGap) {
		t.Fatalf("Verify error = %v, want ErrGap", err)
	}

	// A manifest that skips the middle index range must be rejected at load.
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	m.Segments = []SegmentInfo{m.Segments[0], m.Segments[2]}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), append(data, '\n'), 0o644); err != nil {
		t.Fatalf("WriteFile manifest: %v", err)
	}
	if _, err := OpenReader(dir); !errors.Is(err, ErrGap) {
		t.Fatalf("OpenReader gap error = %v, want ErrGap", err)
	}
}

func TestManifestValidationRejectsBadChain(t *testing.T) {
	dir := buildThreeSegmentArchive(t)
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	m.Segments[2].PrevSHA256 = m.Segments[0].SHA256
	if err := m.Validate(); !errors.Is(err, ErrGap) {
		t.Fatalf("Validate chain error = %v, want ErrGap", err)
	}
}

func TestReplayAllAndVerify(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 2, testRec(1, 1, 20, "b", nil))
	mustAppend(t, w, 1, testRec(1, 1, 10, "a", nil))
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r := mustReader(t, dir)
	var order []uint64
	if err := r.ReplayAll(context.Background(), Target{}, func(rangeID uint64, rec LogRecord) error {
		order = append(order, rangeID)
		return nil
	}); err != nil {
		t.Fatalf("ReplayAll: %v", err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("ReplayAll order = %v, want [1 2]", order)
	}
	report, err := r.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.Ranges != 2 || report.Segments != 2 || report.Records != 2 {
		t.Fatalf("report = %+v", report)
	}
}

func TestVerifyFile(t *testing.T) {
	dir := buildThreeSegmentArchive(t)
	r := mustReader(t, dir)
	for _, info := range r.ListSegments() {
		if err := VerifyFile(dir, r.Manifest().ClusterID, info); err != nil {
			t.Fatalf("VerifyFile %s: %v", info.Path, err)
		}
	}
	info := r.ListSegments()[0]
	info.SizeBytes++
	if err := VerifyFile(dir, r.Manifest().ClusterID, info); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("VerifyFile modified descriptor error = %v, want ErrCorrupt", err)
	}
}
