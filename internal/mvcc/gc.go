package mvcc

import (
	"bytes"
	"fmt"
)

// GC removes versions that transactions at or above safePoint can no longer
// read, plus locks that are assumed to belong to abandoned transactions.
//
// It keeps:
//   - the newest committed put/delete version at or below safePoint (the base
//     version), so snapshots at safePoint and later still see current data;
//   - every data/write version at or above safePoint;
//   - locks whose startTS is at or above safePoint.
//
// It deletes older data and write versions, rollback records below safePoint,
// and locks with startTS < safePoint.
//
// # Deferred work
//
// This is deliberately a basic, offline GC. It does not resolve locks (check
// the primary and commit/rollback the transaction), it does not honour
// lock TTL against a wall clock, and it is not safe to run concurrently with a
// transaction whose startTS is below safePoint. It collects keys in memory
// instead of using range deletes or Pebble compaction filters, and it does not
// run in the background. Callers are responsible for moving safePoint forward
// monotonically; running GC with an older safe point can delete a version a
// later snapshot still needs.
func (s *Store) GC(safePoint uint64) error {
	var deletes [][]byte

	// Data versions: keep the base put version and everything above the safe
	// point; everything else is unreachable by snapshots >= safePoint.
	type baseVersion struct {
		ts    uint64
		kind  byte
		found bool
	}
	baseCache := make(map[string]baseVersion)

	iter, err := s.engine.NewIter([]byte{dataCF}, []byte{dataCF + 1})
	if err != nil {
		return err
	}
	for iter.Valid() {
		key := append([]byte(nil), iter.Key()...)
		userKey, commitTS, ok := decodeVersionedKey(key)
		if !ok || commitTS >= safePoint {
			iter.Next()
			continue
		}

		cacheKey := string(userKey)
		base, cached := baseCache[cacheKey]
		if !cached {
			rec, version, found, err := s.latestRealWriteAtOrBefore(userKey, safePoint)
			if err != nil {
				_ = iter.Close()
				return err
			}
			base = baseVersion{ts: version, kind: rec.kind, found: found}
			baseCache[cacheKey] = base
		}
		if base.found && base.kind == kindPut && base.ts == commitTS {
			iter.Next()
			continue
		}
		deletes = append(deletes, key)
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		_ = iter.Close()
		return err
	}
	if err := iter.Close(); err != nil {
		return err
	}

	// Write records: find the newest real (put/delete) version per key below
	// the safe point, then delete all other write records below it. Rollbacks
	// below the safe point can be dropped once the locks are gone.
	newestReal := make(map[string]uint64)
	iter, err = s.engine.NewIter([]byte{writeCF}, []byte{writeCF + 1})
	if err != nil {
		return err
	}
	for iter.Valid() {
		userKey, commitTS, ok := decodeVersionedKey(iter.Key())
		if !ok || commitTS >= safePoint {
			iter.Next()
			continue
		}
		rec, err := decodeWrite(iter.Value())
		if err != nil {
			_ = iter.Close()
			return err
		}
		if rec.kind == kindPut || rec.kind == kindDelete {
			cacheKey := string(userKey)
			if current, exists := newestReal[cacheKey]; !exists || commitTS > current {
				newestReal[cacheKey] = commitTS
			}
		}
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		_ = iter.Close()
		return err
	}
	if err := iter.Close(); err != nil {
		return err
	}

	iter, err = s.engine.NewIter([]byte{writeCF}, []byte{writeCF + 1})
	if err != nil {
		return err
	}
	for iter.Valid() {
		key := append([]byte(nil), iter.Key()...)
		userKey, commitTS, ok := decodeVersionedKey(key)
		if !ok || commitTS >= safePoint {
			iter.Next()
			continue
		}
		rec, err := decodeWrite(iter.Value())
		if err != nil {
			_ = iter.Close()
			return err
		}
		if rec.kind == kindPut || rec.kind == kindDelete {
			if newest, exists := newestReal[string(userKey)]; exists && newest == commitTS {
				iter.Next()
				continue
			}
		}
		deletes = append(deletes, key)
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		_ = iter.Close()
		return err
	}
	if err := iter.Close(); err != nil {
		return err
	}

	// Locks below the safe point are treated as abandoned. A later
	// milestone will resolve them through the primary instead of deleting them.
	iter, err = s.engine.NewIter([]byte{lockCF}, []byte{lockCF + 1})
	if err != nil {
		return err
	}
	for iter.Valid() {
		lock, err := decodeLock(iter.Value())
		if err != nil {
			_ = iter.Close()
			return err
		}
		if lock.startTS < safePoint {
			deletes = append(deletes, append([]byte(nil), iter.Key()...))
		}
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		_ = iter.Close()
		return err
	}
	if err := iter.Close(); err != nil {
		return err
	}

	return s.applyDeletes(deletes)
}

// latestRealWriteAtOrBefore returns the newest put/delete record for key with a
// commitTS <= maxVersion.
func (s *Store) latestRealWriteAtOrBefore(key []byte, maxVersion uint64) (writeRecord, uint64, bool, error) {
	start, end := versionedRange(writeCF, key)
	iter, err := s.engine.NewIter(start, end)
	if err != nil {
		return writeRecord{}, 0, false, err
	}
	defer iter.Close()

	var (
		best      writeRecord
		bestTS    uint64
		bestFound bool
	)
	for iter.Valid() {
		userKey, commitTS, ok := decodeVersionedKey(iter.Key())
		if !ok || !bytes.Equal(userKey, key) || commitTS > maxVersion {
			iter.Next()
			continue
		}
		rec, err := decodeWrite(iter.Value())
		if err != nil {
			return writeRecord{}, 0, false, err
		}
		if rec.kind != kindPut && rec.kind != kindDelete {
			iter.Next()
			continue
		}
		if !bestFound || commitTS > bestTS {
			best = rec
			bestTS = commitTS
			bestFound = true
		}
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		return writeRecord{}, 0, false, err
	}
	return best, bestTS, bestFound, nil
}

// applyDeletes deletes keys in bounded batches so a large GC does not build one
// enormous engine batch.
func (s *Store) applyDeletes(keys [][]byte) error {
	const gcBatchSize = 1024
	for len(keys) > 0 {
		n := gcBatchSize
		if len(keys) < n {
			n = len(keys)
		}
		batch := s.engine.NewBatch()
		for _, key := range keys[:n] {
			if err := batch.Delete(key); err != nil {
				_ = batch.Close()
				return fmt.Errorf("mvcc: gc delete: %w", err)
			}
		}
		if err := batch.Commit(); err != nil {
			_ = batch.Close()
			return fmt.Errorf("mvcc: gc batch: %w", err)
		}
		if err := batch.Close(); err != nil {
			return fmt.Errorf("mvcc: gc close: %w", err)
		}
		keys = keys[n:]
	}
	return nil
}
