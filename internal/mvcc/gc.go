package mvcc

import (
	"bytes"
	"fmt"
)

// GCStats reports the records removed by one GCWithStats pass.
//
// The counters describe records that GC selected for deletion from the data
// and write columns. Locks are never deleted by GC: LocksRetained reports how
// many lock records the pass deliberately left for the lock resolver.
type GCStats struct {
	// DataVersions is the number of data-column versions deleted.
	DataVersions int
	// WriteRecords is the number of committed put/delete write records
	// deleted. Each such record normally has a matching data version, which
	// is counted separately in DataVersions.
	WriteRecords int
	// RollbackRecords is the number of rollback write records deleted.
	RollbackRecords int
	// LocksRetained is the number of lock records left untouched. GC counts
	// every lock record, regardless of its startTS.
	LocksRetained int
}

// Total returns the number of data/write records deleted by the pass. Locks
// are not included because GC never deletes them.
func (st GCStats) Total() int {
	return st.DataVersions + st.WriteRecords + st.RollbackRecords
}

// GC removes versions that transactions at or above safePoint can no longer
// read. It is a thin wrapper around GCWithStats that discards the statistics.
//
// See GCWithStats for the exact retained/deleted rules. GC never deletes lock
// records, and safePoint == 0 is a no-op.
func (s *Store) GC(safePoint uint64) error {
	_, err := s.GCWithStats(safePoint)
	return err
}

// GCWithStats performs one garbage-collection pass at safePoint and reports
// what it did. safePoint is a lower bound on the startTS of every transaction
// that may still read the store; callers are responsible for advancing it
// monotonically.
//
// It keeps:
//   - the newest committed put/delete version at or below safePoint (the base
//     version), so snapshots at safePoint and later still see current data.
//     A base delete is kept as a tombstone even though it has no data record;
//   - every data/write version at or above safePoint.
//
// It deletes:
//   - older data versions and their committed write records below safePoint;
//   - rollback records below safePoint.
//
// It never deletes locks, even when their startTS is below safePoint. A lock's
// startTS says when its transaction began, not whether that transaction is
// still alive or was abandoned, so a lock below the safe point can still belong
// to a live transaction. Locks must be resolved through the transaction primary
// (and, in a replicated group, through Raft); deleting them here would let a
// reader observe a partially committed transaction. All lock records are left
// for the lock resolver, and their count is returned in GCStats.LocksRetained.
//
// safePoint == 0 is treated as "no safe point established yet" and is a no-op
// returning zero-value GCStats. Zero is the zero value of the timestamp type
// rather than a TSO a caller could have resolved, and a GC that treats an unset
// bound as real risks collecting or retaining data by accident. Callers must
// only call GC once they have a real safe point; a zero-valued safe point is
// never treated as a real lower bound.
//
// The pass is deliberately basic and offline-style. It collects keys in memory
// instead of using range deletes or Pebble compaction filters, and it is not
// safe to run concurrently with a transaction whose startTS is below
// safePoint. Running GC with an older safe point can delete a version a later
// snapshot still needs. On error, some batches may already have been applied;
// the returned statistics count the records selected for deletion.
func (s *Store) GCWithStats(safePoint uint64) (GCStats, error) {
	var stats GCStats
	if safePoint == 0 {
		return stats, nil
	}

	var deletes [][]byte

	// Data versions: keep the base put version and everything above the safe
	// point; everything else is unreachable by snapshots >= safePoint. Data
	// of an uncommitted transaction is also collected here: Commit copies the
	// value from the lock, so it does not need the provisional data version.
	type baseVersion struct {
		ts    uint64
		kind  byte
		found bool
	}
	baseCache := make(map[string]baseVersion)

	iter, err := s.engine.NewIter([]byte{dataCF}, []byte{dataCF + 1})
	if err != nil {
		return stats, err
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
				return stats, err
			}
			base = baseVersion{ts: version, kind: rec.kind, found: found}
			baseCache[cacheKey] = base
		}
		if base.found && base.kind == kindPut && base.ts == commitTS {
			iter.Next()
			continue
		}
		deletes = append(deletes, key)
		stats.DataVersions++
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		_ = iter.Close()
		return stats, err
	}
	if err := iter.Close(); err != nil {
		return stats, err
	}

	// Write records: find the newest real (put/delete) version per key below
	// the safe point, then delete all other write records below it. Rollbacks
	// below the safe point can be dropped once their transaction is resolved or
	// unreachable; removing them does not affect lock resolution, because a
	// lock's own record still names its primary and startTS.
	newestReal := make(map[string]uint64)
	iter, err = s.engine.NewIter([]byte{writeCF}, []byte{writeCF + 1})
	if err != nil {
		return stats, err
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
			return stats, err
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
		return stats, err
	}
	if err := iter.Close(); err != nil {
		return stats, err
	}

	iter, err = s.engine.NewIter([]byte{writeCF}, []byte{writeCF + 1})
	if err != nil {
		return stats, err
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
			return stats, err
		}
		if rec.kind == kindPut || rec.kind == kindDelete {
			if newest, exists := newestReal[string(userKey)]; exists && newest == commitTS {
				iter.Next()
				continue
			}
			stats.WriteRecords++
		} else {
			stats.RollbackRecords++
		}
		deletes = append(deletes, key)
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		_ = iter.Close()
		return stats, err
	}
	if err := iter.Close(); err != nil {
		return stats, err
	}

	// Locks are deliberately not collected. Count them so callers can report
	// how much work the lock resolver still has; do not decode or mutate them.
	iter, err = s.engine.NewIter([]byte{lockCF}, []byte{lockCF + 1})
	if err != nil {
		return stats, err
	}
	for iter.Valid() {
		stats.LocksRetained++
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		_ = iter.Close()
		return stats, err
	}
	if err := iter.Close(); err != nil {
		return stats, err
	}

	return stats, s.applyDeletes(deletes)
}

// OldestLockStartTS returns the smallest startTS among all lock records and
// whether any lock exists. It is the conservative cap a caller should apply to
// a GC safe point: a transaction that has written a lock may still commit or
// roll back later, and its startTS is a lower bound on versions it can still
// need. Locks are never deleted by GC; expired locks are counted too, because
// until the lock resolver finishes them this store cannot prove the
// transaction is gone.
//
// The scan is O(number of locks) and is intended for periodic maintenance,
// not the read path.
func (s *Store) OldestLockStartTS() (uint64, bool, error) {
	it, err := s.engine.NewIter([]byte{lockCF}, []byte{lockCF + 1})
	if err != nil {
		return 0, false, err
	}
	defer it.Close()

	var (
		oldest uint64
		found  bool
	)
	for it.Valid() {
		rec, err := decodeLock(it.Value())
		if err != nil {
			return 0, false, fmt.Errorf("mvcc: decode lock while scanning GC bound: %w", err)
		}
		if !found || rec.startTS < oldest {
			oldest = rec.startTS
			found = true
		}
		it.Next()
	}
	if err := it.Error(); err != nil {
		return 0, false, err
	}
	return oldest, found, nil
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
