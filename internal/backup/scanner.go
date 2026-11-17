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

// ErrRemoteLockAwareScanUnavailable means a RemoteScanner reached a KV node
// that does not implement KVService.ScanForBackup and best-effort fallback was
// not explicitly enabled. It is a hard error: a backup configured to require a
// lock-aware scan must fail closed rather than silently downgrade to the plain
// Scan path.
var ErrRemoteLockAwareScanUnavailable = errors.New("backup: remote server-side lock-aware scan is unavailable")

// SnapshotScanner is the narrow lock-aware scan API used by Backup. A
// ScanForBackup implementation must return the visible key/value set at ts and
// fail with an error matching mvcc.ErrLocked (or containing
// *mvcc.SnapshotLockError) if a lock at or below ts is present.
//
// StoreScanner is the in-process implementation and RemoteScanner is the
// RPC-backed one. RemoteScanner needs a node that has wired
// kvserver.RegisterKVServiceWithBackupScan; until then an old registration
// answers Unimplemented and RemoteScanner fails closed by default. The default
// path in Backup without a Scanner has a narrower, explicitly documented guard
// (see BackupConfig.Scanner).
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

// WaitApplied implements BackupBarrier by delegating to the store's commit
// barrier. A StoreScanner is therefore self-sufficient for
// BackupConfig.RequireLockAwareScan: it supplies both the lock-aware snapshot
// scan and the applied-commit wait. RemoteScanner deliberately does not, because
// it has no server-side barrier RPC yet.
func (s StoreScanner) WaitApplied(ctx context.Context, ts uint64) error {
	return StoreBarrier{Store: s.Store}.WaitApplied(ctx, ts)
}

// RemoteScanner adapts a remote v2 cluster to SnapshotScanner through the
// KVService.ScanForBackup RPC. It is the remote counterpart of StoreScanner and
// is the only adapter that can see locks on keys that have never been
// committed, because the server reads the lock column itself.
//
// The zero value is strict: if an older node answers Unimplemented, ScanForBackup
// returns ErrRemoteLockAwareScanUnavailable and Backup aborts. Set
// AllowBestEffortFallback only for an explicitly best-effort backup: the
// adapter then falls back to the historical Scan RPC at the same snapshot TSO
// and probes the returned keys with CheckTxnStatus. That fallback still sees a
// lock on a previously committed key, but it cannot see a lock-only key that
// has no committed value yet, so it is not equivalent to the RPC. Do not set
// AllowBestEffortFallback together with BackupConfig.RequireLockAwareScan.
//
// RangeID is intentionally not part of this adapter. Backup calls the scanner
// once per range and passes that range's exact start/end bounds, so the client
// sends range_id=0 and the server resolves the owning range from start. That
// keeps split/merge epoch checks authoritative instead of pinning a stale
// descriptor.
type RemoteScanner struct {
	Client *v2client.Client
	// AllowBestEffortFallback permits the non-lock-aware Scan fallback when
	// the server does not implement ScanForBackup. It defaults to false and
	// must be an explicit operator choice.
	AllowBestEffortFallback bool
}

// ScanForBackup implements SnapshotScanner.
func (s RemoteScanner) ScanForBackup(ctx context.Context, start, end []byte, ts uint64, limit int) ([]v2client.KV, error) {
	if s.Client == nil {
		return nil, errors.New("backup: RemoteScanner has a nil Client")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := s.Client.ScanForBackup(ctx, 0, start, end, ts, limit)
	if err == nil {
		return rows, nil
	}
	if !errors.Is(err, v2client.ErrBackupScanUnavailable) {
		return nil, err
	}
	if !s.AllowBestEffortFallback {
		return nil, fmt.Errorf("%w: %w", ErrRemoteLockAwareScanUnavailable, err)
	}

	// Explicit best-effort path. This is the same plain Scan + per-key lock
	// probe that Backup uses when no SnapshotScanner is configured, but it
	// runs at the caller's stable backup TSO instead of allocating a new one.
	rows, scanErr := s.Client.ScanBestEffort(ctx, start, end, ts, limit)
	if scanErr != nil {
		return nil, fmt.Errorf("backup: remote best-effort scan: %w", scanErr)
	}
	if err := probeLocks(ctx, s.Client, rows, ts); err != nil {
		return nil, err
	}
	return rows, nil
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
// returned by the plain Scan, so this fallback cannot see it. Use
// RemoteScanner (with AllowBestEffortFallback false) for the full guarantee;
// this probe only protects RemoteScanner's explicit best-effort fallback.
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
