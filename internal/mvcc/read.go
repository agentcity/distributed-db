package mvcc

import (
	"bytes"
	"errors"
	"sort"

	"distributed-db/internal/storage"
)

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

	lower := []byte{writeCF}
	if start != nil {
		lower = append([]byte{writeCF}, start...)
	}
	upper := []byte{writeCF + 1}
	if end != nil {
		upper = append([]byte{writeCF}, end...)
	}

	iter, err := s.engine.NewIter(lower, upper)
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
		value, err := s.engine.Get(dataKey([]byte(key), v.ts))
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
