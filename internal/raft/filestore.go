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
)

type walRecord struct {
	Type      string        `json:"type"` // "meta", "append", "truncate"
	Term      uint64        `json:"term,omitempty"`
	VotedFor  string        `json:"voted_for,omitempty"`
	Entries   []model.Entry `json:"entries,omitempty"`
	FromIndex uint64        `json:"from_index,omitempty"`
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
}

var errStoreClosed = errors.New("raft: log store closed")

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
			if e.Index != uint64(len(s.entries))+1 {
				return fmt.Errorf("raft: WAL append index %d after %d", e.Index, len(s.entries))
			}
			s.entries = append(s.entries, cloneEntry(e))
		}
	case "truncate":
		if rec.FromIndex == 0 {
			return errors.New("raft: WAL truncate from index 0")
		}
		if rec.FromIndex <= uint64(len(s.entries)) {
			s.entries = s.entries[:rec.FromIndex-1]
		}
	default:
		return fmt.Errorf("raft: unknown WAL record type %q", rec.Type)
	}
	return nil
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
	if len(entries) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	next := uint64(len(s.entries)) + 1
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
	if fromIndex > uint64(len(s.entries)) {
		return nil
	}
	if err := s.writeRecord(walRecord{Type: "truncate", FromIndex: fromIndex}); err != nil {
		return err
	}
	s.entries = s.entries[:fromIndex-1]
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
