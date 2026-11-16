package kvserver

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"distributed-db/internal/storage"
)

// Snapshot stream format (version 1)
//
// A snapshot stream is JSON Lines (NDJSON): UTF-8 JSON objects, one per line,
// each terminated by a single LF. Binary keys and values are carried as padded
// standard base64 strings by encoding/json, so they round-trip byte for byte
// even when they contain NUL, 0xFF or invalid UTF-8.
//
//	header:  {"format":"ddb-kv-snapshot","version":1,"index":123,"comp":"none"}
//	entry:   {"k":"<base64>","v":"<base64>","crc":123456789}
//	...
//	trailer: {"end":true,"count":42,"crc":987654321}
//
// The header is mandatory and must be the first record, and it is always
// uncompressed NDJSON so a reader can identify the stream before decoding its
// body. The optional "comp" member selects the encoding of everything after
// the header:
//
//	"none" (or absent)  entry/trailer records follow as plain NDJSON.
//	"gzip"              all entry/trailer records form one gzip member
//	                    (RFC 1952) directly after the header line. The
//	                    gzip trailer carries its own CRC-32 and length of the
//	                    uncompressed logical records.
//
// A stream written before compression support has no "comp" member and is
// therefore read as "none" (legacy compatibility). An unknown "comp" value is
// rejected as corrupt. Compressed streams are not readable by binaries that
// predate the "comp" member; readers must be upgraded before writers enable
// compression. Small streams and the auto mode's size threshold keep legacy
// compatibility for the common small-snapshot case when auto is used.
//
// index is the last applied Raft index reflected in the stream; it is required
// even when it is zero. Entries are emitted in strictly ascending key order and
// restore rejects unsorted or duplicate keys.
//
// The entry crc is CRC-32/IEEE over uvarint(len(key)) || key || value; the
// length prefix removes the key/value boundary ambiguity. The trailer crc is
// CRC-32/IEEE over the concatenation of the big-endian 4-byte entry CRC of
// every entry, in stream order, and count is the total number of entries.
// These checksums always cover the uncompressed logical records, whether or
// not the body is compressed. The gzip member independently covers its
// decompressed bytes, and Raft's SnapshotMeta CRC32 covers the stored payload
// bytes (the compressed bytes when compression is enabled).
//
// A truncated stream is reported as ErrSnapshotStreamTruncated and a
// malformed stream (bad header, bad JSON, checksum/order mismatch, missing
// trailer, trailing bytes, or a corrupt/truncated gzip body) as
// ErrSnapshotStreamCorrupt. The checksums detect accidental corruption only;
// they provide no cryptographic authentication.
const (
	snapshotStreamFormat  = "ddb-kv-snapshot"
	snapshotStreamVersion = 1

	// snapshotStreamBufferSize bounds the buffered writer and reader. A single
	// record (one key/value pair) may be larger; only whole-range buffering is
	// avoided.
	snapshotStreamBufferSize = 64 << 10
	// snapshotStreamFlushEvery asks the writer to flush at least this often so
	// a slow consumer sees progress without a full snapshot in RAM. For a
	// compressed stream this also emits a gzip flush point.
	snapshotStreamFlushEvery = 256

	// Compression identifiers stored in the header. "none" is also the
	// implicit value of legacy headers that predate the "comp" member.
	snapshotCompressionNone = "none"
	snapshotCompressionGzip = "gzip"

	// snapshotAutoCompressThreshold is the default number of uncompressed
	// logical bytes at which SnapshotCompressionAuto switches to gzip. Below
	// it the stream stays uncompressed: gzip's fixed header/footer usually
	// costs more than it saves, and the plain format is directly inspectable.
	// The writer buffers at most this much plus one record before deciding.
	snapshotAutoCompressThreshold = 1 << 20

	// snapshotGzipLevel balances snapshot write CPU against transfer size.
	snapshotGzipLevel = gzip.DefaultCompression

	// Restore staging limits: a batch is committed after this many mutations
	// or once it reaches this many logical bytes, whichever comes first.
	snapshotBatchEntries = 1024
	snapshotBatchBytes   = 4 << 20
)

// SnapshotCompression selects the body encoding of a snapshot stream. The
// header is always plain NDJSON; the mode only affects the entry and trailer
// records that follow it.
type SnapshotCompression string

const (
	// SnapshotCompressionAuto is used by StreamSnapshot. The body is gzip
	// compressed only when its uncompressed logical size reaches the auto
	// threshold (1 MiB by default), otherwise it stays uncompressed.
	SnapshotCompressionAuto SnapshotCompression = "auto"
	// SnapshotCompressionNone writes the legacy uncompressed wire format.
	SnapshotCompressionNone SnapshotCompression = "none"
	// SnapshotCompressionGzip forces gzip compression, even for tiny streams.
	SnapshotCompressionGzip SnapshotCompression = "gzip"
)

// SnapshotStreamOptions controls StreamSnapshotWithOptions.
type SnapshotStreamOptions struct {
	// Compression defaults to SnapshotCompressionAuto when empty.
	Compression SnapshotCompression
	// AutoCompressThreshold overrides the uncompressed-byte threshold used by
	// SnapshotCompressionAuto. Zero uses snapshotAutoCompressThreshold (1 MiB);
	// negative values are rejected. It is ignored by the forced modes.
	AutoCompressThreshold int64
}

var (
	// ErrSnapshotStreamCorrupt reports a structurally invalid or checksum-
	// mismatching snapshot stream.
	ErrSnapshotStreamCorrupt = errors.New("kvserver: corrupt snapshot stream")
	// ErrSnapshotStreamTruncated reports a stream that ended before its
	// mandatory trailer (or in the middle of a record or header).
	ErrSnapshotStreamTruncated = errors.New("kvserver: truncated snapshot stream")
)

// snapshotStreamHeader is the first record of a v1 snapshot stream. Comp is
// empty in headers written before compression support and otherwise holds
// snapshotCompressionNone or snapshotCompressionGzip.
type snapshotStreamHeader struct {
	Format  string  `json:"format"`
	Version int     `json:"version"`
	Index   *uint64 `json:"index"`
	Comp    string  `json:"comp,omitempty"`
}

// snapshotStreamEntry is one key/value record.
type snapshotStreamEntry struct {
	Key   []byte `json:"k"`
	Value []byte `json:"v"`
	CRC   uint32 `json:"crc"`
}

// snapshotStreamTrailer is the final record of a v1 snapshot stream.
type snapshotStreamTrailer struct {
	End   bool   `json:"end"`
	Count uint64 `json:"count"`
	CRC   uint32 `json:"crc"`
}

// snapshotStreamRecord is the union used by the reader: a record is a trailer
// when it carries "end":true and an entry otherwise.
type snapshotStreamRecord struct {
	Key   []byte `json:"k"`
	Value []byte `json:"v"`
	CRC   uint32 `json:"crc"`
	End   bool   `json:"end"`
	Count uint64 `json:"count"`
}

// StreamSnapshot writes a self-describing snapshot stream for this range and
// returns the last applied index. It uses a Pebble snapshot iterator and never
// materializes all entries: memory is bounded by one record, the fixed
// buffered writer, and (in auto mode) the auto-compression threshold plus at
// most one record held while choosing the body encoding.
//
// It uses SnapshotCompressionAuto: bodies below the auto threshold stay
// uncompressed and larger bodies are gzip compressed. Use
// StreamSnapshotWithOptions to force a mode.
//
// The stream format is documented above. StreamSnapshot does not take a lock:
// like the rest of the state machine it relies on Raft serializing Apply and
// snapshotting. Writes to this range must not run concurrently with it.
//
// A write error leaves a partial stream at the writer; the caller must discard
// it. The returned index is only meaningful when err is nil.
func (m *stateMachine) StreamSnapshot(w io.Writer) (index uint64, err error) {
	return m.StreamSnapshotWithOptions(w, SnapshotStreamOptions{Compression: SnapshotCompressionAuto})
}

// StreamSnapshotWithOptions is StreamSnapshot with an explicit body encoding.
// The header is always plain NDJSON; when compression is selected, all entry
// and trailer records follow it as one gzip member.
//
// In SnapshotCompressionAuto mode the header is withheld until the logical
// stream reaches the threshold, because the header records the chosen
// encoding. A stream smaller than the threshold is therefore emitted as
// "none" and is byte-for-byte the legacy format (apart from the explicit
// "comp":"none" member). The buffered prefix is bounded by the threshold plus
// at most one record.
func (m *stateMachine) StreamSnapshotWithOptions(w io.Writer, opts SnapshotStreamOptions) (index uint64, err error) {
	if w == nil {
		return 0, errors.New("kvserver: stream snapshot: nil writer")
	}
	if m.engine == nil {
		return 0, errors.New("kvserver: stream snapshot: no storage engine")
	}
	mode, err := normalizeSnapshotCompression(opts.Compression)
	if err != nil {
		return 0, err
	}
	threshold := opts.AutoCompressThreshold
	if threshold < 0 {
		return 0, fmt.Errorf("kvserver: stream snapshot: negative auto compression threshold %d", threshold)
	}
	if threshold == 0 {
		threshold = snapshotAutoCompressThreshold
	}

	snap, err := m.engine.Snapshot()
	if err != nil {
		return 0, fmt.Errorf("kvserver: stream snapshot: create engine snapshot: %w", err)
	}
	defer snap.Close()

	it, err := snap.NewIter(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("kvserver: stream snapshot: create iterator: %w", err)
	}
	defer it.Close()

	// Capture the applied index alongside the engine snapshot. Raft serializes
	// Apply with snapshotting, so index describes exactly the iterated state.
	index = m.lastApplied.Load()

	headerIndex := index
	sw := newSnapshotStreamWriter(w, snapshotStreamHeader{
		Format:  snapshotStreamFormat,
		Version: snapshotStreamVersion,
		Index:   &headerIndex,
	}, mode, threshold)
	// Forced modes know the header immediately; auto mode writes it when the
	// threshold is crossed (or at Finish for a small stream).
	if err := sw.startForced(); err != nil {
		return 0, fmt.Errorf("kvserver: stream snapshot: write header: %w", err)
	}

	digest := crc32.NewIEEE()
	var crcBuf [4]byte
	var count uint64
	for it.Valid() {
		key, value := it.Key(), it.Value()
		crc := snapshotEntryCRC(key, value)
		if err := writeSnapshotJSON(sw, snapshotStreamEntry{
			Key:   snapshotNonNull(key),
			Value: snapshotNonNull(value),
			CRC:   crc,
		}); err != nil {
			return 0, fmt.Errorf("kvserver: stream snapshot: write entry %d: %w", count, err)
		}
		binary.BigEndian.PutUint32(crcBuf[:], crc)
		digest.Write(crcBuf[:])
		count++
		if count%snapshotStreamFlushEvery == 0 {
			if err := sw.flush(); err != nil {
				return 0, fmt.Errorf("kvserver: stream snapshot: flush after entry %d: %w", count, err)
			}
		}
		it.Next()
	}
	if err := it.Error(); err != nil {
		return 0, fmt.Errorf("kvserver: stream snapshot: iterate: %w", err)
	}

	if err := writeSnapshotJSON(sw, snapshotStreamTrailer{
		End:   true,
		Count: count,
		CRC:   digest.Sum32(),
	}); err != nil {
		return 0, fmt.Errorf("kvserver: stream snapshot: write trailer: %w", err)
	}
	if err := sw.finish(); err != nil {
		return 0, fmt.Errorf("kvserver: stream snapshot: flush: %w", err)
	}
	return index, nil
}

// normalizeSnapshotCompression maps the zero value to auto and rejects modes
// that this implementation does not write.
func normalizeSnapshotCompression(c SnapshotCompression) (SnapshotCompression, error) {
	switch c {
	case "", SnapshotCompressionAuto:
		return SnapshotCompressionAuto, nil
	case SnapshotCompressionNone:
		return SnapshotCompressionNone, nil
	case SnapshotCompressionGzip:
		return SnapshotCompressionGzip, nil
	default:
		return "", fmt.Errorf("kvserver: unsupported snapshot compression %q", c)
	}
}

// snapshotStreamWriter writes the plain header plus the entry/trailer records
// with the selected body encoding. In auto mode it buffers the logical records
// until the threshold is reached, then commits to an encoding and replays the
// buffered prefix; forced modes write the header on the first record.
//
// It implements io.Writer so callers can keep using writeSnapshotJSON.
type snapshotStreamWriter struct {
	dst       io.Writer
	header    snapshotStreamHeader
	mode      SnapshotCompression
	threshold int64

	// pending holds logical bytes buffered while auto mode is undecided. Once
	// started it is always empty.
	pending bytes.Buffer

	started bool
	bw      *bufio.Writer
	gz      *gzip.Writer
	out     io.Writer
}

func newSnapshotStreamWriter(dst io.Writer, header snapshotStreamHeader, mode SnapshotCompression, threshold int64) *snapshotStreamWriter {
	return &snapshotStreamWriter{
		dst:       dst,
		header:    header,
		mode:      mode,
		threshold: threshold,
	}
}

// startForced writes the header immediately for a forced mode. Auto mode must
// wait for the size decision, so it is a no-op.
func (s *snapshotStreamWriter) startForced() error {
	if s.mode == SnapshotCompressionAuto {
		return nil
	}
	return s.start(s.mode)
}

// Write appends one serialized record. In auto mode the record is buffered
// until the cumulative logical size reaches the threshold; crossing it selects
// gzip and replays the buffered prefix.
func (s *snapshotStreamWriter) Write(p []byte) (int, error) {
	if !s.started {
		if s.mode != SnapshotCompressionAuto {
			if err := s.start(s.mode); err != nil {
				return 0, err
			}
		} else {
			if _, err := s.pending.Write(p); err != nil {
				return 0, err
			}
			if int64(s.pending.Len()) < s.threshold {
				return len(p), nil
			}
			if err := s.start(SnapshotCompressionGzip); err != nil {
				return 0, err
			}
			if err := s.flushPending(); err != nil {
				return 0, err
			}
			return len(p), nil
		}
	}
	n, err := s.out.Write(p)
	if err != nil {
		return n, err
	}
	return len(p), nil
}

// start writes the header with the chosen encoding and opens the body writer.
func (s *snapshotStreamWriter) start(comp SnapshotCompression) error {
	if s.started {
		return nil
	}
	header := s.header
	header.Comp = string(comp)
	s.bw = bufio.NewWriterSize(s.dst, snapshotStreamBufferSize)
	if err := writeSnapshotJSON(s.bw, header); err != nil {
		return err
	}
	if comp == SnapshotCompressionGzip {
		zw, err := gzip.NewWriterLevel(s.bw, snapshotGzipLevel)
		if err != nil {
			return err
		}
		s.gz = zw
		s.out = zw
	} else {
		s.out = s.bw
	}
	s.started = true
	return nil
}

// flushPending writes the buffered auto-mode prefix into the started body.
func (s *snapshotStreamWriter) flushPending() error {
	if s.pending.Len() == 0 {
		return nil
	}
	if _, err := s.out.Write(s.pending.Bytes()); err != nil {
		return err
	}
	s.pending.Reset()
	return nil
}

// flush makes the bytes written so far available to the consumer. For gzip it
// emits a sync flush point first.
func (s *snapshotStreamWriter) flush() error {
	if s.gz != nil {
		if err := s.gz.Flush(); err != nil {
			return err
		}
	}
	if s.bw != nil {
		return s.bw.Flush()
	}
	return nil
}

// finish resolves a still-undecided auto stream as uncompressed, closes the
// gzip member (which writes its checksum and length), and flushes the buffered
// writer. It does not close the caller's writer.
func (s *snapshotStreamWriter) finish() error {
	if !s.started {
		comp := SnapshotCompressionNone
		if s.mode == SnapshotCompressionGzip {
			comp = SnapshotCompressionGzip
		}
		if err := s.start(comp); err != nil {
			return err
		}
		if err := s.flushPending(); err != nil {
			return err
		}
	}
	if s.gz != nil {
		if err := s.gz.Close(); err != nil {
			return err
		}
		s.gz = nil
	}
	if s.bw != nil {
		return s.bw.Flush()
	}
	return nil
}

// RestoreSnapshotStream reads a stream produced by StreamSnapshot and replaces
// this range's keyspace in bounded batches. The header's "comp" member is
// honoured transparently: legacy uncompressed streams (with no "comp" member),
// explicit "none" streams and "gzip" streams are all accepted.
//
// For compatibility with snapshots persisted by releases that predate the
// stream format, the reader may instead hold the legacy single-JSON-object
// payload ({"index":...,"entries":...}). That payload is recognised by its
// leading member and buffered in memory, exactly like the old implementation;
// current snapshots always stream.
//
// The header is fully validated before anything is deleted, so a bad header,
// an unsupported compression mode, or a corrupt/truncated gzip header leaves
// the existing keyspace untouched. Existing keys are then deleted in batches
// and streamed keys/values are inserted in batches of at most
// snapshotBatchEntries records or snapshotBatchBytes bytes (a single oversized
// record is allowed on its own). The last applied index is updated only after
// the trailer has been verified.
//
// The restore is not atomic. If the stream fails after the delete phase starts
// (truncated mid-stream, corrupt compression, checksum mismatch, storage
// error) the range is left with a mix of the old and new records, and this
// method returns an error describing the failure. The range must be treated as
// damaged: retry with a complete stream (replaying a valid stream is
// idempotent, because it begins by deleting the range) or reinstall the range
// from a peer. A successful restore is complete and idempotent.
//
// RestoreSnapshotStream does not take a lock: Raft must serialize it with
// Apply, as it does for Snapshot/RestoreSnapshot.
func (m *stateMachine) RestoreSnapshotStream(r io.Reader) error {
	if r == nil {
		return errors.New("kvserver: restore snapshot stream: nil reader")
	}
	if m.engine == nil {
		return errors.New("kvserver: restore snapshot stream: no storage engine")
	}

	br := bufio.NewReaderSize(r, snapshotStreamBufferSize)
	if looksLikeLegacySnapshot(br) {
		data, err := io.ReadAll(br)
		if err != nil {
			return fmt.Errorf("kvserver: restore snapshot stream: read legacy snapshot: %w", err)
		}
		return m.restoreLegacySnapshot(data)
	}

	header, err := readSnapshotHeader(br)
	if err != nil {
		return err
	}

	// Open the compressed body before deleting anything: a corrupt gzip header
	// is still a header error and must leave the existing keyspace untouched.
	records, gz, err := snapshotRecordReader(br, header.Comp)
	if err != nil {
		return err
	}
	if gz != nil {
		defer func() { _ = gz.Close() }()
	}

	// Header and body encoding are valid: from here on the old keyspace is
	// destroyed, so every later error leaves a partial restore (documented
	// above).
	if err := m.deleteSnapshotKeys(); err != nil {
		return fmt.Errorf("kvserver: restore snapshot stream: delete existing keys: %w", err)
	}

	wb := newSnapshotWriteBatch(m.engine)
	defer wb.close()

	digest := crc32.NewIEEE()
	var crcBuf [4]byte
	var count uint64
	var prevKey []byte
	havePrev := false

	for {
		line, eof, err := readSnapshotLine(records)
		if err != nil {
			if gz != nil {
				return snapshotCompressedReadError(err, count)
			}
			return fmt.Errorf("kvserver: restore snapshot stream: read: %w", err)
		}
		if len(line) == 0 {
			if eof {
				return fmt.Errorf("%w: missing trailer after %d entries", ErrSnapshotStreamTruncated, count)
			}
			return fmt.Errorf("%w: empty record after %d entries", ErrSnapshotStreamCorrupt, count)
		}

		var rec snapshotStreamRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			if eof {
				return fmt.Errorf("%w: partial record %d: %v", ErrSnapshotStreamTruncated, count, err)
			}
			return fmt.Errorf("%w: record %d: %v", ErrSnapshotStreamCorrupt, count, err)
		}

		if rec.End {
			if len(rec.Key) != 0 || len(rec.Value) != 0 {
				return fmt.Errorf("%w: trailer carries key/value data", ErrSnapshotStreamCorrupt)
			}
			if rec.Count != count {
				return fmt.Errorf("%w: trailer count %d, restored %d", ErrSnapshotStreamCorrupt, rec.Count, count)
			}
			if want := digest.Sum32(); rec.CRC != want {
				return fmt.Errorf("%w: trailer checksum %08x, want %08x", ErrSnapshotStreamCorrupt, rec.CRC, want)
			}
			if extra, perr := records.Peek(1); perr != nil && !errors.Is(perr, io.EOF) {
				if gz != nil {
					return snapshotCompressedReadError(perr, count)
				}
				return fmt.Errorf("kvserver: restore snapshot stream: read after trailer: %w", perr)
			} else if len(extra) > 0 {
				return fmt.Errorf("%w: trailing data after trailer", ErrSnapshotStreamCorrupt)
			}
			if gz != nil {
				// records.Peek above read the gzip stream through its EOF, so the
				// member checksum has been verified. The raw input must now end at
				// the member boundary: concatenated members or bytes appended after
				// the member are not part of this format.
				if extra, perr := br.Peek(1); perr != nil && !errors.Is(perr, io.EOF) {
					return fmt.Errorf("kvserver: restore snapshot stream: read after compressed stream: %w", perr)
				} else if len(extra) > 0 {
					return fmt.Errorf("%w: trailing data after compressed stream", ErrSnapshotStreamCorrupt)
				}
			}
			if err := wb.commit(); err != nil {
				return fmt.Errorf("kvserver: restore snapshot stream: commit final batch: %w", err)
			}
			m.lastApplied.Store(*header.Index)
			return nil
		}

		if want := snapshotEntryCRC(rec.Key, rec.Value); rec.CRC != want {
			return fmt.Errorf("%w: entry %d checksum %08x, want %08x", ErrSnapshotStreamCorrupt, count, rec.CRC, want)
		}
		if havePrev && bytes.Compare(rec.Key, prevKey) <= 0 {
			return fmt.Errorf("%w: entry %d key is not strictly after the previous key", ErrSnapshotStreamCorrupt, count)
		}
		if err := wb.batch.Set(rec.Key, rec.Value); err != nil {
			return fmt.Errorf("kvserver: restore snapshot stream: stage entry %d: %w", count, err)
		}
		wb.bytes += len(rec.Key) + len(rec.Value)
		prevKey = append(prevKey[:0], rec.Key...)
		havePrev = true
		binary.BigEndian.PutUint32(crcBuf[:], rec.CRC)
		digest.Write(crcBuf[:])
		count++
		if wb.full() {
			if err := wb.commit(); err != nil {
				return fmt.Errorf("kvserver: restore snapshot stream: commit batch: %w", err)
			}
		}
	}
}

// deleteSnapshotKeys removes every key visible through this range's engine in
// batches. The engine iterator is a point-in-time view, so committing deletes
// while walking it does not skip records.
func (m *stateMachine) deleteSnapshotKeys() error {
	it, err := m.engine.NewIter(nil, nil)
	if err != nil {
		return err
	}
	defer it.Close()

	wb := newSnapshotWriteBatch(m.engine)
	defer wb.close()

	for it.Valid() {
		// The iterator buffer is only valid until Next, while the batch may
		// retain the key until Commit.
		key := append([]byte(nil), it.Key()...)
		if err := wb.batch.Delete(key); err != nil {
			return err
		}
		wb.bytes += len(key)
		if wb.full() {
			if err := wb.commit(); err != nil {
				return err
			}
		}
		it.Next()
	}
	if err := it.Error(); err != nil {
		return err
	}
	return wb.commit()
}

// snapshotWriteBatch accumulates mutations up to a record/byte budget and
// commits them as one atomic engine batch.
type snapshotWriteBatch struct {
	engine storage.Engine
	batch  storage.Batch
	bytes  int
}

func newSnapshotWriteBatch(engine storage.Engine) *snapshotWriteBatch {
	return &snapshotWriteBatch{engine: engine, batch: engine.NewBatch()}
}

func (b *snapshotWriteBatch) full() bool {
	return b.batch.Count() >= snapshotBatchEntries || b.bytes >= snapshotBatchBytes
}

// commit applies the staged mutations and starts a fresh batch. A failed
// commit leaves the old batch in place for close to release.
func (b *snapshotWriteBatch) commit() error {
	if b.batch.Count() == 0 {
		return nil
	}
	if err := b.batch.Commit(); err != nil {
		return err
	}
	if err := b.batch.Close(); err != nil {
		return err
	}
	b.batch = b.engine.NewBatch()
	b.bytes = 0
	return nil
}

// close releases the current batch without committing it. It is idempotent.
func (b *snapshotWriteBatch) close() {
	if b == nil || b.batch == nil {
		return
	}
	_ = b.batch.Close()
	b.batch = nil
}

// readSnapshotHeader reads and validates the mandatory header record.
func readSnapshotHeader(br *bufio.Reader) (snapshotStreamHeader, error) {
	var h snapshotStreamHeader
	line, eof, err := readSnapshotLine(br)
	if err != nil {
		return h, fmt.Errorf("kvserver: restore snapshot stream: read header: %w", err)
	}
	if len(line) == 0 {
		return h, fmt.Errorf("%w: empty stream, missing header", ErrSnapshotStreamTruncated)
	}
	if err := json.Unmarshal(line, &h); err != nil {
		if eof {
			return h, fmt.Errorf("%w: partial header: %v", ErrSnapshotStreamTruncated, err)
		}
		return h, fmt.Errorf("%w: header: %v", ErrSnapshotStreamCorrupt, err)
	}
	if h.Format != snapshotStreamFormat {
		return h, fmt.Errorf("%w: header format %q, want %q", ErrSnapshotStreamCorrupt, h.Format, snapshotStreamFormat)
	}
	if h.Version != snapshotStreamVersion {
		return h, fmt.Errorf("%w: unsupported stream version %d, want %d", ErrSnapshotStreamCorrupt, h.Version, snapshotStreamVersion)
	}
	if h.Index == nil {
		return h, fmt.Errorf("%w: header is missing the applied index", ErrSnapshotStreamCorrupt)
	}
	switch h.Comp {
	case "", snapshotCompressionNone, snapshotCompressionGzip:
	default:
		return h, fmt.Errorf("%w: unsupported stream compression %q", ErrSnapshotStreamCorrupt, h.Comp)
	}
	return h, nil
}

// snapshotRecordReader wraps br with the body decoder selected by the header's
// comp member. The returned reader yields the uncompressed NDJSON records. For
// gzip the returned *gzip.Reader has multistream disabled, so a stream must
// contain exactly one member and trailing raw bytes can be reported as
// corruption.
//
// Opening the gzip reader validates its header immediately; that happens
// before RestoreSnapshotStream deletes any keys, so a bad compressed header is
// non-destructive.
func snapshotRecordReader(br *bufio.Reader, comp string) (*bufio.Reader, *gzip.Reader, error) {
	switch comp {
	case "", snapshotCompressionNone:
		return br, nil, nil
	case snapshotCompressionGzip:
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, nil, snapshotCompressionInitError(err)
		}
		gz.Multistream(false)
		return bufio.NewReaderSize(gz, snapshotStreamBufferSize), gz, nil
	default:
		// readSnapshotHeader rejects unknown modes; keep the helper total.
		return nil, nil, fmt.Errorf("%w: unsupported stream compression %q", ErrSnapshotStreamCorrupt, comp)
	}
}

// snapshotCompressionInitError classifies a failure to open the gzip member.
func snapshotCompressionInitError(err error) error {
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return fmt.Errorf("%w: compressed stream header is incomplete: %v", ErrSnapshotStreamTruncated, err)
	case errors.Is(err, gzip.ErrHeader):
		return fmt.Errorf("%w: invalid compressed gzip stream header: %v", ErrSnapshotStreamCorrupt, err)
	}
	var corrupt flate.CorruptInputError
	if errors.As(err, &corrupt) {
		return fmt.Errorf("%w: invalid compressed gzip stream header: %v", ErrSnapshotStreamCorrupt, err)
	}
	return fmt.Errorf("%w: open compressed stream: %v", ErrSnapshotStreamCorrupt, err)
}

// snapshotCompressedReadError classifies a read failure from the gzip body.
// A stream that ends before the gzip member is complete is truncation; failed
// checksums, invalid headers and deflate errors are corruption.
func snapshotCompressedReadError(err error, count uint64) error {
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return fmt.Errorf("%w: compressed stream ended unexpectedly after %d entries: %v", ErrSnapshotStreamTruncated, count, err)
	case errors.Is(err, gzip.ErrChecksum):
		return fmt.Errorf("%w: compressed stream checksum mismatch after %d entries: %v", ErrSnapshotStreamCorrupt, count, err)
	case errors.Is(err, gzip.ErrHeader):
		return fmt.Errorf("%w: invalid compressed stream after %d entries: %v", ErrSnapshotStreamCorrupt, count, err)
	}
	var corrupt flate.CorruptInputError
	if errors.As(err, &corrupt) {
		return fmt.Errorf("%w: corrupt compressed stream after %d entries: %v", ErrSnapshotStreamCorrupt, count, err)
	}
	return fmt.Errorf("%w: compressed stream read after %d entries: %v", ErrSnapshotStreamCorrupt, count, err)
}

// readSnapshotLine returns one line without its trailing LF. eof reports that
// the reader reached EOF while fetching it, so callers can classify a partial
// final record as truncation rather than corruption.
func readSnapshotLine(br *bufio.Reader) (line []byte, eof bool, err error) {
	line, err = br.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	eof = errors.Is(err, io.EOF)
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	return line, eof, nil
}

// writeSnapshotJSON marshals one record and writes it followed by LF. The
// serialized record is the only per-entry allocation beyond the caller's
// key/value buffers; nothing is accumulated across records.
func writeSnapshotJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

// snapshotEntryCRC hashes the length-prefixed key followed by the value.
func snapshotEntryCRC(key, value []byte) uint32 {
	h := crc32.NewIEEE()
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], uint64(len(key)))
	h.Write(buf[:n])
	h.Write(key)
	h.Write(value)
	return h.Sum32()
}

// snapshotNonNull makes an empty key/value encode as a base64 string ("")
// rather than JSON null, keeping the documented format canonical.
func snapshotNonNull(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// legacySnapshotProbeSize is enough bytes to see the opening brace and the
// first member name of a legacy snapshot, even with incidental whitespace.
const legacySnapshotProbeSize = 64

// looksLikeLegacySnapshot reports whether the reader starts with the JSON
// object written by the pre-streaming Snapshot implementation
// ({"index":...,"entries":...}). It peeks; it never consumes input. Only the
// first member is examined, so this is O(1) in the snapshot size, and a
// current stream (whose first member is "format") is never misdetected.
func looksLikeLegacySnapshot(br *bufio.Reader) bool {
	probe, err := br.Peek(legacySnapshotProbeSize)
	if len(probe) == 0 {
		return false
	}
	_ = err // ErrBufferFull and io.EOF both still give usable bytes.

	trimmed := bytes.TrimLeft(probe, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	trimmed = bytes.TrimLeft(trimmed[1:], " \t\r\n")
	return bytes.HasPrefix(trimmed, []byte(`"index"`))
}

// restoreLegacySnapshot reads the single-JSON-object snapshot format used by
// earlier releases and restores it through the streaming path. It exists only
// for snapshots persisted before the stream format, and is O(snapshot size) in
// memory like the old implementation.
func (m *stateMachine) restoreLegacySnapshot(data []byte) error {
	var payload struct {
		Index   uint64 `json:"index"`
		Entries []struct {
			Key   []byte `json:"k"`
			Value []byte `json:"v"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("kvserver: decode legacy snapshot: %w", err)
	}

	var buf bytes.Buffer
	headerIndex := payload.Index
	if err := writeSnapshotJSON(&buf, snapshotStreamHeader{
		Format:  snapshotStreamFormat,
		Version: snapshotStreamVersion,
		Index:   &headerIndex,
	}); err != nil {
		return fmt.Errorf("kvserver: convert legacy snapshot: %w", err)
	}
	digest := crc32.NewIEEE()
	var crcBuf [4]byte
	for _, e := range payload.Entries {
		crc := snapshotEntryCRC(e.Key, e.Value)
		if err := writeSnapshotJSON(&buf, snapshotStreamEntry{
			Key:   snapshotNonNull(e.Key),
			Value: snapshotNonNull(e.Value),
			CRC:   crc,
		}); err != nil {
			return fmt.Errorf("kvserver: convert legacy snapshot: %w", err)
		}
		binary.BigEndian.PutUint32(crcBuf[:], crc)
		digest.Write(crcBuf[:])
	}
	if err := writeSnapshotJSON(&buf, snapshotStreamTrailer{
		End:   true,
		Count: uint64(len(payload.Entries)),
		CRC:   digest.Sum32(),
	}); err != nil {
		return fmt.Errorf("kvserver: convert legacy snapshot: %w", err)
	}

	if err := m.RestoreSnapshotStream(bytes.NewReader(buf.Bytes())); err != nil {
		return fmt.Errorf("kvserver: restore legacy snapshot: %w", err)
	}
	return nil
}
