package backup

import (
	"context"
	"errors"
	"fmt"

	"distributed-db/internal/mvcc"
	"distributed-db/internal/v2client"
)

// ErrSnapshotLocked means the backup snapshot is blocked by a lock whose
// startTS is at or below the snapshot TSO. A backup must never publish a
// snapshot while such a lock exists: the transaction may have been committed
// at or below that TSO but its commit batch may not be visible yet, so silently
// omitting the key would lose an acknowledged write.
var ErrSnapshotLocked = errors.New("backup: snapshot is blocked by an unresolved lock at or below the snapshot TSO")

// SnapshotScanner is the narrow lock-aware scan API used by Backup. A
// ScanForBackup implementation must return the visible key/value set at ts and
// fail with an error matching mvcc.ErrLocked (or containing
// *mvcc.SnapshotLockError) if a lock at or below ts is present.
//
// StoreScanner is the in-process implementation. A remote cluster needs a
// server-side lock-scan RPC to implement this interface; the default
// v2client-backed path in Backup has a narrower, explicitly documented guard
// (see BackupConfig.Scanner and the package documentation).
type SnapshotScanner interface {
	ScanForBackup(ctx context.Context, start, end []byte, ts uint64, limit int) ([]v2client.KV, error)
}

// StoreScanner adapts an in-process mvcc.Store to SnapshotScanner. Every call
// reads a single engine snapshot and checks the lock column before the write
// column, so a committed value cannot be hidden behind an unresolved lock
// within that snapshot (see mvcc.Store.ScanForBackup).
type StoreScanner struct {
	Store *mvcc.Store
}

// ScanForBackup implements SnapshotScanner.
func (s StoreScanner) ScanForBackup(ctx context.Context, start, end []byte, ts uint64, limit int) ([]v2client.KV, error) {
	if s.Store == nil {
		return nil, errors.New("backup: StoreScanner has a nil Store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kvs, err := s.Store.ScanForBackup(start, end, ts, limit)
	if err != nil {
		return nil, err
	}
	out := make([]v2client.KV, len(kvs))
	for i, kv := range kvs {
		out[i] = v2client.KV{Key: kv.Key, Value: kv.Value}
	}
	return out, nil
}

// SnapshotLockedError carries the first blocking lock observed by a backup
// attempt. It unwraps to both ErrSnapshotLocked and mvcc.ErrLocked so callers
// can use errors.Is with either sentinel.
type SnapshotLockedError struct {
	Key        []byte
	LockTS     uint64
	SnapshotTS uint64
	Attempts   int
	Cause      error
}

func (e *SnapshotLockedError) Error() string {
	msg := ErrSnapshotLocked.Error()
	if len(e.Key) > 0 {
		msg += fmt.Sprintf(": key %q", e.Key)
	}
	if e.LockTS > 0 {
		msg += fmt.Sprintf(" lock_ts=%d", e.LockTS)
	}
	if e.SnapshotTS > 0 {
		msg += fmt.Sprintf(" snapshot_ts=%d", e.SnapshotTS)
	}
	if e.Attempts > 0 {
		msg += fmt.Sprintf(" attempts=%d", e.Attempts)
	}
	if e.Cause != nil {
		msg += fmt.Sprintf(": %v", e.Cause)
	}
	return msg
}

// Unwrap exposes the stable sentinels and the underlying scanner error.
func (e *SnapshotLockedError) Unwrap() []error {
	errs := []error{ErrSnapshotLocked, mvcc.ErrLocked}
	if e.Cause != nil {
		errs = append(errs, e.Cause)
	}
	return errs
}

// lockedError converts a lock-aware scanner error into SnapshotLockedError.
func lockedError(err error, snapshotTS uint64) error {
	var lockErr *mvcc.SnapshotLockError
	if errors.As(err, &lockErr) {
		return &SnapshotLockedError{
			Key:        cloneBytes(lockErr.Key),
			LockTS:     lockErr.StartTS,
			SnapshotTS: snapshotTS,
			Cause:      err,
		}
	}
	return &SnapshotLockedError{SnapshotTS: snapshotTS, Cause: err}
}

// probeLocks checks every key returned by the default (non-lock-aware) remote
// scan for a lock at or below snapshotTS. It is a guard for the keys that were
// actually observed: CheckTxnStatus exposes locks per key, not a lock-column
// scan. A key that has never been committed and is currently locked is not
// returned by Txn.Scan, so this fallback cannot see it; callers that need the
// full guarantee must supply a SnapshotScanner (the server-side lock-scan RPC
// is the remaining wiring, see the package documentation).
func probeLocks(ctx context.Context, client *v2client.Client, rows []v2client.KV, snapshotTS uint64) error {
	for _, kv := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		status, err := client.CheckTxnStatus(ctx, kv.Key, 0)
		if err != nil {
			return fmt.Errorf("backup: probe lock on key %q: %w", kv.Key, err)
		}
		if lockTS := status.GetLockTs(); lockTS != 0 && lockTS <= snapshotTS {
			return &SnapshotLockedError{
				Key:        cloneBytes(kv.Key),
				LockTS:     lockTS,
				SnapshotTS: snapshotTS,
			}
		}
	}
	return nil
}
