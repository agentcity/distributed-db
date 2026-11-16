package mvcc

import (
	"bytes"
	"fmt"
	"time"
)

// TxnStatus is the outcome of checking a transaction's primary lock.
type TxnStatus struct {
	Committed   bool
	CommitTS    uint64
	RolledBack  bool
	LockPrimary []byte
	LockTS      uint64
	TTLMillis   uint64
	Expired     bool
	Keys        [][]byte // all locked keys of the transaction, if any
}

// CheckTxnStatus inspects the lock on key (any key of the transaction works)
// and reports whether the transaction is committed, rolled back, still alive,
// or expired. Expired locks are rolled back and returned as RolledBack.
//
// The TTL is measured against the physical component of startTS (milliseconds)
// and now; this is the same TSO clock the meta service uses.
func (s *Store) CheckTxnStatus(key []byte, startTS uint64, now time.Time) (TxnStatus, error) {
	lock, locked, err := s.lockAt(key)
	if err != nil {
		return TxnStatus{}, err
	}
	if locked {
		if startTS != 0 && startTS != lock.startTS {
			return TxnStatus{}, ErrLocked
		}
		keys, err := s.locksForTxn(lock.startTS)
		if err != nil {
			return TxnStatus{}, err
		}
		// A lock can outlive its commit record only after a crash between the
		// write batch and lock cleanup; resolve it as committed if the primary
		// already has a commit record.
		if commitTS, kind, ok, err := s.writeForStart(lock.primary, lock.startTS); err != nil {
			return TxnStatus{}, err
		} else if ok {
			if kind == kindRollback {
				return TxnStatus{RolledBack: true, LockTS: lock.startTS, Keys: keys}, nil
			}
			return TxnStatus{Committed: true, CommitTS: commitTS, LockTS: lock.startTS, Keys: keys}, nil
		}
		age := now.UnixMilli() - int64(lock.startTS>>18)
		expired := age > int64(lock.ttlMillis)
		if expired {
			if err := s.Rollback(keys, lock.startTS); err != nil {
				return TxnStatus{}, err
			}
			return TxnStatus{
				RolledBack: true, LockPrimary: append([]byte(nil), lock.primary...),
				LockTS: lock.startTS, TTLMillis: lock.ttlMillis, Expired: true, Keys: keys,
			}, nil
		}
		return TxnStatus{
			LockPrimary: append([]byte(nil), lock.primary...), LockTS: lock.startTS,
			TTLMillis: lock.ttlMillis, Keys: keys,
		}, nil
	}

	// No lock: look for a write record with startTS on this key. Commit uses
	// the commitTS as the version; rollback uses startTS.
	start, end := versionedRange(writeCF, key)
	it, err := s.engine.NewIter(start, end)
	if err != nil {
		return TxnStatus{}, err
	}
	defer it.Close()
	for it.Valid() {
		userKey, version, ok := decodeVersionedKey(it.Key())
		if !ok || !bytes.Equal(userKey, key) {
			it.Next()
			continue
		}
		rec, err := decodeWrite(it.Value())
		if err != nil {
			return TxnStatus{}, err
		}
		if startTS != 0 && rec.startTS == startTS {
			switch rec.kind {
			case kindRollback:
				return TxnStatus{RolledBack: true, LockTS: startTS}, nil
			case kindPut, kindDelete:
				return TxnStatus{Committed: true, CommitTS: version}, nil
			}
		}
		it.Next()
	}
	if err := it.Error(); err != nil {
		return TxnStatus{}, err
	}
	return TxnStatus{RolledBack: true}, nil
}

// writeForStart finds a commit or rollback record for key whose startTS is the
// given transaction. It returns the commitTS (for rollback, startTS), the
// record kind, and whether a record exists.
func (s *Store) writeForStart(key []byte, startTS uint64) (uint64, byte, bool, error) {
	start, end := versionedRange(writeCF, key)
	it, err := s.engine.NewIter(start, end)
	if err != nil {
		return 0, 0, false, err
	}
	defer it.Close()
	for it.Valid() {
		userKey, version, ok := decodeVersionedKey(it.Key())
		if !ok || !bytes.Equal(userKey, key) {
			it.Next()
			continue
		}
		rec, err := decodeWrite(it.Value())
		if err != nil {
			return 0, 0, false, err
		}
		if rec.startTS == startTS {
			if rec.kind == kindRollback {
				return version, kindRollback, true, nil
			}
			return version, rec.kind, true, nil
		}
		it.Next()
	}
	if err := it.Error(); err != nil {
		return 0, 0, false, err
	}
	return 0, 0, false, nil
}

// ResolveLock commits (commitTS > 0) or rolls back (commitTS == 0) all keys of
// the transaction identified by startTS.
func (s *Store) ResolveLock(keys [][]byte, startTS, commitTS uint64) error {
	if len(keys) == 0 {
		return nil
	}
	if commitTS == 0 {
		return s.Rollback(keys, startTS)
	}
	return s.Commit(keys, startTS, commitTS)
}

// locksForTxn scans the lock column and returns every key locked by startTS.
func (s *Store) locksForTxn(startTS uint64) ([][]byte, error) {
	start := []byte{lockCF}
	end := []byte{lockCF + 1}
	it, err := s.engine.NewIter(start, end)
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var keys [][]byte
	for it.Valid() {
		key := it.Key()
		if len(key) < 2 {
			it.Next()
			continue
		}
		rec, err := decodeLock(it.Value())
		if err != nil {
			return nil, fmt.Errorf("mvcc: decode lock for %q: %w", key[1:], err)
		}
		if rec.startTS == startTS {
			keys = append(keys, append([]byte(nil), key[1:]...))
		}
		it.Next()
	}
	if err := it.Error(); err != nil {
		return nil, err
	}
	return keys, nil
}
