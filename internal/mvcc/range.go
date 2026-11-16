package mvcc

import (
	"bytes"
	"fmt"

	"distributed-db/internal/storage"
)

// StreamRange batch sizing. A non-positive batchSize selects the default and
// an oversized one is clamped, so a caller cannot accidentally ask for an
// unbounded in-memory batch.
const (
	defaultStreamBatchSize = 1024
	maxStreamBatchSize     = 65536
)

// Record is one raw MVCC engine record: its fully encoded key (column family,
// user key and, for data/write records, the version) and its raw value. It is
// used to copy records between stores for range groups; both fields are opaque
// to callers and are owned by the caller.
type Record struct {
	Key   []byte
	Value []byte
}

// StreamRange iterates raw MVCC records whose user key is in [start, end),
// calling fn with bounded batches. A nil start means the beginning and a nil
// end means the end of the user key space.
//
// Every record is delivered exactly once in ascending raw key order, so records
// from the data, lock and write column families (including rollback write
// records) are all included; records in unknown column families are ignored.
// The batches are capped at batchSize records (or the default if batchSize <=
// 0, and at most 65536), and fn is never called with an empty batch. An empty
// or inverted range calls fn zero times.
//
// Each batch is freshly allocated and every key and value in it is a deep copy,
// so mutating a batch never affects the store or records from another batch.
// The callback owns a batch only for the duration of the call; after fn returns,
// its backing storage may be reused.
//
// A Pebble snapshot is opened before the first record is read, so concurrent
// writes made while fn runs are not visible to the stream: the iteration sees
// the store as of the call. StreamRange holds an iterator and a snapshot but
// never materialises the whole range.
//
// If fn returns an error, StreamRange stops immediately and returns that error
// unchanged. The iterator and snapshot are always closed before it returns.
func (s *Store) StreamRange(start, end []byte, batchSize int, fn func([]Record) error) error {
	if fn == nil {
		return fmt.Errorf("mvcc: StreamRange callback is nil")
	}
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return nil
	}
	batchSize = normalizeStreamBatchSize(batchSize)

	snap, err := s.engine.Snapshot()
	if err != nil {
		return fmt.Errorf("mvcc: StreamRange snapshot: %w", err)
	}

	batch := make([]Record, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := fn(batch); err != nil {
			return err
		}
		// Allocate a new slice for the next batch: the callback owns the one
		// it just received only for the duration of the call.
		batch = make([]Record, 0, batchSize)
		return nil
	}

	err = scanRangeWith(snap.NewIter, start, end, func(key, value []byte) error {
		batch = append(batch, Record{
			Key:   append([]byte(nil), key...),
			Value: append([]byte(nil), value...),
		})
		if len(batch) >= batchSize {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if closeErr := snap.Close(); closeErr != nil && err == nil {
		err = fmt.Errorf("mvcc: StreamRange close snapshot: %w", closeErr)
	}
	return err
}

// normalizeStreamBatchSize maps a caller-supplied batch size onto the supported
// range: non-positive values use the default, oversized values are clamped.
func normalizeStreamBatchSize(batchSize int) int {
	switch {
	case batchSize <= 0:
		return defaultStreamBatchSize
	case batchSize > maxStreamBatchSize:
		return maxStreamBatchSize
	default:
		return batchSize
	}
}

// ExportRange returns a deep copy of every raw MVCC record whose user key lies
// in [start, end). A nil start means the beginning and a nil end means the end
// of the user key space.
//
// All column families MVCC knows (data, lock and write) are included; records
// in unknown column families are safely ignored. The returned records are in
// ascending raw key order.
//
// ExportRange is a convenience wrapper around StreamRange that collects the
// batches into one slice. Callers exporting a large range should use
// StreamRange to avoid holding every record in memory at once.
func (s *Store) ExportRange(start, end []byte) ([]Record, error) {
	records := make([]Record, 0)
	err := s.StreamRange(start, end, 0, func(batch []Record) error {
		records = append(records, batch...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// ImportRecords atomically writes raw records into this store. It is meant for
// installing a range copied with ExportRange into a new range group; records
// are written verbatim, so the caller is responsible for providing complete,
// self-consistent records.
//
// An empty record set is a no-op. Any error discards the whole batch, so the
// store is never left with a partial import.
func (s *Store) ImportRecords(records []Record) error {
	if len(records) == 0 {
		return nil
	}
	batch := s.engine.NewBatch()
	for i := range records {
		if err := batch.Set(records[i].Key, records[i].Value); err != nil {
			_ = batch.Close()
			return fmt.Errorf("mvcc: import record %d: %w", i, err)
		}
	}
	if err := batch.Commit(); err != nil {
		_ = batch.Close()
		return fmt.Errorf("mvcc: import: %w", err)
	}
	return batch.Close()
}

// DeleteRange deletes every raw MVCC record whose user key lies in [start, end).
// A nil start means the beginning and a nil end means the end of the user key
// space. Unknown column families are left untouched.
//
// All known records are deleted with one atomic batch, so a failure (including
// an iterator error while collecting the keys) leaves the store unchanged. An
// empty range is a no-op.
func (s *Store) DeleteRange(start, end []byte) error {
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return nil
	}

	batch := s.engine.NewBatch()
	found := false
	err := s.scanRange(start, end, func(key, _ []byte) error {
		found = true
		if err := batch.Delete(key); err != nil {
			return fmt.Errorf("mvcc: delete range record: %w", err)
		}
		return nil
	})
	if err != nil {
		_ = batch.Close()
		return err
	}
	if !found {
		return batch.Close()
	}
	if err := batch.Commit(); err != nil {
		_ = batch.Close()
		return fmt.Errorf("mvcc: delete range: %w", err)
	}
	return batch.Close()
}

// iterFactory opens an iterator over [start, end). It abstracts over an engine
// and a snapshot so range scans share one implementation.
type iterFactory func(start, end []byte) (storage.Iterator, error)

// scanRange calls fn for every raw record in a known column family whose
// decoded user key lies in [start, end). Column families are visited in their
// natural order (data, lock, write), which is also raw key order.
func (s *Store) scanRange(start, end []byte, fn func(key, value []byte) error) error {
	return scanRangeWith(s.engine.NewIter, start, end, fn)
}

// scanRangeWith is scanRange over an arbitrary iterator source, such as a
// snapshot.
func scanRangeWith(newIter iterFactory, start, end []byte, fn func(key, value []byte) error) error {
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return nil
	}
	for _, cf := range [...]byte{dataCF, lockCF, writeCF} {
		if err := scanCFWith(newIter, cf, start, end, fn); err != nil {
			return err
		}
	}
	return nil
}

// scanCFWith scans one column family. The lower bound can be tightened to
// cf|start, but the upper bound deliberately extends to the end of the column
// family: data and write keys append the version after the user key, so a
// shorter user key's versions can sort after a longer key that has the shorter
// one as a prefix. (For example, "a" at a high version sorts after "a\x00" at a
// low version.) Filtering by the decoded user key is therefore required, and a
// cf|end upper bound could silently drop records that belong to the range.
func scanCFWith(newIter iterFactory, cf byte, start, end []byte, fn func(key, value []byte) error) error {
	lower := []byte{cf}
	if start != nil {
		lower = append(lower, start...)
	}
	iter, err := newIter(lower, []byte{cf + 1})
	if err != nil {
		return err
	}
	defer iter.Close()

	for iter.Valid() {
		rawKey := iter.Key()
		userKey, ok := rangeUserKey(cf, rawKey)
		if !ok {
			return fmt.Errorf("mvcc: corrupt %q record key %x in range scan", cf, rawKey)
		}
		if start != nil && bytes.Compare(userKey, start) < 0 {
			iter.Next()
			continue
		}
		if end != nil && bytes.Compare(userKey, end) >= 0 {
			iter.Next()
			continue
		}
		if err := fn(rawKey, iter.Value()); err != nil {
			return err
		}
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		return err
	}
	return nil
}

// rangeUserKey decodes the user key of a raw key from a known column family.
// Data and write keys carry a trailing big-endian version; lock keys do not.
func rangeUserKey(cf byte, rawKey []byte) ([]byte, bool) {
	if len(rawKey) == 0 || rawKey[0] != cf {
		return nil, false
	}
	switch cf {
	case dataCF, writeCF:
		userKey, _, ok := decodeVersionedKey(rawKey)
		return userKey, ok
	case lockCF:
		if len(rawKey) < 2 {
			return nil, false
		}
		return rawKey[1:], true
	default:
		return nil, false
	}
}
