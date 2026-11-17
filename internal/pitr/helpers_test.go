package pitr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/backup"
	"distributed-db/internal/model"
)

var testTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: testTime} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testRec(index, term, tso uint64, key string, value []byte) LogRecord {
	return LogRecord{
		TSO: tso,
		Entry: model.Entry{
			Term:  term,
			Index: index,
			Type:  model.CommandPut,
			Key:   key,
			Value: value,
		},
	}
}

func newArchiveWriter(t *testing.T, dir string, clock *testClock, maxBytes int64) *Writer {
	t.Helper()
	w, err := NewWriter(WriterConfig{
		Dir:             dir,
		ClusterID:       "cluster-a",
		Incarnation:     1,
		WriterEpoch:     7,
		SegmentMaxBytes: maxBytes,
		SegmentMaxAge:   time.Hour,
		Now:             clock.Now,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func mustAppend(t *testing.T, w *Writer, rangeID uint64, recs ...LogRecord) {
	t.Helper()
	if err := w.Append(context.Background(), rangeID, recs); err != nil {
		t.Fatalf("Append(range %d): %v", rangeID, err)
	}
}

func mustSeal(t *testing.T, w *Writer) {
	t.Helper()
	if err := w.Seal(context.Background()); err != nil {
		t.Fatalf("Seal: %v", err)
	}
}

func mustReader(t *testing.T, dir string) *Reader {
	t.Helper()
	r, err := OpenReader(dir)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	return r
}

func replay(t *testing.T, r *Reader, rangeID uint64, target Target) []LogRecord {
	t.Helper()
	var got []LogRecord
	if err := r.ReplayRange(context.Background(), rangeID, target, func(rec LogRecord) error {
		got = append(got, rec)
		return nil
	}); err != nil {
		t.Fatalf("ReplayRange: %v", err)
	}
	return got
}

func testBase(tso uint64, ranges ...backup.Range) *backup.Manifest {
	return &backup.Manifest{
		Format:        "distributed-db-kvbackup",
		FormatVersion: 1,
		SnapshotTS:    tso,
		Ranges:        ranges,
	}
}

func baseRange(id uint64, start, end []byte) backup.Range {
	return backup.Range{ID: id, Start: start, End: end}
}

func segmentFile(t *testing.T, dir string, info SegmentInfo) string {
	t.Helper()
	p, err := segmentFilePath(dir, info)
	if err != nil {
		t.Fatalf("segmentFilePath: %v", err)
	}
	return p
}

func writeRawFile(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", p, err)
	}
}

func mustNotExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("%s still exists or stat failed: %v", p, err)
	}
}

func formatRecord(rec LogRecord) string {
	return fmt.Sprintf("%d/%d@%d", rec.Entry.Index, rec.Entry.Term, rec.TSO)
}
