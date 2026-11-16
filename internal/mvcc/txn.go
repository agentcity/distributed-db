package mvcc

import (
	"bytes"
	"fmt"

	"distributed-db/internal/storage"
)

// Prewrite validates a set of mutations and atomically writes their locks and
// provisional data versions at startTS.
//
// primary is copied into every lock so that a later lock-resolution milestone
// can find the transaction's coordinating lock. If primary is empty the first
// mutation key is used.
//
// Prewrite returns ErrLocked if another transaction holds a lock on a key, and
// ErrWriteConflict if the key was committed after startTS. Re-prewriting the
// same transaction (same startTS and primary) is allowed and overwrites its
// locks. If the engine rejects the atomic write, Prewrite makes a best-effort
// attempt to roll the partial transaction back and returns the engine error.
func (s *Store) Prewrite(mutations []Mutation, primary []byte, startTS uint64) error {
	if len(mutations) == 0 {
		return fmt.Errorf("mvcc: prewrite requires at least one mutation")
	}
	keys := make([][]byte, 0, len(mutations))
	seen := make(map[string]struct{}, len(mutations))
	for i := range mutations {
		if len(mutations[i].Key) == 0 {
			return fmt.Errorf("mvcc: prewrite mutation has an empty key")
		}
		if _, dup := seen[string(mutations[i].Key)]; dup {
			return fmt.Errorf("mvcc: prewrite has duplicate key %q", mutations[i].Key)
		}
		seen[string(mutations[i].Key)] = struct{}{}
		keys = append(keys, mutations[i].Key)
	}
	if len(primary) == 0 {
		primary = mutations[0].Key
	}

	// Validate everything before writing so that a conflict or a bad lock
	// leaves no partial state behind.
	for i := range mutations {
		key := mutations[i].Key

		lock, locked, err := s.lockAt(key)
		if err != nil {
			return err
		}
		if locked {
			if lock.startTS != startTS {
				return ErrLocked
			}
			if !bytes.Equal(lock.primary, primary) {
				return fmt.Errorf("mvcc: transaction %d re-prewrite with a different primary", startTS)
			}
		}

		conflict, err := s.hasCommittedAfter(key, startTS)
		if err != nil {
			return err
		}
		if conflict {
			return ErrWriteConflict
		}
	}

	batch := s.engine.NewBatch()
	fail := func(err error) error {
		_ = batch.Close()
		return err
	}

	for i := range mutations {
		mutation := &mutations[i]
		kind := kindPut
		var value []byte
		if mutation.Delete {
			kind = kindDelete
		} else {
			value = append([]byte(nil), mutation.Value...)
		}

		lockValue, err := encodeLock(lockRecord{
			startTS:   startTS,
			kind:      kind,
			ttlMillis: defaultLockTTLMillis,
			primary:   primary,
			value:     value,
		})
		if err != nil {
			return fail(err)
		}
		if err := batch.Set(lockKey(mutation.Key), lockValue); err != nil {
			return fail(err)
		}

		if mutation.Delete {
			continue
		}
		// Never overwrite a committed version that happens to occupy this
		// exact timestamp (callers normally never reuse timestamps, but the
		// encoding must stay safe if they do). Commit reads the value from the
		// lock, so the provisional copy is only an optimisation/GC aid.
		rec, found, err := s.writeAt(mutation.Key, startTS)
		if err != nil {
			return fail(err)
		}
		if found && rec.kind != kindRollback {
			continue
		}
		if err := batch.Set(dataKey(mutation.Key, startTS), value); err != nil {
			return fail(err)
		}
	}

	if err := batch.Commit(); err != nil {
		_ = batch.Close()
		s.rollbackPrewrite(keys, startTS)
		return fmt.Errorf("mvcc: prewrite: %w", err)
	}
	return batch.Close()
}

// Commit turns the locks for keys into committed versions at commitTS.
//
// It verifies that every supplied key is still locked by startTS (or already
// committed at commitTS), then atomically writes the data versions, write
// records and deletes the locks. Committing an already committed transaction at
// the same commitTS is a no-op returning nil; committing it at a different
// commitTS returns ErrAlreadyCommitted. A lock owned by another transaction
// yields ErrLocked.
func (s *Store) Commit(keys [][]byte, startTS, commitTS uint64) error {
	keys, err := normalizeKeys(keys)
	if err != nil {
		return err
	}
	if commitTS <= startTS {
		return fmt.Errorf("mvcc: commitTS %d must be greater than startTS %d", commitTS, startTS)
	}

	type commitPlan struct {
		key     []byte
		lock    lockRecord
		hasLock bool
	}
	plans := make([]commitPlan, 0, len(keys))
	anyLock := false

	for _, key := range keys {
		lock, locked, err := s.lockAt(key)
		if err != nil {
			return err
		}
		if locked {
			if lock.startTS != startTS {
				return ErrLocked
			}
			// A different transaction must not reuse our commitTS.
			if rec, found, err := s.writeAt(key, commitTS); err != nil {
				return err
			} else if found && rec.kind != kindRollback && rec.startTS != startTS {
				return ErrWriteConflict
			}
			plans = append(plans, commitPlan{key: key, lock: lock, hasLock: true})
			anyLock = true
			continue
		}

		// No lock: this key may already have been committed, either by an
		// earlier successful Commit or by a retry after a partial commit.
		rec, found, err := s.writeAt(key, commitTS)
		if err != nil {
			return err
		}
		if found && rec.kind != kindRollback {
			if rec.startTS == startTS {
				continue
			}
			return ErrWriteConflict
		}
		if rec, found, err := s.writeAt(key, startTS); err != nil {
			return err
		} else if found && rec.kind == kindRollback {
			return fmt.Errorf("mvcc: transaction %d was rolled back", startTS)
		}
		committed, err := s.hasCommitForStart(key, startTS)
		if err != nil {
			return err
		}
		if committed {
			return ErrAlreadyCommitted
		}
		return fmt.Errorf("mvcc: no lock for key %q", key)
	}

	if !anyLock {
		return nil
	}

	batch := s.engine.NewBatch()
	fail := func(err error) error {
		_ = batch.Close()
		return err
	}
	for i := range plans {
		plan := &plans[i]
		if !plan.hasLock {
			continue
		}

		if plan.lock.kind == kindPut {
			value := append([]byte(nil), plan.lock.value...)
			if err := batch.Set(dataKey(plan.key, commitTS), value); err != nil {
				return fail(err)
			}
		} else {
			if err := batch.Delete(dataKey(plan.key, commitTS)); err != nil {
				return fail(err)
			}
		}

		writeValue := encodeWrite(writeRecord{kind: plan.lock.kind, startTS: startTS})
		if err := batch.Set(writeKey(plan.key, commitTS), writeValue); err != nil {
			return fail(err)
		}
		if err := batch.Delete(lockKey(plan.key)); err != nil {
			return fail(err)
		}
		// Delete our provisional data and any rollback record left at startTS.
		if err := s.cleanupStartVersion(batch, plan.key, startTS); err != nil {
			return fail(err)
		}
	}

	if err := batch.Commit(); err != nil {
		_ = batch.Close()
		return fmt.Errorf("mvcc: commit: %w", err)
	}
	return batch.Close()
}

// Rollback removes the transaction's locks and writes rollback records at
// startTS. It is idempotent: rolling back an already rolled back or never
// prewritten key is a no-op. Rolling back a committed transaction returns
// ErrAlreadyCommitted, and rolling back a key locked by another transaction
// returns ErrLocked.
func (s *Store) Rollback(keys [][]byte, startTS uint64) error {
	keys, err := normalizeKeys(keys)
	if err != nil {
		return err
	}

	for _, key := range keys {
		lock, locked, err := s.lockAt(key)
		if err != nil {
			return err
		}
		if locked && lock.startTS != startTS {
			return ErrLocked
		}
		committed, err := s.hasCommitForStart(key, startTS)
		if err != nil {
			return err
		}
		if committed {
			return ErrAlreadyCommitted
		}
		// Refuse to overwrite a committed version that occupies this exact
		// timestamp; that can only happen when callers reuse timestamps.
		if rec, found, err := s.writeAt(key, startTS); err != nil {
			return err
		} else if found && rec.kind != kindRollback {
			return fmt.Errorf("mvcc: rollback timestamp %d already holds a committed version", startTS)
		}
	}

	batch := s.engine.NewBatch()
	fail := func(err error) error {
		_ = batch.Close()
		return err
	}
	for _, key := range keys {
		if err := batch.Delete(lockKey(key)); err != nil {
			return fail(err)
		}
		if err := s.cleanupStartVersion(batch, key, startTS); err != nil {
			return fail(err)
		}
		rollback := encodeWrite(writeRecord{kind: kindRollback, startTS: startTS})
		if err := batch.Set(writeKey(key, startTS), rollback); err != nil {
			return fail(err)
		}
	}

	if err := batch.Commit(); err != nil {
		_ = batch.Close()
		return fmt.Errorf("mvcc: rollback: %w", err)
	}
	return batch.Close()
}

// hasCommittedAfter reports whether key has a committed (put/delete) version
// with a commitTS greater than startTS.
func (s *Store) hasCommittedAfter(key []byte, startTS uint64) (bool, error) {
	start, end := versionedRange(writeCF, key)
	iter, err := s.engine.NewIter(start, end)
	if err != nil {
		return false, err
	}
	defer iter.Close()

	for iter.Valid() {
		userKey, commitTS, ok := decodeVersionedKey(iter.Key())
		if !ok || !bytes.Equal(userKey, key) || commitTS <= startTS {
			iter.Next()
			continue
		}
		rec, err := decodeWrite(iter.Value())
		if err != nil {
			return false, err
		}
		if rec.kind == kindPut || rec.kind == kindDelete {
			return true, nil
		}
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		return false, err
	}
	return false, nil
}

// hasCommitForStart reports whether key already has a committed version that
// belongs to the transaction identified by startTS.
func (s *Store) hasCommitForStart(key []byte, startTS uint64) (bool, error) {
	start, end := versionedRange(writeCF, key)
	iter, err := s.engine.NewIter(start, end)
	if err != nil {
		return false, err
	}
	defer iter.Close()

	for iter.Valid() {
		userKey, _, ok := decodeVersionedKey(iter.Key())
		if !ok || !bytes.Equal(userKey, key) {
			iter.Next()
			continue
		}
		rec, err := decodeWrite(iter.Value())
		if err != nil {
			return false, err
		}
		if rec.kind != kindRollback && rec.startTS == startTS {
			return true, nil
		}
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		return false, err
	}
	return false, nil
}

// cleanupStartVersion removes the provisional data version and stale rollback
// record at startTS. It leaves a committed version at that exact timestamp
// untouched.
func (s *Store) cleanupStartVersion(batch storage.Batch, key []byte, startTS uint64) error {
	rec, found, err := s.writeAt(key, startTS)
	if err != nil {
		return err
	}
	if found && rec.kind != kindRollback {
		return nil
	}
	if err := batch.Delete(dataKey(key, startTS)); err != nil {
		return err
	}
	if found {
		if err := batch.Delete(writeKey(key, startTS)); err != nil {
			return err
		}
	}
	return nil
}

// rollbackPrewrite is the best-effort cleanup used when the atomic prewrite
// batch fails. It never returns an error: the caller already has the primary
// failure, and a partial cleanup is handled by a later Rollback or GC.
func (s *Store) rollbackPrewrite(keys [][]byte, startTS uint64) {
	batch := s.engine.NewBatch()
	for _, key := range keys {
		_ = batch.Delete(lockKey(key))
		// Only touch the data version when no committed version owns it.
		rec, found, err := s.writeAt(key, startTS)
		if err != nil {
			_ = batch.Close()
			return
		}
		if found && rec.kind != kindRollback {
			continue
		}
		_ = batch.Delete(dataKey(key, startTS))
		_ = batch.Set(writeKey(key, startTS), encodeWrite(writeRecord{kind: kindRollback, startTS: startTS}))
	}
	if err := batch.Commit(); err != nil {
		_ = batch.Close()
		return
	}
	_ = batch.Close()
}
