package mvcc

import (
	"bytes"
	"fmt"

	"distributed-db/internal/storage"
)

// SingleCommand is one independent 1PC transaction in a CommitSingles call.
// Each command keeps its own startTS/commitTS/primary; the batch is not atomic
// across commands, but all successful commands share one durable engine batch
// (and therefore one fsync).
type SingleCommand struct {
	Mutations []Mutation
	Primary   []byte
	StartTS   uint64
	CommitTS  uint64
}

type singlePlan struct {
	key         []byte
	kind        byte
	value       []byte
	startTS     uint64
	commitTS    uint64
	hasLock     bool
	alreadyDone bool
}

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
	return s.CommitSingles([]SingleCommand{{
		Mutations: mutations,
		Primary:   primary,
		StartTS:   startTS,
		CommitTS:  commitTS,
	}})[0]
}

// CommitSingles applies several independent 1PC commands through one Pebble
// batch and one Commit/fsync. Per-command validation errors are returned in
// input order and do not prevent other commands in the batch from committing.
// If the shared batch cannot be staged or committed, every command that needed
// a write receives that error.
func (s *Store) CommitSingles(commands []SingleCommand) []error {
	errs := make([]error, len(commands))
	if len(commands) == 0 {
		return errs
	}

	plans := make([][]singlePlan, len(commands))
	needWrite := false
	seen := make(map[string]int)
	for i := range commands {
		cmd := &commands[i]
		for _, m := range cmd.Mutations {
			if prev, ok := seen[string(m.Key)]; ok && prev != i {
				// Retrying the same command twice in one Raft batch is
				// idempotent and must succeed. Different transactions
				// touching the same key in one batch are rejected because
				// their plans were validated against the pre-batch state.
				if !sameSingleCommand(commands[prev], commands[i]) {
					errs[i] = fmt.Errorf("mvcc: key %q appears in different commands in one batch", m.Key)
					break
				}
			}
			seen[string(m.Key)] = i
		}
		if errs[i] != nil {
			continue
		}
		p, err := s.planCommitSingle(cmd.Mutations, cmd.Primary, cmd.StartTS, cmd.CommitTS)
		if err != nil {
			errs[i] = err
			continue
		}
		if len(p) > 0 {
			plans[i] = p
			needWrite = true
		}
	}
	if !needWrite {
		return errs
	}

	batch := s.engine.NewBatch()
	failAll := func(err error) []error {
		_ = batch.Close()
		for i := range commands {
			if len(plans[i]) > 0 {
				errs[i] = err
			}
		}
		return errs
	}
	for i := range plans {
		if len(plans[i]) == 0 {
			continue
		}
		if err := s.applySinglePlans(batch, plans[i]); err != nil {
			return failAll(err)
		}
	}
	if err := batch.Commit(); err != nil {
		return failAll(fmt.Errorf("mvcc: commit singles: %w", err))
	}
	if err := batch.Close(); err != nil {
		return failAll(fmt.Errorf("mvcc: close commit singles batch: %w", err))
	}
	return errs
}

func sameSingleCommand(a, b SingleCommand) bool {
	if a.StartTS != b.StartTS || a.CommitTS != b.CommitTS || !bytes.Equal(a.Primary, b.Primary) {
		return false
	}
	if len(a.Mutations) != len(b.Mutations) {
		return false
	}
	for i := range a.Mutations {
		x, y := a.Mutations[i], b.Mutations[i]
		if x.Delete != y.Delete || !bytes.Equal(x.Key, y.Key) || !bytes.Equal(x.Value, y.Value) {
			return false
		}
	}
	return true
}

func (s *Store) planCommitSingle(mutations []Mutation, primary []byte, startTS, commitTS uint64) ([]singlePlan, error) {
	if len(mutations) == 0 {
		return nil, fmt.Errorf("mvcc: commit single requires at least one mutation")
	}
	if commitTS <= startTS {
		return nil, fmt.Errorf("mvcc: commitTS %d must be greater than startTS %d", commitTS, startTS)
	}
	seen := make(map[string]struct{}, len(mutations))
	for i := range mutations {
		key := mutations[i].Key
		if len(key) == 0 {
			return nil, fmt.Errorf("mvcc: commit single mutation has an empty key")
		}
		if _, dup := seen[string(key)]; dup {
			return nil, fmt.Errorf("mvcc: commit single has duplicate key %q", key)
		}
		seen[string(key)] = struct{}{}
	}
	if len(primary) == 0 {
		primary = mutations[0].Key
	}

	plans := make([]singlePlan, 0, len(mutations))
	for i := range mutations {
		mutation := &mutations[i]
		key := mutation.Key

		lock, locked, err := s.lockAt(key)
		if err != nil {
			return nil, err
		}
		if locked {
			if lock.startTS != startTS {
				return nil, ErrLocked
			}
			if !bytes.Equal(lock.primary, primary) {
				return nil, fmt.Errorf("mvcc: transaction %d commit single with a different primary", startTS)
			}
		}

		// A write record at commitTS owned by this transaction means the
		// commit already happened. Retrying is a no-op, except that a lock
		// left by an earlier prewrite must still be removed.
		rec, found, err := s.writeAt(key, commitTS)
		if err != nil {
			return nil, err
		}
		if found && rec.kind != kindRollback {
			if rec.startTS != startTS {
				return nil, ErrWriteConflict
			}
			if locked {
				plans = append(plans, singlePlan{key: key, startTS: startTS, hasLock: true, alreadyDone: true})
			}
			continue
		}
		if found {
			// A rollback record occupies commitTS even though it carries no
			// committed data. Overwriting it would reuse another transaction's
			// timestamp.
			return nil, fmt.Errorf("mvcc: commit timestamp %d already holds a rollback record", commitTS)
		}

		// Our transaction must not have been rolled back.
		startRec, startFound, err := s.writeAt(key, startTS)
		if err != nil {
			return nil, err
		}
		if startFound && startRec.kind == kindRollback {
			return nil, fmt.Errorf("mvcc: transaction %d was rolled back", startTS)
		}

		// Any committed version newer than startTS makes this transaction's
		// snapshot stale, exactly as Prewrite would reject it.
		conflict, err := s.hasCommittedAfter(key, startTS)
		if err != nil {
			return nil, err
		}
		if conflict {
			return nil, ErrWriteConflict
		}

		kind := kindPut
		var value []byte
		if mutation.Delete {
			kind = kindDelete
		} else {
			value = append([]byte(nil), mutation.Value...)
		}
		plans = append(plans, singlePlan{
			key:      key,
			kind:     kind,
			value:    value,
			startTS:  startTS,
			commitTS: commitTS,
			hasLock:  locked,
		})
	}
	return plans, nil
}

func (s *Store) applySinglePlans(batch storage.Batch, plans []singlePlan) error {
	for i := range plans {
		plan := &plans[i]
		if plan.alreadyDone {
			if err := batch.Delete(lockKey(plan.key)); err != nil {
				return err
			}
			if err := s.cleanupStartVersion(batch, plan.key, plan.startTS); err != nil {
				return err
			}
			continue
		}

		if plan.kind == kindPut {
			if err := batch.Set(dataKey(plan.key, plan.commitTS), plan.value); err != nil {
				return err
			}
		} else {
			// A delete writes a tombstone write record and no data version,
			// matching Commit.
			if err := batch.Delete(dataKey(plan.key, plan.commitTS)); err != nil {
				return err
			}
		}
		writeValue := encodeWrite(writeRecord{kind: plan.kind, startTS: plan.startTS})
		if err := batch.Set(writeKey(plan.key, plan.commitTS), writeValue); err != nil {
			return err
		}
		if err := batch.Delete(lockKey(plan.key)); err != nil {
			return err
		}
		if plan.hasLock {
			if err := s.cleanupStartVersion(batch, plan.key, plan.startTS); err != nil {
				return err
			}
		}
	}
	return nil
}
