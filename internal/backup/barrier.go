package backup

import (
	"context"
	"errors"
	"fmt"

	"distributed-db/internal/mvcc"
)

// ErrBackupBarrierUnavailable means the backup requires a lock-aware,
// apply-barrier-protected snapshot but no BackupBarrier was configured.
// BackupConfig.RequireLockAwareScan fails closed with this error instead of
// silently taking a snapshot that could miss a commit at or below its TSO.
var ErrBackupBarrierUnavailable = errors.New("backup: applied-commit barrier is required but unavailable")

// BackupBarrier is the applied-commit/commit-lease barrier a backup waits on
// before scanning a snapshot. WaitApplied must block until every commit that
// was registered with a commit timestamp <= ts is visible in the engine (or
// return an error). *mvcc.Store has this method; StoreBarrier adapts one.
//
// The barrier closes the apply window: a commit whose commitTS was allocated
// and registered before WaitApplied is called cannot be missed by a scan at
// ts. It does not close the allocation window by itself. A commitTS can be
// allocated, but not yet registered, before WaitApplied starts; the allocator
// must hold an mvcc.CommitLease across allocation and apply (see
// mvcc.Store.AcquireCommitLease) for that stronger guarantee. A nil barrier
// means the backup makes neither claim.
type BackupBarrier interface {
	WaitApplied(ctx context.Context, ts uint64) error
}

// BarrierFunc adapts a function to BackupBarrier. It is useful for tests and
// for a caller that already owns a barrier implementation.
type BarrierFunc func(ctx context.Context, ts uint64) error

// WaitApplied implements BackupBarrier.
func (f BarrierFunc) WaitApplied(ctx context.Context, ts uint64) error {
	if f == nil {
		return errors.New("backup: nil BarrierFunc")
	}
	return f(ctx, ts)
}

// StoreBarrier adapts an in-process mvcc.Store to BackupBarrier. Its
// WaitApplied is Store.WaitApplied, which observes the store's commit-lease
// table without holding an engine lock.
type StoreBarrier struct {
	Store *mvcc.Store
}

// WaitApplied implements BackupBarrier.
func (b StoreBarrier) WaitApplied(ctx context.Context, ts uint64) error {
	if b.Store == nil {
		return errors.New("backup: StoreBarrier has a nil Store")
	}
	if err := b.Store.WaitApplied(ctx, ts); err != nil {
		return fmt.Errorf("backup: store apply barrier: %w", err)
	}
	return nil
}
