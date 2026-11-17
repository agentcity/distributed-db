package mvcc

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"distributed-db/internal/storage"
)

// SnapshotLockError reports a lock that makes a backup snapshot unsafe. It is
// returned by ScanForBackup and unwraps to ErrLocked so existing lock-wait
// handling keeps working.
type SnapshotLockError struct {
	// Key is the locked user key.
	Key []byte
	// StartTS is the lock's start timestamp.
	StartTS uint64
	// SnapshotTS is the timestamp the backup asked for.
	SnapshotTS uint64
}

func (e *SnapshotLockError) Error() string {
	return fmt.Sprintf("mvcc: lock on key %q started at %d blocks backup snapshot at %d", e.Key, e.StartTS, e.SnapshotTS)
}

// Unwrap lets callers use errors.Is(err, ErrLocked).
func (e *SnapshotLockError) Unwrap() error { return ErrLocked }

// lockAt reads and decodes the lock for userKey, if any.
func (s *Store) lockAt(userKey []byte) (lockRecord, bool, error) {
	value, err := s.engine.Get(lockKey(userKey))
	if errors.Is(err, storage.ErrNotFound) {
		return lockRecord{}, false, nil
	}
	if err != nil {
		return lockRecord{}, false, err
	}
	rec, err := decodeLock(value)
	if err != nil {
		return lockRecord{}, false, err
	}
	return rec, true, nil
}

// writeAt reads and decodes the write record at exactly commitTS.
func (s *Store) writeAt(userKey []byte, commitTS uint64) (writeRecord, bool, error) {
	value, err := s.engine.Get(writeKey(userKey, commitTS))
	if errors.Is(err, storage.ErrNotFound) {
		return writeRecord{}, false, nil
	}
	if err != nil {
		return writeRecord{}, false, err
	}
	rec, err := decodeWrite(value)
	if err != nil {
		return writeRecord{}, false, err
	}
	return rec, true, nil
}

// Get returns the newest value committed at or before ts.
//
// If a lock with startTS <= ts exists, Get returns ErrLocked: lock resolution
// (checking the primary and committing or rolling back the lock) is deferred
// to a later milestone. Locks with startTS > ts belong to transactions that are
// invisible to this snapshot and are ignored.
//
// The write column is the authoritative list of committed versions: rollback
// records are skipped, a delete record hides all older versions, and a put's
// value is read from the data column at that commitTS.
func (s *Store) Get(key []byte, ts uint64) ([]byte, error) {
	lock, locked, err := s.lockAt(key)
	if err != nil {
		return nil, err
	}
	if locked && lock.startTS <= ts {
		return nil, ErrLocked
	}

	start, end := versionedRange(writeCF, key)
	iter, err := s.engine.NewIter(start, end)
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var (
		bestKind  byte
		bestTS    uint64
		bestFound bool
	)
	for iter.Valid() {
		userKey, commitTS, ok := decodeVersionedKey(iter.Key())
		if !ok || !bytes.Equal(userKey, key) || commitTS > ts {
			iter.Next()
			continue
		}
		rec, err := decodeWrite(iter.Value())
		if err != nil {
			return nil, err
		}
		if rec.kind == kindRollback {
			iter.Next()
			continue
		}
		if !bestFound || commitTS > bestTS {
			bestKind = rec.kind
			bestTS = commitTS
			bestFound = true
		}
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}
	if !bestFound || bestKind == kindDelete {
		return nil, ErrKeyNotFound
	}

	value, err := s.engine.Get(dataKey(key, bestTS))
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return value, nil
}

// Scan returns the newest visible version of every user key in [start, end) at
// snapshot ts, in ascending key order. A nil start means the beginning and a
// nil end means the end of the key space. limit <= 0 means unlimited.
//
// Committed write versions are merged and deduplicated per user key; rollback
// records are skipped and a delete hides the key. Locks are not returned and do
// not cause ErrLocked here: a prewrite is invisible until it commits, so a scan
// sees the previous committed version (or nothing).
func (s *Store) Scan(start, end []byte, ts uint64, limit int) ([]KV, error) {
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return []KV{}, nil
	}
	return scanWrites(s.engine.NewIter, s.engine.Get, start, end, ts, limit)
}

// ScanForBackup returns the same visible key/value set as Scan at snapshot ts,
// but fails closed when the underlying store contains a lock that could hide a
// committed version at or below ts. It is the narrow lock-aware read mode for
// logical backups; Scan keeps its historical lock-ignoring semantics for normal
// callers.
//
// A lock with startTS <= ts is returned (wrapped in *SnapshotLockError, which
// unwraps to ErrLocked). Locks with startTS > ts belong to transactions that
// are invisible at ts and are ignored, matching Get. The caller should resolve
// the lock through its primary and retry, or abort the backup with a clear
// error.
//
// Why a committed value cannot be missed: the lock column and the write column
// are read from one engine snapshot. Commit writes the committed data and write
// record and deletes the lock in one atomic batch, so within that snapshot a
// key is either (a) still locked (detected before the write snapshot is read)
// or (b) already has its write record at commitTS <= ts. If no lock at or below
// ts exists, every commitTS <= ts that is present in the snapshot is returned
// by the normal write scan; no superseded version can win because the scan
// keeps the highest commitTS per key.
//
// ScanForBackup cannot detect a commit that was allocated at or below ts but
// whose atomic write batch has not yet reached the engine (notably 1PC commits,
// which never install a lock). That gap is a TSO/commit-lease problem, not a
// lock problem: callers must run a barrier that either waits for in-flight
// commits at or below the snapshot TSO to apply or chooses a lagged TSO. See
// the backup package for the barrier and its documented limits.
func (s *Store) ScanForBackup(start, end []byte, ts uint64, limit int) ([]KV, error) {
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return []KV{}, nil
	}
	snap, err := s.engine.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("mvcc: backup scan snapshot: %w", err)
	}
	defer snap.Close()

	if err := scanLocks(snap.NewIter, start, end, ts); err != nil {
		return nil, err
	}
	return scanWrites(snap.NewIter, snap.Get, start, end, ts, limit)
}

// scanLocks fails on the first lock in [start, end) whose startTS could make a
// commit visible at ts. It runs before the write pass of ScanForBackup.
func scanLocks(newIter iterFactory, start, end []byte, ts uint64) error {
	lower := []byte{lockCF}
	if start != nil {
		lower = append([]byte{lockCF}, start...)
	}
	// Lock keys have no version suffix, so an exclusive bound on the user key
	// is also an exact bound on the lock column.
	upper := []byte{lockCF + 1}
	if end != nil {
		upper = append([]byte{lockCF}, end...)
	}
	iter, err := newIter(lower, upper)
	if err != nil {
		return err
	}
	defer iter.Close()

	for iter.Valid() {
		rawKey := iter.Key()
		if len(rawKey) < 2 || rawKey[0] != lockCF {
			return fmt.Errorf("mvcc: corrupt lock record key %x", rawKey)
		}
		userKey := rawKey[1:]
		if start != nil && bytes.Compare(userKey, start) < 0 {
			iter.Next()
			continue
		}
		if end != nil && bytes.Compare(userKey, end) >= 0 {
			iter.Next()
			continue
		}
		rec, err := decodeLock(iter.Value())
		if err != nil {
			return err
		}
		if rec.startTS <= ts {
			return &SnapshotLockError{
				Key:        append([]byte(nil), userKey...),
				StartTS:    rec.startTS,
				SnapshotTS: ts,
			}
		}
		iter.Next()
	}
	return iter.Error()
}

// kvGetter reads one raw engine key. It is engine.Get or snapshot.Get.
type kvGetter func(key []byte) ([]byte, error)

// scanWrites is the shared implementation of Scan and ScanForBackup. It reads
// the write column and then the data version of the newest visible record for
// each key.
func scanWrites(newIter iterFactory, get kvGetter, start, end []byte, ts uint64, limit int) ([]KV, error) {
	lower := []byte{writeCF}
	if start != nil {
		lower = append([]byte{writeCF}, start...)
	}
	upper := []byte{writeCF + 1}
	if end != nil {
		upper = append([]byte{writeCF}, end...)
	}

	iter, err := newIter(lower, upper)
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	type version struct {
		ts   uint64
		kind byte
	}
	best := make(map[string]version)
	for iter.Valid() {
		userKey, commitTS, ok := decodeVersionedKey(iter.Key())
		if !ok || commitTS > ts {
			iter.Next()
			continue
		}
		if start != nil && bytes.Compare(userKey, start) < 0 {
			iter.Next()
			continue
		}
		if end != nil && bytes.Compare(userKey, end) >= 0 {
			iter.Next()
			continue
		}
		rec, err := decodeWrite(iter.Value())
		if err != nil {
			return nil, err
		}
		if rec.kind == kindRollback {
			iter.Next()
			continue
		}
		keyString := string(userKey)
		if prev, exists := best[keyString]; !exists || commitTS > prev.ts {
			best[keyString] = version{ts: commitTS, kind: rec.kind}
		}
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(best))
	for key := range best {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]KV, 0, len(keys))
	for _, key := range keys {
		v := best[key]
		if v.kind == kindDelete {
			continue
		}
		value, err := get(dataKey([]byte(key), v.ts))
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, KV{Key: []byte(key), Value: value})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
