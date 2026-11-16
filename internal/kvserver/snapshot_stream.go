package kvserver

import (
	"bufio"
	"bytes"
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
//	header:  {"format":"ddb-kv-snapshot","version":1,"index":123}
//	entry:   {"k":"<base64>","v":"<base64>","crc":123456789}
//	...
//	trailer: {"end":true,"count":42,"crc":987654321}
//
// The header is mandatory and must be the first record. index is the last
// applied Raft index reflected in the stream; it is required even when it is
// zero. Entries are emitted in strictly ascending key order and restore
// rejects unsorted or duplicate keys.
//
// The entry crc is CRC-32/IEEE over uvarint(len(key)) || key || value; the
// length prefix removes the key/value boundary ambiguity. The trailer crc is
// CRC-32/IEEE over the concatenation of the big-endian 4-byte entry CRC of
// every entry, in stream order, and count is the total number of entries.
//
// A truncated stream is reported as ErrSnapshotStreamTruncated and a
// malformed stream (bad header, bad JSON, checksum/order mismatch, missing
// trailer, trailing bytes) as ErrSnapshotStreamCorrupt. The checksums detect
// accidental corruption only; they provide no cryptographic authentication.
// The stream is not compressed: compression is left to the transport (gRPC
// compression) or the surrounding snapshot store.
const (
	snapshotStreamFormat  = "ddb-kv-snapshot"
	snapshotStreamVersion = 1

	// snapshotStreamBufferSize bounds the buffered writer and reader. A single
	// record (one key/value pair) may be larger; only whole-range buffering is
	// avoided.
	snapshotStreamBufferSize = 64 << 10
	// snapshotStreamFlushEvery asks the writer to flush at least this often so
	// a slow consumer sees progress without a full snapshot in RAM.
	snapshotStreamFlushEvery = 256

	// Restore staging limits: a batch is committed after this many mutations
	// or once it reaches this many logical bytes, whichever comes first.
	snapshotBatchEntries = 1024
	snapshotBatchBytes   = 4 << 20
)

var (
	// ErrSnapshotStreamCorrupt reports a structurally invalid or checksum-
	// mismatching snapshot stream.
	ErrSnapshotStreamCorrupt = errors.New("kvserver: corrupt snapshot stream")
	// ErrSnapshotStreamTruncated reports a stream that ended before its
	// mandatory trailer (or in the middle of a record or header).
	ErrSnapshotStreamTruncated = errors.New("kvserver: truncated snapshot stream")
)

// snapshotStreamHeader is the first record of a v1 snapshot stream.
type snapshotStreamHeader struct {
	Format  string  `json:"format"`
	Version int     `json:"version"`
	Index   *uint64 `json:"index"`
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
// materializes all entries: memory is bounded by one record plus the fixed
// buffered writer, and the writer is flushed periodically.
//
// The stream format is documented above. StreamSnapshot does not take a lock:
// like the rest of the state machine it relies on Raft serializing Apply and
// snapshotting. Writes to this range must not run concurrently with it.
//
// A write error leaves a partial stream at the writer; the caller must discard
// it. The returned index is only meaningful when err is nil.
func (m *stateMachine) StreamSnapshot(w io.Writer) (index uint64, err error) {
	if w == nil {
		return 0, errors.New("kvserver: stream snapshot: nil writer")
	}
	if m.engine == nil {
		return 0, errors.New("kvserver: stream snapshot: no storage engine")
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

	bw := bufio.NewWriterSize(w, snapshotStreamBufferSize)
	headerIndex := index
	if err := writeSnapshotJSON(bw, snapshotStreamHeader{
		Format:  snapshotStreamFormat,
		Version: snapshotStreamVersion,
		Index:   &headerIndex,
	}); err != nil {
		return 0, fmt.Errorf("kvserver: stream snapshot: write header: %w", err)
	}

	digest := crc32.NewIEEE()
	var crcBuf [4]byte
	var count uint64
	for it.Valid() {
		key, value := it.Key(), it.Value()
		crc := snapshotEntryCRC(key, value)
		if err := writeSnapshotJSON(bw, snapshotStreamEntry{
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
			if err := bw.Flush(); err != nil {
				return 0, fmt.Errorf("kvserver: stream snapshot: flush after entry %d: %w", count, err)
			}
		}
		it.Next()
	}
	if err := it.Error(); err != nil {
		return 0, fmt.Errorf("kvserver: stream snapshot: iterate: %w", err)
	}

	if err := writeSnapshotJSON(bw, snapshotStreamTrailer{
		End:   true,
		Count: count,
		CRC:   digest.Sum32(),
	}); err != nil {
		return 0, fmt.Errorf("kvserver: stream snapshot: write trailer: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return 0, fmt.Errorf("kvserver: stream snapshot: flush: %w", err)
	}
	return index, nil
}

// RestoreSnapshotStream reads a stream produced by StreamSnapshot and replaces
// this range's keyspace in bounded batches.
//
// For compatibility with snapshots persisted by releases that predate the
// stream format, the reader may instead hold the legacy single-JSON-object
// payload ({"index":...,"entries":...}). That payload is recognised by its
// leading member and buffered in memory, exactly like the old implementation;
// current snapshots always stream.
//
// The header is fully validated before anything is deleted, so a bad header
// leaves the existing keyspace untouched. Existing keys are then deleted in
// batches and streamed keys/values are inserted in batches of at most
// snapshotBatchEntries records or snapshotBatchBytes bytes (a single oversized
// record is allowed on its own). The last applied index is updated only after
// the trailer has been verified.
//
// The restore is not atomic. If the stream fails after the delete phase starts
// (truncated mid-stream, checksum mismatch, storage error) the range is left
// with a mix of the old and new records, and this method returns an error
// describing the failure. The range must be treated as damaged: retry with a
// complete stream (replaying a valid stream is idempotent, because it begins
// by deleting the range) or reinstall the range from a peer. A successful
// restore is complete and idempotent.
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

	// Header is valid: from here on the old keyspace is destroyed, so every
	// later error leaves a partial restore (documented above).
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
		line, eof, err := readSnapshotLine(br)
		if err != nil {
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
			if extra, perr := br.Peek(1); perr != nil && !errors.Is(perr, io.EOF) {
				return fmt.Errorf("kvserver: restore snapshot stream: read after trailer: %w", perr)
			} else if len(extra) > 0 {
				return fmt.Errorf("%w: trailing data after trailer", ErrSnapshotStreamCorrupt)
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
	return h, nil
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
