package kvserver

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
	"testing"
	"time"

	"distributed-db/internal/mvcc"
	"distributed-db/internal/storage"
)

type streamKV struct {
	key   []byte
	value []byte
}

func openStreamTestEngine(t *testing.T) storage.Engine {
	t.Helper()
	engine, err := storage.Open(t.TempDir(), storage.Options{
		CacheSize:    8 << 20,
		MemTableSize: 4 << 20,
	})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close storage: %v", err)
		}
	})
	return engine
}

func newStreamTestSM(t *testing.T, shared storage.Engine, rangeID uint64) (*stateMachine, storage.Engine) {
	t.Helper()
	engine := storage.Prefix(shared, rangePrefix(rangeID))
	return &stateMachine{store: mvcc.New(engine), engine: engine}, engine
}

func seedStreamTest(t *testing.T, engine storage.Engine, pairs []streamKV) {
	t.Helper()
	batch := engine.NewBatch()
	defer func() { _ = batch.Close() }()
	staged := 0
	commit := func() {
		t.Helper()
		if err := batch.Commit(); err != nil {
			t.Fatalf("commit seed batch: %v", err)
		}
		if err := batch.Close(); err != nil {
			t.Fatalf("close seed batch: %v", err)
		}
		batch = engine.NewBatch()
		staged = 0
	}
	for _, p := range pairs {
		if err := batch.Set(p.key, p.value); err != nil {
			t.Fatalf("seed set %q: %v", p.key, err)
		}
		staged++
		if staged >= snapshotBatchEntries {
			commit()
		}
	}
	if staged > 0 {
		commit()
	}
}

func dumpStreamTest(t *testing.T, engine storage.Engine) []streamKV {
	t.Helper()
	it, err := engine.NewIter(nil, nil)
	if err != nil {
		t.Fatalf("iterate engine: %v", err)
	}
	defer it.Close()
	var out []streamKV
	for it.Valid() {
		out = append(out, streamKV{
			key:   append([]byte(nil), it.Key()...),
			value: append([]byte(nil), it.Value()...),
		})
		it.Next()
	}
	if err := it.Error(); err != nil {
		t.Fatalf("iterate engine: %v", err)
	}
	return out
}

func sortedStreamKVs(pairs []streamKV) []streamKV {
	out := make([]streamKV, len(pairs))
	for i, p := range pairs {
		out[i] = streamKV{
			key:   append([]byte(nil), p.key...),
			value: append([]byte(nil), p.value...),
		}
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].key, out[j].key) < 0 })
	return out
}

func assertStreamContents(t *testing.T, engine storage.Engine, want []streamKV) {
	t.Helper()
	want = sortedStreamKVs(want)
	got := dumpStreamTest(t, engine)
	if len(got) != len(want) {
		t.Fatalf("engine holds %d keys, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i].key, want[i].key) {
			t.Fatalf("key %d = %q, want %q", i, got[i].key, want[i].key)
		}
		if !bytes.Equal(got[i].value, want[i].value) {
			t.Fatalf("value for %q = %x, want %x", got[i].key, got[i].value, want[i].value)
		}
	}
}

// captureStream snapshots sm into memory and returns the bytes and index.
func captureStream(t *testing.T, sm *stateMachine) ([]byte, uint64) {
	t.Helper()
	var buf bytes.Buffer
	index, err := sm.StreamSnapshot(&buf)
	if err != nil {
		t.Fatalf("StreamSnapshot: %v", err)
	}
	return append([]byte(nil), buf.Bytes()...), index
}

func splitStreamLines(t *testing.T, data []byte) [][]byte {
	t.Helper()
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("stream does not end with LF: %q", data)
	}
	return bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
}

func joinStreamLines(lines [][]byte) []byte {
	out := bytes.Join(lines, []byte("\n"))
	return append(out, '\n')
}

func mutateStreamLine(t *testing.T, data []byte, line int, mutate func(map[string]any)) []byte {
	t.Helper()
	lines := splitStreamLines(t, data)
	if line < 0 || line >= len(lines) {
		t.Fatalf("stream line %d out of range (%d lines)", line, len(lines))
	}
	var obj map[string]any
	if err := json.Unmarshal(lines[line], &obj); err != nil {
		t.Fatalf("decode stream line %d: %v", line, err)
	}
	mutate(obj)
	encoded, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("encode stream line %d: %v", line, err)
	}
	lines[line] = encoded
	return joinStreamLines(lines)
}

// encodeStreamForTest builds a v1 stream from entries in the given order. It is
// intentionally independent of StreamSnapshot so tests can construct streams
// with semantic defects (unsorted/duplicate keys) that a real snapshot cannot
// produce.
func encodeStreamForTest(t *testing.T, index uint64, entries []streamKV) []byte {
	t.Helper()
	var buf bytes.Buffer
	headerIndex := index
	if err := writeSnapshotJSON(&buf, snapshotStreamHeader{
		Format:  snapshotStreamFormat,
		Version: snapshotStreamVersion,
		Index:   &headerIndex,
	}); err != nil {
		t.Fatalf("write test header: %v", err)
	}
	digest := crc32.NewIEEE()
	var crcBuf [4]byte
	for _, e := range entries {
		crc := snapshotEntryCRC(e.key, e.value)
		if err := writeSnapshotJSON(&buf, snapshotStreamEntry{
			Key:   snapshotNonNull(e.key),
			Value: snapshotNonNull(e.value),
			CRC:   crc,
		}); err != nil {
			t.Fatalf("write test entry: %v", err)
		}
		binary.BigEndian.PutUint32(crcBuf[:], crc)
		digest.Write(crcBuf[:])
	}
	if err := writeSnapshotJSON(&buf, snapshotStreamTrailer{
		End:   true,
		Count: uint64(len(entries)),
		CRC:   digest.Sum32(),
	}); err != nil {
		t.Fatalf("write test trailer: %v", err)
	}
	return buf.Bytes()
}

// TestSnapshotStreamRoundTrip covers small streams, arbitrary binary and
// non-UTF-8 keys/values, a multi-megabyte value, index propagation and range
// isolation through storage.Prefix.
func TestSnapshotStreamRoundTrip(t *testing.T) {
	shared := openStreamTestEngine(t)
	src, srcEngine := newStreamTestSM(t, shared, 1)
	dst, dstEngine := newStreamTestSM(t, shared, 2)
	_, bystanderEngine := newStreamTestSM(t, shared, 3)

	bigValue := make([]byte, (1<<20)+17)
	for i := range bigValue {
		bigValue[i] = byte((i*131 + 7) % 256)
	}
	pairs := []streamKV{
		{key: []byte{}, value: []byte("empty-key")},
		{key: []byte{0x00}, value: []byte{}},
		{key: []byte{0x00, 0x01, 0xff, 0xfe}, value: []byte{0x00, 0xff, 0x80, 0x81}},
		{key: []byte("a"), value: []byte("one")},
		{key: []byte("a\x00"), value: []byte{0x00}},
		{key: []byte{0xff, 0x00, 'z'}, value: bigValue},
		{key: []byte("ключ"), value: []byte("значение")},
	}
	seedStreamTest(t, srcEngine, pairs)
	src.lastApplied.Store(42)
	seedStreamTest(t, dstEngine, []streamKV{{key: []byte("stale"), value: []byte("old")}})
	seedStreamTest(t, bystanderEngine, []streamKV{{key: []byte("keep"), value: []byte("me")}})

	data, index := captureStream(t, src)
	if index != 42 {
		t.Fatalf("StreamSnapshot index = %d, want 42", index)
	}
	if err := dst.RestoreSnapshotStream(bytes.NewReader(data)); err != nil {
		t.Fatalf("RestoreSnapshotStream: %v", err)
	}
	if got := dst.lastApplied.Load(); got != 42 {
		t.Fatalf("restored lastApplied = %d, want 42", got)
	}
	assertStreamContents(t, dstEngine, pairs)
	// The source and an unrelated range sharing the same physical engine are
	// untouched.
	assertStreamContents(t, srcEngine, pairs)
	assertStreamContents(t, bystanderEngine, []streamKV{{key: []byte("keep"), value: []byte("me")}})

	// The stream is self-describing: its header names the format and version.
	lines := splitStreamLines(t, data)
	if len(lines) != len(pairs)+2 {
		t.Fatalf("stream has %d lines, want %d", len(lines), len(pairs)+2)
	}
	var header struct {
		Format  string `json:"format"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.Format != snapshotStreamFormat || header.Version != snapshotStreamVersion {
		t.Fatalf("header = %+v, want format %q version %d", header, snapshotStreamFormat, snapshotStreamVersion)
	}
}

// TestSnapshotStreamLargeRoundTrip round-trips 20k keys and compares every
// key/value exactly.
func TestSnapshotStreamLargeRoundTrip(t *testing.T) {
	const numKeys = 20000

	srcShared := openStreamTestEngine(t)
	src, srcEngine := newStreamTestSM(t, srcShared, 1)
	src.lastApplied.Store(123456)

	pairs := make([]streamKV, numKeys)
	for i := range pairs {
		key := []byte(fmt.Sprintf("key-%06d", i))
		value := []byte(fmt.Sprintf("value-%06d-%s", i, bytes.Repeat([]byte{'x'}, i%37)))
		value = append(value, byte(i))
		pairs[i] = streamKV{key: key, value: value}
	}
	seedStreamTest(t, srcEngine, pairs)

	data, index := captureStream(t, src)
	if index != 123456 {
		t.Fatalf("StreamSnapshot index = %d, want 123456", index)
	}

	dstShared := openStreamTestEngine(t)
	dst, dstEngine := newStreamTestSM(t, dstShared, 9)
	dst.lastApplied.Store(1)
	if err := dst.RestoreSnapshotStream(bytes.NewReader(data)); err != nil {
		t.Fatalf("RestoreSnapshotStream: %v", err)
	}
	if got := dst.lastApplied.Load(); got != 123456 {
		t.Fatalf("restored lastApplied = %d, want 123456", got)
	}
	assertStreamContents(t, dstEngine, pairs)
}

// TestRestoreSnapshotStreamReplacesExistingKeys verifies that keys absent from
// the stream disappear instead of surviving the restore.
func TestRestoreSnapshotStreamReplacesExistingKeys(t *testing.T) {
	shared := openStreamTestEngine(t)
	src, srcEngine := newStreamTestSM(t, shared, 1)
	dst, dstEngine := newStreamTestSM(t, shared, 2)

	newPairs := []streamKV{
		{key: []byte("b"), value: []byte("new-b")},
		{key: []byte("d"), value: []byte("new-d")},
	}
	seedStreamTest(t, srcEngine, newPairs)
	src.lastApplied.Store(11)
	seedStreamTest(t, dstEngine, []streamKV{
		{key: []byte("a"), value: []byte("old-a")},
		{key: []byte("b"), value: []byte("old-b")},
		{key: []byte("c"), value: []byte("old-c")},
	})

	data, index := captureStream(t, src)
	if index != 11 {
		t.Fatalf("StreamSnapshot index = %d, want 11", index)
	}
	if err := dst.RestoreSnapshotStream(bytes.NewReader(data)); err != nil {
		t.Fatalf("RestoreSnapshotStream: %v", err)
	}
	assertStreamContents(t, dstEngine, newPairs)
	for _, key := range []string{"a", "c"} {
		if _, err := dstEngine.Get([]byte(key)); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("key %q survived restore (err=%v)", key, err)
		}
	}
	if got := dst.lastApplied.Load(); got != 11 {
		t.Fatalf("restored lastApplied = %d, want 11", got)
	}
}

// TestSnapshotStreamEmptyState verifies the header+trailer-only stream and that
// restoring it empties the range.
func TestSnapshotStreamEmptyState(t *testing.T) {
	shared := openStreamTestEngine(t)
	src, _ := newStreamTestSM(t, shared, 1)
	src.lastApplied.Store(7)

	data, index := captureStream(t, src)
	if index != 7 {
		t.Fatalf("StreamSnapshot index = %d, want 7", index)
	}
	if lines := splitStreamLines(t, data); len(lines) != 2 {
		t.Fatalf("empty stream has %d lines, want header+trailer (2)", len(lines))
	}

	dst, dstEngine := newStreamTestSM(t, shared, 2)
	seedStreamTest(t, dstEngine, []streamKV{{key: []byte("stale"), value: []byte("data")}})
	if err := dst.RestoreSnapshotStream(bytes.NewReader(data)); err != nil {
		t.Fatalf("RestoreSnapshotStream: %v", err)
	}
	assertStreamContents(t, dstEngine, nil)
	if got := dst.lastApplied.Load(); got != 7 {
		t.Fatalf("restored lastApplied = %d, want 7", got)
	}
}

// TestRestoreSnapshotStreamRejectsBadStreams checks truncation, corruption,
// ordering defects and the documented partial-restore behaviour. A bad header
// must not touch existing keys; an error after the header may leave the range
// partially restored.
func TestRestoreSnapshotStreamRejectsBadStreams(t *testing.T) {
	entries := []streamKV{
		{key: []byte("a"), value: []byte("1")},
		{key: []byte("b"), value: []byte("2")},
		{key: []byte("c"), value: []byte("3")},
	}
	base := encodeStreamForTest(t, 5, entries)
	lines := splitStreamLines(t, base)
	if len(lines) != 5 {
		t.Fatalf("base stream has %d lines, want 5", len(lines))
	}

	midEntry := append(joinStreamLines(lines[:2]), lines[2][:len(lines[2])/2]...)
	beforeTrailer := joinStreamLines(lines[:len(lines)-1])

	cases := []struct {
		name        string
		data        []byte
		want        error
		destructive bool
	}{
		{"empty", nil, ErrSnapshotStreamTruncated, false},
		{"partial header", []byte(`{"format":"ddb-`), ErrSnapshotStreamTruncated, false},
		{"missing index", mutateStreamLine(t, base, 0, func(m map[string]any) { delete(m, "index") }), ErrSnapshotStreamCorrupt, false},
		{"bad format", mutateStreamLine(t, base, 0, func(m map[string]any) { m["format"] = "other" }), ErrSnapshotStreamCorrupt, false},
		{"bad version", mutateStreamLine(t, base, 0, func(m map[string]any) { m["version"] = float64(99) }), ErrSnapshotStreamCorrupt, false},
		{"partial entry", midEntry, ErrSnapshotStreamTruncated, true},
		{"header only", joinStreamLines(lines[:1]), ErrSnapshotStreamTruncated, true},
		{"missing trailer", beforeTrailer, ErrSnapshotStreamTruncated, true},
		{"bad entry crc", mutateStreamLine(t, base, 1, func(m map[string]any) { m["crc"] = float64(1) }), ErrSnapshotStreamCorrupt, true},
		{"bad trailer count", mutateStreamLine(t, base, len(lines)-1, func(m map[string]any) { m["count"] = float64(2) }), ErrSnapshotStreamCorrupt, true},
		{"bad trailer crc", mutateStreamLine(t, base, len(lines)-1, func(m map[string]any) { m["crc"] = float64(0) }), ErrSnapshotStreamCorrupt, true},
		{"trailing data", append(append([]byte(nil), base...), []byte("junk\n")...), ErrSnapshotStreamCorrupt, true},
		{"unsorted keys", encodeStreamForTest(t, 5, []streamKV{entries[1], entries[0], entries[2]}), ErrSnapshotStreamCorrupt, true},
		{"duplicate key", encodeStreamForTest(t, 5, []streamKV{entries[0], entries[0], entries[1]}), ErrSnapshotStreamCorrupt, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shared := openStreamTestEngine(t)
			dst, dstEngine := newStreamTestSM(t, shared, 1)
			seedStreamTest(t, dstEngine, []streamKV{{key: []byte("sentinel"), value: []byte("value")}})

			err := dst.RestoreSnapshotStream(bytes.NewReader(tc.data))
			if err == nil {
				t.Fatal("RestoreSnapshotStream succeeded, want error")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, tc.want)
			}
			if got := dst.lastApplied.Load(); got != 0 {
				t.Fatalf("lastApplied = %d after failed restore, want 0", got)
			}
			_, getErr := dstEngine.Get([]byte("sentinel"))
			if tc.destructive {
				if !errors.Is(getErr, storage.ErrNotFound) {
					t.Fatalf("sentinel survived a post-header error (err=%v); partial restore should have deleted it", getErr)
				}
			} else if getErr != nil {
				t.Fatalf("header error destroyed the existing keyspace: %v", getErr)
			}
		})
	}
}

type countingDiscardWriter struct {
	calls int
	total int
	max   int
}

func (w *countingDiscardWriter) Write(p []byte) (int, error) {
	w.calls++
	w.total += len(p)
	if len(p) > w.max {
		w.max = len(p)
	}
	return len(p), nil
}

// TestSnapshotStreamManyEntriesToDiscard streams 50k entries to a discard
// writer. It cannot measure peak heap exactly, but it exercises the
// bounded-batch path (no []entry accumulation) and checks that output is
// written incrementally in buffer-sized chunks rather than as one giant blob.
func TestSnapshotStreamManyEntriesToDiscard(t *testing.T) {
	const numKeys = 50000

	shared := openStreamTestEngine(t)
	src, srcEngine := newStreamTestSM(t, shared, 1)
	src.lastApplied.Store(777)

	pairs := make([]streamKV, numKeys)
	for i := range pairs {
		pairs[i] = streamKV{
			key:   []byte(fmt.Sprintf("k-%06d", i)),
			value: []byte(fmt.Sprintf("v-%06d", i)),
		}
	}
	seedStreamTest(t, srcEngine, pairs)

	var counter countingDiscardWriter
	writer := io.MultiWriter(io.Discard, &counter)
	start := time.Now()
	index, err := src.StreamSnapshot(writer)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("StreamSnapshot: %v", err)
	}
	if index != 777 {
		t.Fatalf("StreamSnapshot index = %d, want 777", index)
	}
	if counter.calls < 2 {
		t.Fatalf("stream was written in %d call(s), want incremental flushes", counter.calls)
	}
	if counter.total < 1<<20 {
		t.Fatalf("stream wrote %d bytes, want > 1 MiB for %d entries", counter.total, numKeys)
	}
	if counter.max > snapshotStreamBufferSize {
		t.Fatalf("largest write = %d bytes, want <= buffer size %d", counter.max, snapshotStreamBufferSize)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("streaming %d entries took %v, want under 30s", numKeys, elapsed)
	}
}

// TestSnapshotRestoreCompatibility covers the old []byte APIs, the new stream
// APIs and loading a legacy pre-streaming payload.
func TestSnapshotRestoreCompatibility(t *testing.T) {
	shared := openStreamTestEngine(t)
	src, srcEngine := newStreamTestSM(t, shared, 1)
	pairs := []streamKV{
		{key: []byte("alpha"), value: []byte("one")},
		{key: []byte("beta"), value: []byte("two")},
	}
	seedStreamTest(t, srcEngine, pairs)
	src.lastApplied.Store(99)

	data, index, err := src.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if index != 99 {
		t.Fatalf("Snapshot index = %d, want 99", index)
	}
	stream, streamIndex := captureStream(t, src)
	if streamIndex != index || !bytes.Equal(data, stream) {
		t.Fatalf("Snapshot output differs from StreamSnapshot (index %d/%d)", index, streamIndex)
	}

	t.Run("stream restore accepts Snapshot bytes", func(t *testing.T) {
		dst, dstEngine := newStreamTestSM(t, shared, 2)
		if err := dst.RestoreSnapshotStream(bytes.NewReader(data)); err != nil {
			t.Fatalf("RestoreSnapshotStream: %v", err)
		}
		assertStreamContents(t, dstEngine, pairs)
		if got := dst.lastApplied.Load(); got != 99 {
			t.Fatalf("lastApplied = %d, want 99", got)
		}
	})

	t.Run("RestoreSnapshot accepts Snapshot bytes", func(t *testing.T) {
		dst, dstEngine := newStreamTestSM(t, shared, 3)
		seedStreamTest(t, dstEngine, []streamKV{{key: []byte("stale"), value: []byte("old")}})
		if err := dst.RestoreSnapshot(data); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		assertStreamContents(t, dstEngine, pairs)
		if got := dst.lastApplied.Load(); got != 99 {
			t.Fatalf("lastApplied = %d, want 99", got)
		}
	})

	t.Run("RestoreSnapshot accepts StreamSnapshot bytes", func(t *testing.T) {
		dst, dstEngine := newStreamTestSM(t, shared, 4)
		if err := dst.RestoreSnapshot(stream); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		assertStreamContents(t, dstEngine, pairs)
	})

	// Format written by the pre-streaming Snapshot implementation: a single
	// JSON object with "index" and "entries" (base64 k/v).
	legacy := []byte(`{"index":7,"entries":[{"k":"YQ==","v":"MQ=="},{"k":"Yg==","v":"Mg=="}]}`)
	legacyWant := []streamKV{
		{key: []byte("a"), value: []byte("1")},
		{key: []byte("b"), value: []byte("2")},
	}

	t.Run("legacy payload", func(t *testing.T) {
		dst, dstEngine := newStreamTestSM(t, shared, 5)
		seedStreamTest(t, dstEngine, []streamKV{{key: []byte("stale"), value: []byte("old")}})
		if err := dst.RestoreSnapshot(legacy); err != nil {
			t.Fatalf("RestoreSnapshot(legacy): %v", err)
		}
		assertStreamContents(t, dstEngine, legacyWant)
		if got := dst.lastApplied.Load(); got != 7 {
			t.Fatalf("legacy lastApplied = %d, want 7", got)
		}
	})

	t.Run("legacy payload via stream restore", func(t *testing.T) {
		// Raft's startup path restores the persisted data file through
		// RestoreSnapshotStream, so legacy on-disk snapshots must work here too.
		dst, dstEngine := newStreamTestSM(t, shared, 6)
		seedStreamTest(t, dstEngine, []streamKV{{key: []byte("stale"), value: []byte("old")}})
		if err := dst.RestoreSnapshotStream(bytes.NewReader(legacy)); err != nil {
			t.Fatalf("RestoreSnapshotStream(legacy): %v", err)
		}
		assertStreamContents(t, dstEngine, legacyWant)
		if got := dst.lastApplied.Load(); got != 7 {
			t.Fatalf("legacy lastApplied = %d, want 7", got)
		}
	})
}

// TestSnapshotStreamFormat pins down the documented JSON Lines layout and
// binary encoding so the format cannot silently change without a version bump.
func TestSnapshotStreamFormat(t *testing.T) {
	shared := openStreamTestEngine(t)
	sm, engine := newStreamTestSM(t, shared, 1)
	key := []byte{'k', 0x00, 0xff}
	value := []byte{0x00, 0xff, 'v', 0x80}
	seedStreamTest(t, engine, []streamKV{{key: key, value: value}})
	sm.lastApplied.Store(9)

	data, _ := captureStream(t, sm)
	lines := splitStreamLines(t, data)
	if len(lines) != 3 {
		t.Fatalf("stream has %d lines, want 3", len(lines))
	}

	var header map[string]any
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header["format"] != snapshotStreamFormat {
		t.Fatalf("header format = %v, want %q", header["format"], snapshotStreamFormat)
	}
	if header["version"] != float64(snapshotStreamVersion) {
		t.Fatalf("header version = %v, want %d", header["version"], snapshotStreamVersion)
	}
	if header["index"] != float64(9) {
		t.Fatalf("header index = %v, want 9", header["index"])
	}

	var entry map[string]any
	if err := json.Unmarshal(lines[1], &entry); err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	keyB64, ok := entry["k"].(string)
	if !ok {
		t.Fatalf("entry k = %T, want base64 string", entry["k"])
	}
	gotKey, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || !bytes.Equal(gotKey, key) {
		t.Fatalf("decoded key = %x (err %v), want %x", gotKey, err, key)
	}
	valueB64, ok := entry["v"].(string)
	if !ok {
		t.Fatalf("entry v = %T, want base64 string", entry["v"])
	}
	gotValue, err := base64.StdEncoding.DecodeString(valueB64)
	if err != nil || !bytes.Equal(gotValue, value) {
		t.Fatalf("decoded value = %x (err %v), want %x", gotValue, err, value)
	}
	if _, ok := entry["crc"]; !ok {
		t.Fatal("entry has no crc")
	}

	var trailer map[string]any
	if err := json.Unmarshal(lines[2], &trailer); err != nil {
		t.Fatalf("decode trailer: %v", err)
	}
	if trailer["end"] != true || trailer["count"] != float64(1) {
		t.Fatalf("trailer = %v, want end=true count=1", trailer)
	}
	if _, ok := trailer["crc"]; !ok {
		t.Fatal("trailer has no crc")
	}
}
