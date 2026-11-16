package kvserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/raft"
	"distributed-db/internal/storage"
)

// streamHeaderForTest decodes the always-plain first line of a snapshot
// stream.
func streamHeaderForTest(t *testing.T, data []byte) snapshotStreamHeader {
	t.Helper()
	end := bytes.IndexByte(data, '\n')
	if end < 0 {
		t.Fatalf("stream has no header line: %q", data)
	}
	var h snapshotStreamHeader
	if err := json.Unmarshal(data[:end], &h); err != nil {
		t.Fatalf("decode stream header %q: %v", data[:end], err)
	}
	return h
}

// plainHeaderEnd returns the byte offset just after the header line.
func plainHeaderEnd(t *testing.T, data []byte) int {
	t.Helper()
	end := bytes.IndexByte(data, '\n')
	if end < 0 {
		t.Fatalf("stream has no header line: %q", data)
	}
	return end + 1
}

// mutatePlainHeader rewrites only the uncompressed header line and keeps the
// (possibly compressed) body untouched.
func mutatePlainHeader(t *testing.T, data []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	end := plainHeaderEnd(t, data)
	var obj map[string]any
	if err := json.Unmarshal(data[:end], &obj); err != nil {
		t.Fatalf("decode stream header: %v", err)
	}
	mutate(obj)
	encoded, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("encode stream header: %v", err)
	}
	out := append(encoded, '\n')
	return append(out, data[end:]...)
}

func flipStreamByte(data []byte, at int) []byte {
	out := append([]byte(nil), data...)
	out[at] ^= 0xff
	return out
}

// compressiblePairs returns n entries whose repeated values compress well while
// still exceeding the small-stream threshold for realistic sizes.
func compressiblePairs(n int) []streamKV {
	body := bytes.Repeat([]byte("compressible-snapshot-value-"), 24)
	pairs := make([]streamKV, n)
	for i := range pairs {
		value := append([]byte(nil), body...)
		value = append(value, byte(i), byte(i>>8), byte(i>>16))
		pairs[i] = streamKV{
			key:   []byte(fmt.Sprintf("key-%06d", i)),
			value: value,
		}
	}
	return pairs
}

// TestSnapshotStreamCompressedRoundTrip round-trips small and large payloads
// through forced gzip and checks both directions of the compatibility story:
// gzip streams restore through RestoreSnapshotStream and the []byte
// RestoreSnapshot compatibility API, and uncompressed streams still work.
func TestSnapshotStreamCompressedRoundTrip(t *testing.T) {
	smallPairs := []streamKV{
		{key: []byte("alpha"), value: []byte("one")},
		{key: []byte{0x00, 0xff}, value: []byte{}},
		{key: []byte("ключ"), value: []byte{0x00, 0xff, 0x80}},
	}
	cases := []struct {
		name      string
		pairs     []streamKV
		wantRatio bool
	}{
		{name: "empty", pairs: nil},
		{name: "small", pairs: smallPairs},
		{name: "large", pairs: compressiblePairs(2000), wantRatio: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shared := openStreamTestEngine(t)
			src, srcEngine := newStreamTestSM(t, shared, 1)
			seedStreamTest(t, srcEngine, tc.pairs)
			src.lastApplied.Store(321)

			raw, rawIndex := captureStreamWith(t, src, SnapshotStreamOptions{Compression: SnapshotCompressionNone})
			compressed, compressedIndex := captureStreamWith(t, src, SnapshotStreamOptions{Compression: SnapshotCompressionGzip})
			if rawIndex != 321 || compressedIndex != 321 {
				t.Fatalf("indexes = %d/%d, want 321", rawIndex, compressedIndex)
			}
			if h := streamHeaderForTest(t, raw); h.Comp != snapshotCompressionNone {
				t.Fatalf("raw header comp = %q, want %q", h.Comp, snapshotCompressionNone)
			}
			if h := streamHeaderForTest(t, compressed); h.Comp != snapshotCompressionGzip {
				t.Fatalf("compressed header comp = %q, want %q", h.Comp, snapshotCompressionGzip)
			}
			if tc.wantRatio && len(compressed) >= len(raw) {
				t.Fatalf("compressed stream is %d bytes, raw is %d; want compression", len(compressed), len(raw))
			}
			t.Logf("%s: raw=%d compressed=%d ratio=%.2fx", tc.name, len(raw), len(compressed), float64(len(raw))/float64(len(compressed)))

			// Stream restore.
			dst, dstEngine := newStreamTestSM(t, shared, 2)
			dst.lastApplied.Store(1)
			if err := dst.RestoreSnapshotStream(bytes.NewReader(compressed)); err != nil {
				t.Fatalf("RestoreSnapshotStream(compressed): %v", err)
			}
			if got := dst.lastApplied.Load(); got != 321 {
				t.Fatalf("restored lastApplied = %d, want 321", got)
			}
			assertStreamContents(t, dstEngine, tc.pairs)

			// []byte compatibility restore.
			dstCompat, dstCompatEngine := newStreamTestSM(t, shared, 3)
			if err := dstCompat.RestoreSnapshot(compressed); err != nil {
				t.Fatalf("RestoreSnapshot(compressed): %v", err)
			}
			assertStreamContents(t, dstCompatEngine, tc.pairs)
		})
	}
}

// TestSnapshotStreamCompressedThroughFileStore verifies the opaque-store
// contract: compressed bytes pass through raft.FileStore unchanged, its Size
// and CRC32 describe the stored (compressed) payload, and the restored bytes
// still decode through RestoreSnapshotStream.
func TestSnapshotStreamCompressedThroughFileStore(t *testing.T) {
	shared := openStreamTestEngine(t)
	src, srcEngine := newStreamTestSM(t, shared, 1)
	pairs := compressiblePairs(300)
	seedStreamTest(t, srcEngine, pairs)
	src.lastApplied.Store(42)

	compressed, _ := captureStreamWith(t, src, SnapshotStreamOptions{Compression: SnapshotCompressionGzip})

	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.SaveSnapshotFromReader(42, 7, bytes.NewReader(compressed)); err != nil {
		t.Fatalf("SaveSnapshotFromReader: %v", err)
	}
	meta, rc, err := store.OpenSnapshot()
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	stored, err := io.ReadAll(rc)
	if err != nil {
		_ = rc.Close()
		t.Fatalf("read stored snapshot: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close stored snapshot: %v", err)
	}
	if meta.Index != 42 || meta.Term != 7 {
		t.Fatalf("meta = {%d,%d}, want {42,7}", meta.Index, meta.Term)
	}
	if int64(len(stored)) != meta.Size {
		t.Fatalf("stored size = %d, meta.Size = %d", len(stored), meta.Size)
	}
	if got := crc32.ChecksumIEEE(stored); got != meta.CRC32 {
		t.Fatalf("stored CRC32 = %08x, meta.CRC32 = %08x", got, meta.CRC32)
	}
	if !bytes.Equal(stored, compressed) {
		t.Fatal("FileStore did not preserve compressed bytes verbatim")
	}

	dst, dstEngine := newStreamTestSM(t, shared, 2)
	if err := dst.RestoreSnapshotStream(bytes.NewReader(stored)); err != nil {
		t.Fatalf("RestoreSnapshotStream(stored compressed): %v", err)
	}
	assertStreamContents(t, dstEngine, pairs)
	if got := dst.lastApplied.Load(); got != 42 {
		t.Fatalf("restored lastApplied = %d, want 42", got)
	}
}

// TestSnapshotStreamRestoreLegacyUncompressed pins legacy read compatibility:
// stream headers written before the comp member (and explicit "none" headers)
// both restore through the streaming entry point.
func TestSnapshotStreamRestoreLegacyUncompressed(t *testing.T) {
	entries := []streamKV{
		{key: []byte{0x00, 0xff}, value: []byte{0x80}},
		{key: []byte("a"), value: []byte("1")},
		{key: []byte("b"), value: []byte("2")},
	}
	noComp := encodeStreamForTest(t, 12, entries)

	var header map[string]any
	if err := json.Unmarshal(bytes.SplitN(noComp, []byte("\n"), 2)[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if _, ok := header["comp"]; ok {
		t.Fatalf("pre-compression header unexpectedly has comp: %v", header)
	}

	explicitNone := mutatePlainHeader(t, noComp, func(m map[string]any) { m["comp"] = snapshotCompressionNone })

	cases := []struct {
		name string
		data []byte
	}{
		{name: "header without comp", data: noComp},
		{name: "header with comp none", data: explicitNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shared := openStreamTestEngine(t)
			dst, dstEngine := newStreamTestSM(t, shared, 4)
			seedStreamTest(t, dstEngine, []streamKV{{key: []byte("stale"), value: []byte("value")}})
			if err := dst.RestoreSnapshotStream(bytes.NewReader(tc.data)); err != nil {
				t.Fatalf("RestoreSnapshotStream: %v", err)
			}
			assertStreamContents(t, dstEngine, entries)
			if got := dst.lastApplied.Load(); got != 12 {
				t.Fatalf("lastApplied = %d, want 12", got)
			}
		})
	}
}

// TestSnapshotStreamRejectsCorruptCompression covers truncated gzip members,
// bad gzip headers/checksums, unsupported comp values and trailing bytes. A
// failure to decode the gzip header must not destroy the existing keyspace;
// failures after the payload starts are documented as destructive.
func TestSnapshotStreamRejectsCorruptCompression(t *testing.T) {
	shared := openStreamTestEngine(t)
	src, srcEngine := newStreamTestSM(t, shared, 1)
	seedStreamTest(t, srcEngine, compressiblePairs(500))
	src.lastApplied.Store(55)

	compressed, _ := captureStreamWith(t, src, SnapshotStreamOptions{Compression: SnapshotCompressionGzip})
	raw, _ := captureStreamWith(t, src, SnapshotStreamOptions{Compression: SnapshotCompressionNone})
	headerEnd := plainHeaderEnd(t, compressed)
	if headerEnd >= len(compressed) {
		t.Fatal("compressed stream has no body")
	}

	cases := []struct {
		name        string
		data        []byte
		want        error
		destructive bool
	}{
		{
			name: "truncated after header",
			data: compressed[:headerEnd],
			want: ErrSnapshotStreamTruncated,
		},
		{
			name:        "truncated mid body",
			data:        compressed[:headerEnd+(len(compressed)-headerEnd)/2],
			want:        ErrSnapshotStreamTruncated,
			destructive: true,
		},
		{
			name:        "truncated gzip footer",
			data:        compressed[:len(compressed)-4],
			want:        ErrSnapshotStreamTruncated,
			destructive: true,
		},
		{
			name: "corrupt gzip magic",
			data: flipStreamByte(compressed, headerEnd),
			want: ErrSnapshotStreamCorrupt,
		},
		{
			name:        "corrupt gzip checksum",
			data:        flipStreamByte(compressed, len(compressed)-5),
			want:        ErrSnapshotStreamCorrupt,
			destructive: true,
		},
		{
			name:        "trailing bytes after gzip member",
			data:        append(append([]byte(nil), compressed...), []byte("trailing-junk")...),
			want:        ErrSnapshotStreamCorrupt,
			destructive: true,
		},
		{
			name: "unsupported compression mode",
			data: mutatePlainHeader(t, compressed, func(m map[string]any) { m["comp"] = "zstd" }),
			want: ErrSnapshotStreamCorrupt,
		},
		{
			name: "gzip declared over raw body",
			data: mutatePlainHeader(t, raw, func(m map[string]any) { m["comp"] = snapshotCompressionGzip }),
			want: ErrSnapshotStreamCorrupt,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst, dstEngine := newStreamTestSM(t, openStreamTestEngine(t), 2)
			seedStreamTest(t, dstEngine, []streamKV{{key: []byte("sentinel"), value: []byte("value")}})

			err := dst.RestoreSnapshotStream(bytes.NewReader(tc.data))
			if err == nil {
				t.Fatal("RestoreSnapshotStream succeeded, want error")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, tc.want)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "compress") {
				t.Fatalf("error %q does not mention compression", err)
			}
			if got := dst.lastApplied.Load(); got != 0 {
				t.Fatalf("lastApplied = %d after failed restore, want 0", got)
			}
			_, getErr := dstEngine.Get([]byte("sentinel"))
			if tc.destructive {
				if !errors.Is(getErr, storage.ErrNotFound) {
					t.Fatalf("sentinel survived a post-header error (err=%v)", getErr)
				}
			} else if getErr != nil {
				t.Fatalf("compression header error destroyed the existing keyspace: %v", getErr)
			}
		})
	}
}

// TestSnapshotStreamAutoCompression pins the auto policy: below the threshold
// the output is exactly the uncompressed representation; at or above it the
// body is gzip. It also checks that StreamSnapshot (the production entry
// point) uses auto.
func TestSnapshotStreamAutoCompression(t *testing.T) {
	const threshold = 4 << 10

	t.Run("small stays uncompressed", func(t *testing.T) {
		shared := openStreamTestEngine(t)
		src, srcEngine := newStreamTestSM(t, shared, 1)
		seedStreamTest(t, srcEngine, []streamKV{
			{key: []byte("a"), value: []byte("1")},
			{key: []byte("b"), value: []byte("2")},
		})
		src.lastApplied.Store(5)

		auto, _ := captureStreamWith(t, src, SnapshotStreamOptions{
			Compression:           SnapshotCompressionAuto,
			AutoCompressThreshold: threshold,
		})
		none, _ := captureStreamWith(t, src, SnapshotStreamOptions{Compression: SnapshotCompressionNone})
		if h := streamHeaderForTest(t, auto); h.Comp != snapshotCompressionNone {
			t.Fatalf("auto header comp = %q, want %q", h.Comp, snapshotCompressionNone)
		}
		if !bytes.Equal(auto, none) {
			t.Fatalf("auto output differs from uncompressed output below the threshold (%d vs %d bytes)", len(auto), len(none))
		}

		dst, dstEngine := newStreamTestSM(t, shared, 2)
		if err := dst.RestoreSnapshotStream(bytes.NewReader(auto)); err != nil {
			t.Fatalf("RestoreSnapshotStream: %v", err)
		}
		assertStreamContents(t, dstEngine, []streamKV{
			{key: []byte("a"), value: []byte("1")},
			{key: []byte("b"), value: []byte("2")},
		})
	})

	t.Run("large compresses", func(t *testing.T) {
		shared := openStreamTestEngine(t)
		src, srcEngine := newStreamTestSM(t, shared, 1)
		pairs := compressiblePairs(200)
		seedStreamTest(t, srcEngine, pairs)
		src.lastApplied.Store(6)

		auto, _ := captureStreamWith(t, src, SnapshotStreamOptions{
			Compression:           SnapshotCompressionAuto,
			AutoCompressThreshold: threshold,
		})
		none, _ := captureStreamWith(t, src, SnapshotStreamOptions{Compression: SnapshotCompressionNone})
		if h := streamHeaderForTest(t, auto); h.Comp != snapshotCompressionGzip {
			t.Fatalf("auto header comp = %q, want %q above the threshold", h.Comp, snapshotCompressionGzip)
		}
		if len(auto) >= len(none) {
			t.Fatalf("auto output %d bytes is not smaller than uncompressed %d bytes", len(auto), len(none))
		}

		dst, dstEngine := newStreamTestSM(t, shared, 2)
		if err := dst.RestoreSnapshotStream(bytes.NewReader(auto)); err != nil {
			t.Fatalf("RestoreSnapshotStream: %v", err)
		}
		assertStreamContents(t, dstEngine, pairs)
	})

	t.Run("StreamSnapshot defaults to auto", func(t *testing.T) {
		shared := openStreamTestEngine(t)
		src, srcEngine := newStreamTestSM(t, shared, 1)
		pairs := compressiblePairs(2000)
		seedStreamTest(t, srcEngine, pairs)
		src.lastApplied.Store(7)

		var buf bytes.Buffer
		index, err := src.StreamSnapshot(&buf)
		if err != nil {
			t.Fatalf("StreamSnapshot: %v", err)
		}
		if index != 7 {
			t.Fatalf("StreamSnapshot index = %d, want 7", index)
		}
		if h := streamHeaderForTest(t, buf.Bytes()); h.Comp != snapshotCompressionGzip {
			t.Fatalf("default header comp = %q, want %q for a stream above the default threshold", h.Comp, snapshotCompressionGzip)
		}

		dst, dstEngine := newStreamTestSM(t, shared, 2)
		if err := dst.RestoreSnapshotStream(bytes.NewReader(buf.Bytes())); err != nil {
			t.Fatalf("RestoreSnapshotStream: %v", err)
		}
		assertStreamContents(t, dstEngine, pairs)
	})

	t.Run("negative threshold rejected", func(t *testing.T) {
		shared := openStreamTestEngine(t)
		src, _ := newStreamTestSM(t, shared, 1)
		if _, err := src.StreamSnapshotWithOptions(io.Discard, SnapshotStreamOptions{
			Compression:           SnapshotCompressionAuto,
			AutoCompressThreshold: -1,
		}); err == nil {
			t.Fatal("StreamSnapshotWithOptions accepted a negative threshold")
		}
	})

	t.Run("unknown mode rejected", func(t *testing.T) {
		shared := openStreamTestEngine(t)
		src, _ := newStreamTestSM(t, shared, 1)
		if _, err := src.StreamSnapshotWithOptions(io.Discard, SnapshotStreamOptions{
			Compression: SnapshotCompression("zstd"),
		}); err == nil {
			t.Fatal("StreamSnapshotWithOptions accepted an unsupported compression mode")
		}
	})
}

// TestSnapshotStreamCompressedWritesIncrementally verifies that gzip output
// still goes through the bounded buffered writer in multiple writes rather
// than accumulating the whole compressed snapshot.
func TestSnapshotStreamCompressedWritesIncrementally(t *testing.T) {
	shared := openStreamTestEngine(t)
	src, srcEngine := newStreamTestSM(t, shared, 1)
	pairs := compressiblePairs(2000)
	seedStreamTest(t, srcEngine, pairs)
	src.lastApplied.Store(88)

	raw, _ := captureStreamWith(t, src, SnapshotStreamOptions{Compression: SnapshotCompressionNone})

	var counter countingDiscardWriter
	writer := io.MultiWriter(io.Discard, &counter)
	start := time.Now()
	index, err := src.StreamSnapshot(writer)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("StreamSnapshot: %v", err)
	}
	if index != 88 {
		t.Fatalf("StreamSnapshot index = %d, want 88", index)
	}
	if counter.calls < 2 {
		t.Fatalf("compressed stream used %d write call(s), want incremental flushes", counter.calls)
	}
	if counter.max > snapshotStreamBufferSize {
		t.Fatalf("largest compressed write = %d bytes, want <= %d", counter.max, snapshotStreamBufferSize)
	}
	if counter.total >= len(raw) {
		t.Fatalf("compressed stream wrote %d bytes, raw stream is %d; want compression", counter.total, len(raw))
	}
	if elapsed > 30*time.Second {
		t.Fatalf("compressed streaming took %v, want under 30s", elapsed)
	}
}

// BenchmarkSnapshotStreamCompression reports gzip throughput and the size
// ratio against the uncompressed stream for a moderately large, realistic
// snapshot (repetitive NDJSON/base64 values).
func BenchmarkSnapshotStreamCompression(b *testing.B) {
	shared := openStreamTestEngine(b)
	sm, engine := newStreamTestSM(b, shared, 1)
	seedStreamTest(b, engine, compressiblePairs(2000))
	sm.lastApplied.Store(1)

	var raw bytes.Buffer
	if _, err := sm.StreamSnapshotWithOptions(&raw, SnapshotStreamOptions{Compression: SnapshotCompressionNone}); err != nil {
		b.Fatalf("uncompressed snapshot: %v", err)
	}
	var compressed bytes.Buffer
	if _, err := sm.StreamSnapshotWithOptions(&compressed, SnapshotStreamOptions{Compression: SnapshotCompressionGzip}); err != nil {
		b.Fatalf("compressed snapshot: %v", err)
	}
	ratio := float64(raw.Len()) / float64(compressed.Len())

	b.Run("none", func(b *testing.B) {
		b.SetBytes(int64(raw.Len()))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := sm.StreamSnapshotWithOptions(io.Discard, SnapshotStreamOptions{Compression: SnapshotCompressionNone}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("gzip", func(b *testing.B) {
		b.SetBytes(int64(raw.Len()))
		b.ReportAllocs()
		b.ReportMetric(ratio, "ratio")
		for i := 0; i < b.N; i++ {
			if _, err := sm.StreamSnapshotWithOptions(io.Discard, SnapshotStreamOptions{Compression: SnapshotCompressionGzip}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
