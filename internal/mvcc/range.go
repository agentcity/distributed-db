package mvcc

import (
	"bytes"
	"fmt"
)

// Record is one raw MVCC engine record: its fully encoded key (column family,
// user key and, for data/write records, the version) and its raw value. It is
// used to copy records between stores for range groups; both fields are opaque
// to callers and are owned by the caller.
type Record struct {
	Key   []byte
	Value []byte
}

// ExportRange returns a deep copy of every raw MVCC record whose user key lies
// in [start, end). A nil start means the beginning and a nil end means the end
// of the user key space.
//
// All column families MVCC knows (data, lock and write) are included; records
// in unknown column families are safely ignored. The returned records are in
// ascending raw key order.
func (s *Store) ExportRange(start, end []byte) ([]Record, error) {
	records := make([]Record, 0)
	err := s.scanRange(start, end, func(key, value []byte) error {
		records = append(records, Record{
			Key:   append([]byte(nil), key...),
			Value: append([]byte(nil), value...),
		})
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

// scanRange calls fn for every raw record in a known column family whose
// decoded user key lies in [start, end). Column families are visited in their
// natural order (data, lock, write), which is also raw key order.
func (s *Store) scanRange(start, end []byte, fn func(key, value []byte) error) error {
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return nil
	}
	for _, cf := range [...]byte{dataCF, lockCF, writeCF} {
		if err := s.scanCF(cf, start, end, fn); err != nil {
			return err
		}
	}
	return nil
}

// scanCF scans one column family. The lower bound can be tightened to
// cf|start, but the upper bound deliberately extends to the end of the column
// family: data and write keys append the version after the user key, so a
// shorter user key's versions can sort after a longer key that has the shorter
// one as a prefix. (For example, "a" at a high version sorts after "a\x00" at a
// low version.) Filtering by the decoded user key is therefore required, and a
// cf|end upper bound could silently drop records that belong to the range.
func (s *Store) scanCF(cf byte, start, end []byte, fn func(key, value []byte) error) error {
	lower := []byte{cf}
	if start != nil {
		lower = append(lower, start...)
	}
	iter, err := s.engine.NewIter(lower, []byte{cf + 1})
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
