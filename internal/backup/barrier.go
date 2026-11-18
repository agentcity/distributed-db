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
		return fmt.Errorf("%w: StoreBarrier has a nil Store", ErrBackupBarrierUnavailable)
	}
	if err := b.Store.WaitApplied(ctx, ts); err != nil {
		return fmt.Errorf("backup: store apply barrier: %w", err)
	}
	return nil
}

// LeaseBarrier is the fail-closed BackupBarrier adapter that couples the
// store's commit-lease table with its applied-commit wait. Compared with
// StoreBarrier it adds three checks that turn an unavailable or self-blocking
// barrier into ErrBackupBarrierUnavailable instead of a silent pass or an
// unbounded wait:
//
//   - Store must be non-nil.
//   - ts must be a positive commit TSO.
//   - An optional caller-held allocation Lease must be above ts. A lease at or
//     below ts is the caller's own unapplied commit, so waiting for it would
//     never finish; the adapter refuses instead of deadlocking.
//
// WaitApplied then delegates to Store.WaitApplied. AppliedFrontier exposes the
// store's observed applied frontier for a caller that records it in an archive
// checkpoint or passes it to pitr.PlanRequest.BarrierFrontier. It is a
// conservative observation, not a global safe point.
type LeaseBarrier struct {
	Store *mvcc.Store
	// Lease is an optional commit/allocation lease already held by the caller.
	// It is never released by the barrier; ownership stays with the caller.
	Lease *mvcc.CommitLease
}

// WaitApplied implements BackupBarrier.
func (b LeaseBarrier) WaitApplied(ctx context.Context, ts uint64) error {
	if b.Store == nil {
		return fmt.Errorf("%w: LeaseBarrier has a nil Store", ErrBackupBarrierUnavailable)
	}
	if ts == 0 {
		return fmt.Errorf("%w: LeaseBarrier tso is zero", ErrBackupBarrierUnavailable)
	}
	if b.Lease != nil {
		leaseTS := b.Lease.CommitTS()
		if leaseTS == 0 {
			return fmt.Errorf("%w: caller-held commit lease has zero tso", ErrBackupBarrierUnavailable)
		}
		if leaseTS <= ts {
			return fmt.Errorf("%w: caller-held commit lease %d is at or below snapshot tso %d; release or abort it before waiting",
				ErrBackupBarrierUnavailable, leaseTS, ts)
		}
	}
	if err := b.Store.WaitApplied(ctx, ts); err != nil {
		return fmt.Errorf("backup: lease/apply barrier: %w", err)
	}
	return nil
}

// AppliedFrontier returns the store's current applied-commit frontier. A nil
// store returns zero. The value can move backwards when an older commit starts
// after a newer one was applied, so callers should use it as a bound only
// after a successful WaitApplied.
func (b LeaseBarrier) AppliedFrontier() uint64 {
	if b.Store == nil {
		return 0
	}
	return b.Store.AppliedTS()
}
