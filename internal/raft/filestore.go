package raft

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"distributed-db/internal/model"
)

// WAL layout: a sequence of records. Each record is
//
//	4-byte big-endian magic ("RAFT")
//	4-byte big-endian payload length
//	4-byte big-endian CRC32 (IEEE) of the payload
//	payload (JSON-encoded walRecord)
//
// The magic, length and CRC let Load distinguish a torn tail (a crash in the
// middle of a write, or trailing garbage) from a complete record and drop only
// the incomplete suffix.
const (
	walFileName   = "wal.log"
	walFrameHdr   = 12
	walMagic      = 0x52414654 // "RAFT"
	maxWalPayload = 256 << 20

	// Snapshots live beside the WAL, never inside it: a snapshot payload can be
	// much larger than a log record and is written with its own temp+rename.
	snapshotDirName  = "snapshot"
	snapshotMetaName = "meta.json"
)

type walRecord struct {
	Type      string        `json:"type"` // "meta", "append", "truncate", "truncate_prefix"
	Term      uint64        `json:"term,omitempty"`
	VotedFor  string        `json:"voted_for,omitempty"`
	Entries   []model.Entry `json:"entries,omitempty"`
	FromIndex uint64        `json:"from_index,omitempty"`
}

// snapshotMeta is the durable description of the current snapshot. The data
// file is versioned by index so a crash between writing the data and the meta
// leaves the previous meta pointing at an untouched file.
type snapshotMeta struct {
	Index    uint64 `json:"index"`
	Term     uint64 `json:"term"`
	DataFile string `json:"data_file"`
	Size     int64  `json:"size"`
	CRC32    uint32 `json:"crc32"`
}

// FileStore is an append-only WAL LogStore. Every mutation is fsynced before
// it updates the in-memory view or returns, so an acknowledged vote or entry
// survives a process crash.
//
// A FileStore is safe for concurrent use.
type FileStore struct {
	mu       sync.Mutex
	dir      string
	path     string
	f        *os.File
	size     int64
	closed   bool
	term     uint64
	votedFor string
	entries  []model.Entry
	// prefix is the highest index removed by TruncatePrefix. Entries are
	// contiguous from prefix+1 (or empty).
	prefix   uint64
	snapMeta snapshotMeta
}

var errStoreClosed = errors.New("raft: log store closed")

// FileStore implements the base LogStore and the optional group-commit
// BatchAppender capability.
var _ BatchAppender = (*FileStore)(nil)

// NewFileStore opens (creating if needed) the WAL in dir and replays it. A
// partial or corrupt final record is discarded and the file is truncated back
// to the last complete record.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("raft: empty log store directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("raft: create log directory: %w", err)
	}
	path := filepath.Join(dir, walFileName)

	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("raft: open WAL: %w", err)
	}
	s := &FileStore{dir: dir, path: path, f: f}
	// Read the snapshot description before replaying the WAL: entries the
	// snapshot already covers are dropped, so Load starts after its index even
	// if the prefix-truncation record never made it to disk.
	meta, err := readSnapshotMeta(dir)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	s.snapMeta = meta
	s.prefix = meta.Index
	if err := s.replay(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if created {
		if err := syncDir(dir); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("raft: sync log directory: %w", err)
		}
	}
	return s, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// replay rebuilds the in-memory state from the WAL and truncates an incomplete
// tail so future appends are not appended after garbage.
func (s *FileStore) replay() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("raft: read WAL: %w", err)
	}
	off := 0
	for off < len(data) {
		// A torn header or payload can only be the last thing in the file.
		if len(data)-off < walFrameHdr {
			break
		}
		if binary.BigEndian.Uint32(data[off:off+4]) != walMagic {
			// Trailing garbage (or a torn record whose header never landed).
			break
		}
		plen := binary.BigEndian.Uint32(data[off+4 : off+8])
		wantCRC := binary.BigEndian.Uint32(data[off+8 : off+12])
		if uint64(plen) > maxWalPayload {
			// A garbage length can only come from a torn tail; no valid
			// record can be this large.
			break
		}
		end := off + walFrameHdr + int(plen)
		if end > len(data) {
			break
		}
		payload := data[off+walFrameHdr : end]
		var rec walRecord
		bad := crc32.ChecksumIEEE(payload) != wantCRC || json.Unmarshal(payload, &rec) != nil
		if bad {
			if end == len(data) {
				// Corrupt final record: drop it.
				break
			}
			return fmt.Errorf("raft: corrupt WAL record at offset %d", off)
		}
		if err := s.applyRecord(rec); err != nil {
			return err
		}
		off = end
	}
	if off < len(data) {
		if err := os.Truncate(s.path, int64(off)); err != nil {
			return fmt.Errorf("raft: truncate torn WAL tail: %w", err)
		}
		if err := s.f.Sync(); err != nil {
			return fmt.Errorf("raft: sync WAL after tail truncation: %w", err)
		}
	}
	s.size = int64(off)
	if _, err := s.f.Seek(s.size, io.SeekStart); err != nil {
		return fmt.Errorf("raft: seek WAL: %w", err)
	}
	return nil
}

func (s *FileStore) applyRecord(rec walRecord) error {
	switch rec.Type {
	case "meta":
		s.term = rec.Term
		s.votedFor = rec.VotedFor
	case "append":
		for _, e := range rec.Entries {
			if e.Index <= s.prefix {
				// Covered by a snapshot or a prefix truncation that is
				// applied while replaying.
				continue
			}
			if e.Index != s.nextIndexLocked() {
				return fmt.Errorf("raft: WAL append index %d after %d", e.Index, s.lastIndexLocked())
			}
			s.entries = append(s.entries, cloneEntry(e))
		}
	case "truncate":
		if rec.FromIndex == 0 {
			return errors.New("raft: WAL truncate from index 0")
		}
		s.dropFromLocked(rec.FromIndex)
	case "truncate_prefix":
		if rec.FromIndex == 0 {
			return errors.New("raft: WAL truncate prefix from index 0")
		}
		if rec.FromIndex > s.prefix {
			s.prefix = rec.FromIndex
			s.dropThroughLocked(rec.FromIndex)
		}
	default:
		return fmt.Errorf("raft: unknown WAL record type %q", rec.Type)
	}
	return nil
}

// lastIndexLocked returns the index of the last log entry (or the compacted
// prefix when no entries remain).
func (s *FileStore) lastIndexLocked() uint64 {
	if len(s.entries) == 0 {
		return s.prefix
	}
	return s.entries[len(s.entries)-1].Index
}

func (s *FileStore) nextIndexLocked() uint64 {
	return s.lastIndexLocked() + 1
}

func (s *FileStore) dropFromLocked(fromIndex uint64) {
	if fromIndex <= s.prefix {
		s.entries = nil
		return
	}
	keep := make([]model.Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.Index < fromIndex {
			keep = append(keep, e)
		}
	}
	s.entries = keep
}

func (s *FileStore) dropThroughLocked(index uint64) {
	keep := make([]model.Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.Index > index {
			keep = append(keep, e)
		}
	}
	s.entries = keep
}

// Load returns the durable state. The returned entries are copies.
func (s *FileStore) Load() (PersistentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return PersistentState{}, errStoreClosed
	}
	return PersistentState{
		Term:     s.term,
		VotedFor: s.votedFor,
		Entries:  cloneEntries(s.entries),
	}, nil
}

// SaveMeta durably records the current term and vote.
func (s *FileStore) SaveMeta(term uint64, votedFor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	if err := s.writeRecord(walRecord{Type: "meta", Term: term, VotedFor: votedFor}); err != nil {
		return err
	}
	s.term = term
	s.votedFor = votedFor
	return nil
}

// Append durably appends contiguous entries. It is atomic with respect to the
// in-memory view: on error nothing is added there.
func (s *FileStore) Append(entries ...model.Entry) error {
	return s.append(entries)
}

// AppendBatch durably appends a contiguous batch of entries as a single WAL
// record: one write and one fsync cover the whole batch. It is atomic with
// respect to both the durable and the in-memory log: on error neither is
// changed. It implements the optional BatchAppender capability used for group
// commit.
func (s *FileStore) AppendBatch(entries []model.Entry) error {
	return s.append(entries)
}

func (s *FileStore) append(entries []model.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	next := s.nextIndexLocked()
	for i, e := range entries {
		if e.Index != next+uint64(i) {
			return fmt.Errorf("raft: append index %d, want %d", e.Index, next+uint64(i))
		}
	}
	copied := cloneEntries(entries)
	if err := s.writeRecord(walRecord{Type: "append", Entries: copied}); err != nil {
		return err
	}
	s.entries = append(s.entries, copied...)
	return nil
}

// Truncate durably removes entries with Index >= fromIndex. Truncating at or
// beyond the end of the log is a no-op.
func (s *FileStore) Truncate(fromIndex uint64) error {
	if fromIndex == 0 {
		return errors.New("raft: truncate from index 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	if fromIndex > s.lastIndexLocked() {
		return nil
	}
	if err := s.writeRecord(walRecord{Type: "truncate", FromIndex: fromIndex}); err != nil {
		return err
	}
	s.dropFromLocked(fromIndex)
	return nil
}

// TruncatePrefix durably removes all entries with Index <= index. The snapshot
// that covers the removed prefix must have been saved first: truncating beyond
// the durable snapshot would make the log unloadable, so it is rejected.
func (s *FileStore) TruncatePrefix(index uint64) error {
	if index == 0 {
		return errors.New("raft: truncate prefix from index 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	if index <= s.prefix {
		return nil
	}
	if index > s.snapMeta.Index {
		return fmt.Errorf("raft: truncate prefix %d beyond snapshot index %d", index, s.snapMeta.Index)
	}
	if err := s.writeRecord(walRecord{Type: "truncate_prefix", FromIndex: index}); err != nil {
		return err
	}
	s.prefix = index
	s.dropThroughLocked(index)
	return nil
}

// SaveSnapshot durably stores snapshot data and metadata under the store's
// snapshot/ directory. Both files are written to temporaries, fsynced and
// renamed; the meta file is the commit point, and its data file name is
// versioned by index so a crash cannot pair new data with old metadata.
func (s *FileStore) SaveSnapshot(snapshot Snapshot) error {
	if snapshot.Index == 0 {
		return errors.New("raft: snapshot index must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	if snapshot.Index <= s.snapMeta.Index {
		return nil
	}

	dir := filepath.Join(s.dir, snapshotDirName)
	created := false
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		created = true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("raft: create snapshot directory: %w", err)
	}
	if created {
		if err := syncDir(s.dir); err != nil {
			return fmt.Errorf("raft: sync log directory: %w", err)
		}
	}

	dataName := fmt.Sprintf("data-%d.bin", snapshot.Index)
	dataPath := filepath.Join(dir, dataName)
	if err := writeFileAtomic(dataPath, snapshot.Data); err != nil {
		return fmt.Errorf("raft: write snapshot data: %w", err)
	}
	meta := snapshotMeta{
		Index:    snapshot.Index,
		Term:     snapshot.Term,
		DataFile: dataName,
		Size:     int64(len(snapshot.Data)),
		CRC32:    crc32.ChecksumIEEE(snapshot.Data),
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("raft: marshal snapshot metadata: %w", err)
	}
	metaPath := filepath.Join(dir, snapshotMetaName)
	if err := writeFileAtomic(metaPath, payload); err != nil {
		return fmt.Errorf("raft: write snapshot metadata: %w", err)
	}
	s.snapMeta = meta
	// Best-effort cleanup of superseded data files. A stale file is harmless:
	// LoadSnapshot only follows the meta file.
	if des, err := os.ReadDir(dir); err == nil {
		for _, de := range des {
			if de.IsDir() || de.Name() == snapshotMetaName || de.Name() == dataName {
				continue
			}
			if strings.HasPrefix(de.Name(), "data-") || strings.HasSuffix(de.Name(), ".tmp") {
				_ = os.Remove(filepath.Join(dir, de.Name()))
			}
		}
	}
	return nil
}

// LoadSnapshot returns the durable snapshot. When none has ever been saved it
// returns the zero Snapshot and a nil error.
func (s *FileStore) LoadSnapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Snapshot{}, errStoreClosed
	}
	return s.loadSnapshotLocked()
}

func (s *FileStore) loadSnapshotLocked() (Snapshot, error) {
	meta := s.snapMeta
	if meta.Index == 0 {
		return Snapshot{}, nil
	}
	data, err := os.ReadFile(filepath.Join(s.dir, snapshotDirName, meta.DataFile))
	if err != nil {
		return Snapshot{}, fmt.Errorf("raft: read snapshot data: %w", err)
	}
	if int64(len(data)) != meta.Size || crc32.ChecksumIEEE(data) != meta.CRC32 {
		return Snapshot{}, fmt.Errorf("raft: snapshot data at index %d is corrupt", meta.Index)
	}
	return Snapshot{Index: meta.Index, Term: meta.Term, Data: data}, nil
}

// readSnapshotMeta loads and validates the snapshot metadata. A missing file
// (or directory) means the store simply has no snapshot.
func readSnapshotMeta(dir string) (snapshotMeta, error) {
	path := filepath.Join(dir, snapshotDirName, snapshotMetaName)
	payload, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return snapshotMeta{}, nil
		}
		return snapshotMeta{}, fmt.Errorf("raft: read snapshot metadata: %w", err)
	}
	var meta snapshotMeta
	if err := json.Unmarshal(payload, &meta); err != nil {
		return snapshotMeta{}, fmt.Errorf("raft: parse snapshot metadata: %w", err)
	}
	if meta.Index == 0 {
		return snapshotMeta{}, errors.New("raft: snapshot metadata has index 0")
	}
	if meta.DataFile == "" || filepath.Base(meta.DataFile) != meta.DataFile {
		return snapshotMeta{}, fmt.Errorf("raft: invalid snapshot data file %q", meta.DataFile)
	}
	if meta.Size < 0 {
		return snapshotMeta{}, fmt.Errorf("raft: invalid snapshot size %d", meta.Size)
	}
	return meta, nil
}

// writeFileAtomic writes b to a sibling temporary file, fsyncs it, renames it
// over path and fsyncs the containing directory.
func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := writeFull(f, b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

// Close syncs and closes the WAL. Close is idempotent. FileStore is unusable
// after Close.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.f == nil {
		return nil
	}
	err := s.f.Sync()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	s.f = nil
	return err
}

// writeRecord appends one framed record and fsyncs it. On failure it rolls the
// file back to the previously known-good size so a torn write cannot poison
// later appends.
func (s *FileStore) writeRecord(rec walRecord) error {
	frame, err := encodeWalRecord(rec)
	if err != nil {
		return err
	}
	start := s.size
	if err := writeFull(s.f, frame); err != nil {
		s.rollback(start)
		return fmt.Errorf("raft: write WAL: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		s.rollback(start)
		return fmt.Errorf("raft: sync WAL: %w", err)
	}
	s.size = start + int64(len(frame))
	return nil
}

func encodeWalRecord(rec walRecord) ([]byte, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("raft: marshal WAL record: %w", err)
	}
	if len(payload) > maxWalPayload {
		return nil, fmt.Errorf("raft: WAL record too large (%d bytes)", len(payload))
	}
	frame := make([]byte, walFrameHdr+len(payload))
	binary.BigEndian.PutUint32(frame[:4], walMagic)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(payload))
	copy(frame[walFrameHdr:], payload)
	return frame, nil
}

func (s *FileStore) rollback(size int64) {
	_ = s.f.Truncate(size)
	_, _ = s.f.Seek(size, io.SeekStart)
	s.size = size
	_ = s.f.Sync()
}

func writeFull(f *os.File, b []byte) error {
	for len(b) > 0 {
		n, err := f.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
