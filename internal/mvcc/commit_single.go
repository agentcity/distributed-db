package mvcc

import (
	"bytes"
	"fmt"
)

// CommitSingle applies prewrite+commit for a single-range transaction in one
// atomic engine batch. It validates locks/conflicts first, then writes data and
// write records at commitTS and removes any locks, all in one commit.
//
// primary defaults to the first mutation key when empty, matching Prewrite. An
// existing lock owned by the same transaction (same startTS and primary) is
// accepted and removed; its provisional data at startTS is cleaned up. No
// provisional data is written for a fresh 1PC transaction, so readers never see
// an intermediate version and GC only has to consider the committed version.
//
// Repeating the call after a successful commit is idempotent: a write record at
// commitTS owned by startTS makes the affected keys no-ops (any stale lock is
// still removed). A lock held by another transaction yields ErrLocked, a
// committed version newer than startTS yields ErrWriteConflict, and a rolled
// back transaction is rejected. All validation and staging happen before the
// single batch Commit, so on any error the batch is closed and no state changes.
func (s *Store) CommitSingle(mutations []Mutation, primary []byte, startTS, commitTS uint64) error {
	if len(mutations) == 0 {
		return fmt.Errorf("mvcc: commit single requires at least one mutation")
	}
	if commitTS <= startTS {
		return fmt.Errorf("mvcc: commitTS %d must be greater than startTS %d", commitTS, startTS)
	}
	seen := make(map[string]struct{}, len(mutations))
	for i := range mutations {
		key := mutations[i].Key
		if len(key) == 0 {
			return fmt.Errorf("mvcc: commit single mutation has an empty key")
		}
		if _, dup := seen[string(key)]; dup {
			return fmt.Errorf("mvcc: commit single has duplicate key %q", key)
		}
		seen[string(key)] = struct{}{}
	}
	if len(primary) == 0 {
		primary = mutations[0].Key
	}

	type singlePlan struct {
		key         []byte
		kind        byte
		value       []byte
		hasLock     bool
		alreadyDone bool
	}

	plans := make([]singlePlan, 0, len(mutations))
	needBatch := false

	for i := range mutations {
		mutation := &mutations[i]
		key := mutation.Key

		lock, locked, err := s.lockAt(key)
		if err != nil {
			return err
		}
		if locked {
			if lock.startTS != startTS {
				return ErrLocked
			}
			if !bytes.Equal(lock.primary, primary) {
				return fmt.Errorf("mvcc: transaction %d commit single with a different primary", startTS)
			}
		}

		// A write record at commitTS owned by this transaction means the
		// commit already happened. Retrying is a no-op, except that a lock
		// left by an earlier prewrite must still be removed.
		rec, found, err := s.writeAt(key, commitTS)
		if err != nil {
			return err
		}
		if found && rec.kind != kindRollback {
			if rec.startTS != startTS {
				return ErrWriteConflict
			}
			if locked {
				plans = append(plans, singlePlan{key: key, hasLock: true, alreadyDone: true})
				needBatch = true
			}
			continue
		}
		if found {
			// A rollback record occupies commitTS even though it carries no
			// committed data. Overwriting it would reuse another transaction's
			// timestamp.
			return fmt.Errorf("mvcc: commit timestamp %d already holds a rollback record", commitTS)
		}

		// Our transaction must not have been rolled back.
		startRec, startFound, err := s.writeAt(key, startTS)
		if err != nil {
			return err
		}
		if startFound && startRec.kind == kindRollback {
			return fmt.Errorf("mvcc: transaction %d was rolled back", startTS)
		}

		// Any committed version newer than startTS makes this transaction's
		// snapshot stale, exactly as Prewrite would reject it.
		conflict, err := s.hasCommittedAfter(key, startTS)
		if err != nil {
			return err
		}
		if conflict {
			return ErrWriteConflict
		}

		kind := kindPut
		var value []byte
		if mutation.Delete {
			kind = kindDelete
		} else {
			value = append([]byte(nil), mutation.Value...)
		}
		plans = append(plans, singlePlan{key: key, kind: kind, value: value, hasLock: locked})
		needBatch = true
	}

	if !needBatch {
		return nil
	}

	batch := s.engine.NewBatch()
	fail := func(err error) error {
		_ = batch.Close()
		return err
	}

	for i := range plans {
		plan := &plans[i]
		if plan.alreadyDone {
			if err := batch.Delete(lockKey(plan.key)); err != nil {
				return fail(err)
			}
			if err := s.cleanupStartVersion(batch, plan.key, startTS); err != nil {
				return fail(err)
			}
			continue
		}

		if plan.kind == kindPut {
			if err := batch.Set(dataKey(plan.key, commitTS), plan.value); err != nil {
				return fail(err)
			}
		} else {
			// A delete writes a tombstone write record and no data version,
			// matching Commit.
			if err := batch.Delete(dataKey(plan.key, commitTS)); err != nil {
				return fail(err)
			}
		}
		writeValue := encodeWrite(writeRecord{kind: plan.kind, startTS: startTS})
		if err := batch.Set(writeKey(plan.key, commitTS), writeValue); err != nil {
			return fail(err)
		}
		if err := batch.Delete(lockKey(plan.key)); err != nil {
			return fail(err)
		}
		if plan.hasLock {
			// Upgrade from a prior 2PC prewrite: remove its provisional data
			// version and any stale rollback record at startTS.
			if err := s.cleanupStartVersion(batch, plan.key, startTS); err != nil {
				return fail(err)
			}
		}
	}

	if err := batch.Commit(); err != nil {
		_ = batch.Close()
		return fmt.Errorf("mvcc: commit single: %w", err)
	}
	return batch.Close()
}
