// Package kv implements the replicated key/value state machine used by Raft.
//
// A Store keeps an in-memory map plus an append-only, newline-delimited JSON
// write-ahead log in the directory passed to Open. Apply mutates the map and
// appends the matching record, fsyncing before it returns. Open replays the WAL
// to rebuild the map, ignoring (and truncating) a torn trailing record. Once the
// WAL holds more records than compactThreshold, it is rewritten atomically from
// the live map using a temp file, fsync, rename and a directory fsync.
package kv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"distributed-db/internal/model"
)

// errClosed is returned by Apply after Close.
var errClosed = errors.New("kv: store is closed")

// Store is a durable map[string][]byte state machine. It is safe for
// concurrent use: Get takes a read lock while Apply and Close take the write
// lock.
type Store struct {
	mu   sync.RWMutex
	dir  string
	data map[string][]byte

	// wal is the append-only log. The remaining fields describe it and are
	// protected by mu.
	wal     *os.File
	walSize int64
	records int
	closed  bool
	walErr  error // sticky, unrecoverable WAL failure
}

// Open opens (creating if needed) the state machine stored in dir, replays the
// WAL and returns the rebuilt Store. A torn final record is ignored and
// removed from the log.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("kv: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("kv: create directory %q: %w", dir, err)
	}

	path := filepath.Join(dir, walFileName)
	records, complete, err := readWAL(path)
	if err != nil {
		return nil, err
	}

	s := &Store{dir: dir, data: make(map[string][]byte)}
	for i, rec := range records {
		if err := s.replayRecord(rec); err != nil {
			return nil, fmt.Errorf("kv: replay WAL record %d: %w", i+1, err)
		}
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, walFilePerm)
	if err != nil {
		return nil, fmt.Errorf("kv: open WAL: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("kv: stat WAL: %w", err)
	}
	if info.Size() > complete {
		// The file ends in a torn record. Drop it so subsequent appends start
		// on a clean line and replay stays unambiguous.
		if err := f.Truncate(complete); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("kv: truncate torn WAL tail: %w", err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("kv: sync WAL after truncating torn tail: %w", err)
		}
		if info, err = f.Stat(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("kv: stat WAL: %w", err)
		}
	}

	s.wal = f
	s.walSize = info.Size()
	s.records = len(records)

	// Make the directory entry itself durable (relevant when wal.log was
	// created by this call).
	if err := syncDir(dir); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("kv: sync directory: %w", err)
	}

	if err := s.maybeCompactLocked(); err != nil {
		_ = s.wal.Close()
		s.wal = nil
		return nil, err
	}
	return s, nil
}

// replayRecord applies one durable WAL record to the in-memory map. It performs
// no I/O; validation happens in Apply before anything is written.
func (s *Store) replayRecord(entry model.Entry) error {
	switch entry.Type {
	case model.CommandNoop:
		return nil
	case model.CommandPut:
		s.data[entry.Key] = cloneBytes(entry.Value)
	case model.CommandDelete:
		delete(s.data, entry.Key)
	default:
		return fmt.Errorf("kv: unknown command type %q", entry.Type)
	}
	return nil
}

// Apply implements raft.StateMachine. put stores a copy of entry.Value,
// delete is idempotent, and noop changes nothing. Each state-changing command
// is appended to the WAL and fsynced before Apply returns; unknown command
// types return an error without changing state or the log.
func (s *Store) Apply(entry model.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errClosed
	}
	if s.walErr != nil {
		return s.walErr
	}

	switch entry.Type {
	case model.CommandNoop:
		// Nothing to persist: noop has no effect on the map.
		return nil
	case model.CommandPut:
		rec := model.Entry{
			Term:  entry.Term,
			Index: entry.Index,
			Type:  model.CommandPut,
			Key:   entry.Key,
			Value: entry.Value,
		}
		// Persist first: if the write or fsync fails the map is untouched and
		// replay cannot observe a record Apply did not acknowledge.
		if err := s.appendRecordLocked(rec); err != nil {
			return err
		}
		s.data[entry.Key] = cloneBytes(entry.Value)
	case model.CommandDelete:
		rec := model.Entry{
			Term:  entry.Term,
			Index: entry.Index,
			Type:  model.CommandDelete,
			Key:   entry.Key,
		}
		if err := s.appendRecordLocked(rec); err != nil {
			return err
		}
		delete(s.data, entry.Key)
	default:
		return fmt.Errorf("kv: unknown command type %q", entry.Type)
	}

	return s.maybeCompactLocked()
}

// Get returns a copy of the value stored under key. The boolean reports
// whether the key exists.
func (s *Store) Get(key string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	value, ok := s.data[key]
	if !ok {
		return nil, false
	}
	return cloneBytes(value), true
}

// Len returns the number of keys currently stored.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

// Close flushes and closes the WAL. It is idempotent and safe to call on an
// already closed Store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true
	if s.wal == nil {
		return nil
	}

	syncErr := s.wal.Sync()
	closeErr := s.wal.Close()
	s.wal = nil
	return errors.Join(syncErr, closeErr)
}

// cloneBytes returns an independent copy of b (nil stays nil).
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
