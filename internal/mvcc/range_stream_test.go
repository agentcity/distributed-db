package mvcc

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"distributed-db/internal/storage"
)

// cloneRecords deep-copies a batch so a test can retain it after the callback
// returns without depending on the callback ownership window.
func cloneRecords(batch []Record) []Record {
	out := make([]Record, len(batch))
	for i := range batch {
		out[i] = Record{
			Key:   append([]byte(nil), batch[i].Key...),
			Value: append([]byte(nil), batch[i].Value...),
		}
	}
	return out
}

// collectStreamRange runs StreamRange and returns both the flattened records
// and the batches as they were handed to the callback.
func collectStreamRange(t *testing.T, store *Store, start, end []byte, batchSize int) ([]Record, [][]Record) {
	t.Helper()
	var batches [][]Record
	if err := store.StreamRange(start, end, batchSize, func(batch []Record) error {
		batches = append(batches, cloneRecords(batch))
		return nil
	}); err != nil {
		t.Fatalf("StreamRange(%q, %q, batchSize=%d): %v", start, end, batchSize, err)
	}
	records := make([]Record, 0)
	for _, batch := range batches {
		records = append(records, batch...)
	}
	return records, batches
}

// requireSameRecords compares two record slices key-by-key and byte-for-byte.
func requireSameRecords(t *testing.T, got, want []Record) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("records = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i].Key, want[i].Key) || !bytes.Equal(got[i].Value, want[i].Value) {
			t.Fatalf("record %d = {%x, %x}, want {%x, %x}", i, got[i].Key, got[i].Value, want[i].Key, want[i].Value)
		}
	}
}

// TestStreamRangeDeliversEveryRecordOnce verifies that a range larger than the
// batch size is delivered in full, in order, with full batches except the last.
func TestStreamRangeDeliversEveryRecordOnce(t *testing.T) {
	store := newTestStore(t)
	base := uint64(time.Now().UnixMilli()) << 18

	// 30 keys x 3 versions x (data + write) = 180 raw records.
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("key-%02d", i)
		for v := 0; v < 3; v++ {
			startTS := base + uint64(i)*100 + uint64(v)*10 + 1
			putValue(t, store, key, fmt.Sprintf("v%d", v), startTS, startTS+1)
		}
	}

	want, err := store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange: %v", err)
	}
	if len(want) != 180 {
		t.Fatalf("ExportRange = %d records, want 180", len(want))
	}

	const batchSize = 32
	got, batches := collectStreamRange(t, store, nil, nil, batchSize)
	requireSameRecords(t, got, want)

	wantBatches := (len(want) + batchSize - 1) / batchSize
	if len(batches) != wantBatches {
		t.Fatalf("StreamRange made %d batches, want %d", len(batches), wantBatches)
	}
	for i, batch := range batches {
		if len(batch) == 0 {
			t.Fatalf("batch %d is empty", i)
		}
		if len(batch) > batchSize {
			t.Fatalf("batch %d has %d records, want at most %d", i, len(batch), batchSize)
		}
		if i < len(batches)-1 && len(batch) != batchSize {
			t.Fatalf("batch %d has %d records, want full batch of %d", i, len(batch), batchSize)
		}
	}

	// Raw keys are strictly ascending, which also proves no record was repeated.
	for i := 1; i < len(got); i++ {
		if bytes.Compare(got[i-1].Key, got[i].Key) >= 0 {
			t.Fatalf("records not in raw key order at %d: %x >= %x", i, got[i-1].Key, got[i].Key)
		}
	}
}

func TestNormalizeStreamBatchSize(t *testing.T) {
	for _, tc := range []struct {
		in, want int
	}{
		{0, defaultStreamBatchSize},
		{-1, defaultStreamBatchSize},
		{1, 1},
		{1024, 1024},
		{maxStreamBatchSize, maxStreamBatchSize},
		{maxStreamBatchSize + 1, maxStreamBatchSize},
		{1 << 30, maxStreamBatchSize},
	} {
		if got := normalizeStreamBatchSize(tc.in); got != tc.want {
			t.Fatalf("normalizeStreamBatchSize(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestStreamRangeBatchSizes(t *testing.T) {
	store := newTestStore(t)
	base := uint64(time.Now().UnixMilli()) << 18
	for i := 0; i < 5; i++ {
		startTS := base + uint64(i)*10 + 1
		putValue(t, store, fmt.Sprintf("key-%d", i), "value", startTS, startTS+1)
	}
	want, err := store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange: %v", err)
	}
	if len(want) != 10 {
		t.Fatalf("ExportRange = %d records, want 10", len(want))
	}

	for _, batchSize := range []int{-10, 0, 1, 2, 3, 7} {
		got, batches := collectStreamRange(t, store, nil, nil, batchSize)
		requireSameRecords(t, got, want)

		effective := normalizeStreamBatchSize(batchSize)
		if effective > len(want) {
			effective = len(want)
		}
		wantBatches := (len(want) + effective - 1) / effective
		if len(batches) != wantBatches {
			t.Fatalf("batchSize %d: made %d batches, want %d", batchSize, len(batches), wantBatches)
		}
		for i, batch := range batches {
			if len(batch) == 0 || len(batch) > effective {
				t.Fatalf("batchSize %d: batch %d has %d records, want 1..%d", batchSize, i, len(batch), effective)
			}
			if i < len(batches)-1 && len(batch) != effective {
				t.Fatalf("batchSize %d: batch %d has %d records, want full batch of %d", batchSize, i, len(batch), effective)
			}
		}
	}
}

// TestStreamRangeFreshBatchSlices checks that each callback receives a distinct
// batch backing slice, so retained batches are not aliased by later batches.
func TestStreamRangeFreshBatchSlices(t *testing.T) {
	store := newTestStore(t)
	base := uint64(time.Now().UnixMilli()) << 18
	for i := 0; i < 6; i++ {
		startTS := base + uint64(i)*10 + 1
		putValue(t, store, fmt.Sprintf("key-%d", i), "value", startTS, startTS+1)
	}

	var firsts []*Record
	if err := store.StreamRange(nil, nil, 2, func(batch []Record) error {
		if len(batch) != 2 {
			t.Fatalf("batch has %d records, want 2", len(batch))
		}
		firsts = append(firsts, &batch[0])
		return nil
	}); err != nil {
		t.Fatalf("StreamRange: %v", err)
	}
	if len(firsts) != 6 {
		t.Fatalf("StreamRange made %d full batches, want 6", len(firsts))
	}
	seen := make(map[*Record]struct{}, len(firsts))
	for i, rec := range firsts {
		if _, duplicate := seen[rec]; duplicate {
			t.Fatalf("batch %d reused a previous batch's backing slice", i)
		}
		seen[rec] = struct{}{}
	}
}

func TestStreamRangeCallbackError(t *testing.T) {
	store := newTestStore(t)
	base := uint64(time.Now().UnixMilli()) << 18
	for i := 0; i < 5; i++ {
		startTS := base + uint64(i)*10 + 1
		putValue(t, store, fmt.Sprintf("key-%d", i), "value", startTS, startTS+1)
	}
	want, err := store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange: %v", err)
	}

	for _, tc := range []struct {
		batchSize int
		failAt    int
	}{
		{2, 1},
		{2, 2},
		{2, 5}, // last full batch
		{3, 4}, // final partial batch, flushed after the scan
	} {
		sentinel := errors.New("stop streaming")
		calls := 0
		var seen []Record
		err := store.StreamRange(nil, nil, tc.batchSize, func(batch []Record) error {
			calls++
			if len(batch) == 0 {
				t.Fatal("callback received an empty batch")
			}
			seen = append(seen, cloneRecords(batch)...)
			if calls == tc.failAt {
				return sentinel
			}
			return nil
		})
		if err != sentinel {
			t.Fatalf("batchSize %d failAt %d: error = %v, want the callback error unchanged", tc.batchSize, tc.failAt, err)
		}
		if calls != tc.failAt {
			t.Fatalf("batchSize %d failAt %d: callback called %d times, want %d", tc.batchSize, tc.failAt, calls, tc.failAt)
		}
		// Records delivered before the error are a prefix of the full range.
		requireSameRecords(t, seen, want[:len(seen)])
	}

	// The aborted streams left no iterator or snapshot behind: a fresh stream
	// still delivers everything.
	got, _ := collectStreamRange(t, store, nil, nil, 3)
	requireSameRecords(t, got, want)
}

func TestStreamRangeEmptyRanges(t *testing.T) {
	empty := newTestStore(t)
	if err := empty.StreamRange(nil, nil, 1, func(batch []Record) error {
		t.Fatalf("empty store: callback called with %d records", len(batch))
		return nil
	}); err != nil {
		t.Fatalf("empty store StreamRange: %v", err)
	}

	store := newTestStore(t)
	base := uint64(time.Now().UnixMilli()) << 18
	putValue(t, store, "a", "va", base+1, base+2)

	for _, tc := range []struct {
		name       string
		start, end []byte
	}{
		{"equal bounds", []byte("a"), []byte("a")},
		{"inverted bounds", []byte("c"), []byte("b")},
		{"empty end", nil, []byte{}},
		{"past the end", []byte("z"), nil},
	} {
		for _, batchSize := range []int{0, 1, 1024} {
			calls := 0
			if err := store.StreamRange(tc.start, tc.end, batchSize, func(batch []Record) error {
				calls++
				return nil
			}); err != nil {
				t.Fatalf("%s batchSize %d: StreamRange: %v", tc.name, batchSize, err)
			}
			if calls != 0 {
				t.Fatalf("%s batchSize %d: callback called %d times, want 0", tc.name, batchSize, calls)
			}
		}
	}

	if err := store.StreamRange(nil, nil, 1, nil); err == nil {
		t.Fatal("StreamRange with a nil callback succeeded")
	}
}

// TestStreamRangeBoundariesAndPrefixKeys mirrors the ExportRange boundary
// matrix: keys that are prefixes of each other are not contiguous in the raw
// encoding, so the user key must still be decoded and filtered.
func TestStreamRangeBoundariesAndPrefixKeys(t *testing.T) {
	store := newTestStore(t)
	base := uint64(time.Now().UnixMilli()) << 18

	keys := []string{"a", "a\x00", "a0", "b"}
	for i, key := range keys {
		startTS := base + uint64(2*i+1)
		putValue(t, store, key, "v"+key, startTS, startTS+1)
	}

	for _, tc := range []struct {
		name       string
		start, end []byte
		wantCount  int
	}{
		{"all", nil, nil, 8},
		{"a to b", []byte("a"), []byte("b"), 6},
		{"a to a\\x01", []byte("a"), []byte("a\x01"), 4},
		{"a\\x00 to a0", []byte("a\x00"), []byte("a0"), 2},
		{"a0 to end", []byte("a0"), nil, 4},
		{"empty", []byte("a"), []byte("a"), 0},
		{"inverted", []byte("b"), []byte("a"), 0},
		{"past the end", []byte("z"), nil, 0},
		{"empty end", nil, []byte{}, 0},
	} {
		want, err := store.ExportRange(tc.start, tc.end)
		if err != nil {
			t.Fatalf("%s: ExportRange: %v", tc.name, err)
		}
		if len(want) != tc.wantCount {
			t.Fatalf("%s: ExportRange = %d records, want %d", tc.name, len(want), tc.wantCount)
		}
		got, _ := collectStreamRange(t, store, tc.start, tc.end, 1)
		requireSameRecords(t, got, want)
	}
}

// TestStreamRangeIncludesLocksAndRollback checks that every raw record kind
// ExportRange includes is streamed: committed data/write records, an
// uncommitted lock with its provisional data, and a rollback write record.
func TestStreamRangeIncludesLocksAndRollback(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)
	base := uint64(time.Now().UnixMilli()) << 18

	putValue(t, store, "a", "va", base+1, base+2)

	lockTS := base + 10
	if err := store.Prewrite([]Mutation{{Key: []byte("b"), Value: []byte("pending")}}, []byte("b"), lockTS); err != nil {
		t.Fatalf("Prewrite b: %v", err)
	}
	rollbackTS := base + 20
	if err := store.Prewrite([]Mutation{{Key: []byte("c"), Value: []byte("rolled-back")}}, []byte("c"), rollbackTS); err != nil {
		t.Fatalf("Prewrite c: %v", err)
	}
	if err := store.Rollback([][]byte{[]byte("c")}, rollbackTS); err != nil {
		t.Fatalf("Rollback c: %v", err)
	}

	want, err := store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange: %v", err)
	}
	got, _ := collectStreamRange(t, store, nil, nil, 1)
	requireSameRecords(t, got, want)

	lockValue, err := engine.Get(lockKey([]byte("b")))
	if err != nil {
		t.Fatalf("engine.Get(lock b): %v", err)
	}
	sawLock, sawRollback := false, false
	for _, rec := range got {
		switch {
		case bytes.Equal(rec.Key, lockKey([]byte("b"))):
			sawLock = true
			if !bytes.Equal(rec.Value, lockValue) {
				t.Fatalf("streamed lock = %x, want %x", rec.Value, lockValue)
			}
		case bytes.Equal(rec.Key, writeKey([]byte("c"), rollbackTS)):
			sawRollback = true
			write, err := decodeWrite(rec.Value)
			if err != nil {
				t.Fatalf("decode rolled-back write: %v", err)
			}
			if write.kind != kindRollback || write.startTS != rollbackTS {
				t.Fatalf("streamed write = %+v, want rollback at %d", write, rollbackTS)
			}
		}
	}
	if !sawLock {
		t.Fatal("stream did not include the lock record")
	}
	if !sawRollback {
		t.Fatal("stream did not include the rollback write record")
	}

	// Mutating a delivered batch must not affect the store, so the next stream
	// is identical to the first one.
	if err := store.StreamRange(nil, nil, 2, func(batch []Record) error {
		for i := range batch {
			for j := range batch[i].Key {
				batch[i].Key[j] = 0xff
			}
			for j := range batch[i].Value {
				batch[i].Value[j] = 0xff
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("mutating StreamRange: %v", err)
	}
	after, err := store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange after mutation: %v", err)
	}
	requireSameRecords(t, after, want)
	stillLocked, err := engine.Get(lockKey([]byte("b")))
	if err != nil {
		t.Fatalf("engine.Get(lock b) after mutation: %v", err)
	}
	if !bytes.Equal(stillLocked, lockValue) {
		t.Fatalf("store lock changed to %x, want %x", stillLocked, lockValue)
	}
}

// TestStreamRangeSnapshotIsolation writes new records, updates an existing key
// and deletes another key from inside the callback. The stream must keep
// returning the snapshot taken before the first record was read.
func TestStreamRangeSnapshotIsolation(t *testing.T) {
	store := newTestStore(t)
	base := uint64(time.Now().UnixMilli()) << 18

	putValue(t, store, "a", "va", base+1, base+2)
	putValue(t, store, "b", "vb", base+3, base+4)
	putValue(t, store, "c", "vc", base+5, base+6)

	before, err := store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange before stream: %v", err)
	}

	// The writes use timestamps far above the seeded ones so they are
	// unambiguously new.
	var got []Record
	wrote := false
	if err := store.StreamRange(nil, nil, 1, func(batch []Record) error {
		got = append(got, cloneRecords(batch)...)
		if !wrote {
			wrote = true
			putValue(t, store, "aa", "new", base+100, base+101)
			putValue(t, store, "b", "b2", base+102, base+103)
			deleteValue(t, store, "c", base+104, base+105)
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamRange: %v", err)
	}
	if !wrote {
		t.Fatal("callback never ran, so no concurrent write was attempted")
	}
	requireSameRecords(t, got, before)

	// The writes really happened and are visible after the stream.
	requireValue(t, store, "aa", base+200, "new")
	requireValue(t, store, "b", base+200, "b2")
	requireKeyNotFound(t, store, "c", base+200)
	after, err := store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange after stream: %v", err)
	}
	if len(after) <= len(before) {
		t.Fatalf("ExportRange after stream = %d records, want more than %d", len(after), len(before))
	}
}

// TestStreamRangeDefaultBatch covers a range that exactly fills one default
// batch and crosses into a second, ensuring the default does not lose records.
func TestStreamRangeDefaultBatch(t *testing.T) {
	store := newTestStore(t)
	base := uint64(time.Now().UnixMilli()) << 18

	// 512 keys produce 1024 data/write records: exactly one default batch.
	for i := 0; i < 512; i++ {
		startTS := base + uint64(i)*10 + 1
		putValue(t, store, fmt.Sprintf("key-%04d", i), "value", startTS, startTS+1)
	}
	want, err := store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange: %v", err)
	}
	if len(want) != 1024 {
		t.Fatalf("ExportRange = %d records, want 1024", len(want))
	}

	got, batches := collectStreamRange(t, store, nil, nil, 0)
	requireSameRecords(t, got, want)
	if len(batches) != 1 || len(batches[0]) != len(want) {
		t.Fatalf("default batch yielded %d batches, want one batch of %d", len(batches), len(want))
	}
}

// BenchmarkStreamRange measures snapshot iteration and per-batch copying. Each
// committed put stores one data and one write record.
func BenchmarkStreamRange(b *testing.B) {
	engine, err := storage.Open(b.TempDir(), storage.Options{
		CacheSize:    8 << 20,
		MemTableSize: 1 << 20,
		Compression:  "none",
	})
	if err != nil {
		b.Fatalf("storage.Open: %v", err)
	}
	b.Cleanup(func() {
		if err := engine.Close(); err != nil {
			b.Errorf("engine.Close: %v", err)
		}
	})
	store := New(engine)

	const keys = 2000
	base := uint64(time.Now().UnixMilli()) << 18
	for i := 0; i < keys; i++ {
		key := []byte(fmt.Sprintf("key-%06d", i))
		startTS := base + uint64(i)*10 + 1
		if err := store.Prewrite([]Mutation{{Key: key, Value: []byte("value")}}, key, startTS); err != nil {
			b.Fatalf("Prewrite: %v", err)
		}
		if err := store.Commit([][]byte{key}, startTS, startTS+1); err != nil {
			b.Fatalf("Commit: %v", err)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		records := 0
		if err := store.StreamRange(nil, nil, 1024, func(batch []Record) error {
			records += len(batch)
			return nil
		}); err != nil {
			b.Fatalf("StreamRange: %v", err)
		}
		if records != 2*keys {
			b.Fatalf("StreamRange delivered %d records, want %d", records, 2*keys)
		}
	}
}
