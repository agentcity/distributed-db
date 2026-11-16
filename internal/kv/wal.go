package kv

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"distributed-db/internal/model"
)

const (
	walFileName    = "wal.log"
	walTmpFileName = "wal.log.tmp"
	walFilePerm    = 0o644
)

// compactThreshold is the record count above which the WAL is rewritten from
// the live map. It is a variable so tests can lower it; production code must
// not modify it after a Store is opened.
var compactThreshold = 10000

// readWAL parses every complete newline-terminated JSON record in path and
// returns the records plus the byte offset just past the last complete line. If
// the file does not exist it returns no records and offset 0. A final line
// without a trailing newline is treated as a torn write and ignored.
func readWAL(path string) ([]model.Entry, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("kv: read WAL: %w", err)
	}

	var records []model.Entry
	var complete int64
	offset := 0
	for offset < len(data) {
		end := bytes.IndexByte(data[offset:], '\n')
		if end < 0 {
			// Torn final record: ignore it. Open truncates it from the file.
			break
		}
		lineStart := offset
		line := data[offset : offset+end]
		offset += end + 1
		if len(bytes.TrimSpace(line)) == 0 {
			complete = int64(offset)
			continue
		}

		var entry model.Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, 0, fmt.Errorf("kv: corrupt WAL record at offset %d: %w", lineStart, err)
		}
		records = append(records, entry)
		complete = int64(offset)
	}
	return records, complete, nil
}

// appendRecordLocked appends one JSON record and fsyncs it. Callers must hold
// s.mu for writing. On failure the partial record is rolled back to the
// previous end of file so the log never contains an unacknowledged line.
func (s *Store) appendRecordLocked(entry model.Entry) error {
	if s.walErr != nil {
		return s.walErr
	}
	if s.wal == nil {
		return errClosed
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("kv: encode WAL record: %w", err)
	}
	line = append(line, '\n')

	oldSize := s.walSize
	n, err := s.wal.Write(line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err == nil {
		if serr := s.wal.Sync(); serr != nil {
			err = serr
		}
	}
	if err != nil {
		// O_APPEND always writes at EOF, so truncating to oldSize removes the
		// partial record regardless of how many bytes were written.
		if terr := s.wal.Truncate(oldSize); terr != nil {
			s.walErr = fmt.Errorf("kv: WAL append failed (%v) and rollback failed: %w", err, terr)
			return s.walErr
		}
		if serr := s.wal.Sync(); serr != nil {
			s.walErr = fmt.Errorf("kv: WAL append failed (%v) and rollback sync failed: %w", err, serr)
			return s.walErr
		}
		return fmt.Errorf("kv: append WAL record: %w", err)
	}

	s.walSize = oldSize + int64(n)
	s.records++
	return nil
}

// maybeCompactLocked rewrites the WAL when it holds more records than
// compactThreshold. The second condition avoids compacting on every Apply when
// the live key set itself is larger than the threshold: a rewrite can only help
// if it reduces the record count. Callers must hold s.mu for writing.
func (s *Store) maybeCompactLocked() error {
	if s.records <= compactThreshold || s.records <= len(s.data) {
		return nil
	}
	return s.compactLocked()
}

// compactLocked atomically replaces the WAL with a snapshot of the live map.
// The snapshot is written to a temp file, fsynced, renamed over wal.log and the
// directory is fsynced so the rename survives a crash. Callers must hold s.mu
// for writing.
func (s *Store) compactLocked() error {
	if s.walErr != nil {
		return s.walErr
	}

	path := filepath.Join(s.dir, walFileName)
	tmpPath := filepath.Join(s.dir, walTmpFileName)

	f, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, walFilePerm)
	if err != nil {
		return fmt.Errorf("kv: create WAL snapshot: %w", err)
	}
	discard := func(cause error) error {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return cause
	}

	bw := bufio.NewWriterSize(f, 64*1024)
	records := 0
	for key, value := range s.data {
		line, err := json.Marshal(model.Entry{
			Type:  model.CommandPut,
			Key:   key,
			Value: value,
		})
		if err != nil {
			return discard(fmt.Errorf("kv: encode WAL snapshot: %w", err))
		}
		if _, err := bw.Write(line); err != nil {
			return discard(fmt.Errorf("kv: write WAL snapshot: %w", err))
		}
		if err := bw.WriteByte('\n'); err != nil {
			return discard(fmt.Errorf("kv: write WAL snapshot: %w", err))
		}
		records++
	}
	if err := bw.Flush(); err != nil {
		return discard(fmt.Errorf("kv: flush WAL snapshot: %w", err))
	}
	if err := f.Sync(); err != nil {
		return discard(fmt.Errorf("kv: sync WAL snapshot: %w", err))
	}
	info, err := f.Stat()
	if err != nil {
		return discard(fmt.Errorf("kv: stat WAL snapshot: %w", err))
	}
	size := info.Size()
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("kv: close WAL snapshot: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("kv: install WAL snapshot: %w", err)
	}

	newFile, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, walFilePerm)
	if err != nil {
		// The old descriptor points at an unlinked inode now; close it and
		// refuse further applies rather than writing to a ghost file.
		if s.wal != nil {
			_ = s.wal.Close()
			s.wal = nil
		}
		s.walErr = fmt.Errorf("kv: reopen compacted WAL: %w", err)
		return s.walErr
	}
	if s.wal != nil {
		_ = s.wal.Close()
	}
	s.wal = newFile
	s.walSize = size
	s.records = records

	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("kv: sync directory after compaction: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so entry creation and renames are durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	return errors.Join(syncErr, closeErr)
}
