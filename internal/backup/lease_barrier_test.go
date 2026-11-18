package backup

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLeaseBarrierWaitsAndFailsClosed covers the 1PC barrier adapter: it waits
// for registered commits, refuses a self-blocking caller lease, and turns an
// unavailable store into ErrBackupBarrierUnavailable instead of silently
// succeeding.
func TestLeaseBarrierWaitsAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := newLocalMVCC(t)

	if err := (LeaseBarrier{}).WaitApplied(ctx, 10); !errors.Is(err, ErrBackupBarrierUnavailable) {
		t.Fatalf("nil-store error = %v, want ErrBackupBarrierUnavailable", err)
	}
	if err := (StoreBarrier{}).WaitApplied(ctx, 10); !errors.Is(err, ErrBackupBarrierUnavailable) {
		t.Fatalf("StoreBarrier nil-store error = %v, want ErrBackupBarrierUnavailable", err)
	}
	if err := (StoreScanner{}).WaitApplied(ctx, 10); !errors.Is(err, ErrBackupBarrierUnavailable) {
		t.Fatalf("StoreScanner nil-store error = %v, want ErrBackupBarrierUnavailable", err)
	}
	if err := (LeaseBarrier{Store: store}).WaitApplied(ctx, 0); !errors.Is(err, ErrBackupBarrierUnavailable) {
		t.Fatalf("zero-tso error = %v, want ErrBackupBarrierUnavailable", err)
	}

	// A commit at TSO 40 is allocated but not applied.
	lease := store.AcquireCommitLease(40)
	done := make(chan error, 1)
	go func() {
		done <- LeaseBarrier{Store: store}.WaitApplied(ctx, 40)
	}()
	select {
	case err := <-done:
		t.Fatalf("barrier returned %v while commit 40 was outstanding", err)
	case <-time.After(50 * time.Millisecond):
	}

	// A snapshot below the outstanding commit does not need to wait for it.
	if err := (LeaseBarrier{Store: store}).WaitApplied(ctx, 30); err != nil {
		t.Fatalf("WaitApplied(30): %v", err)
	}

	// The caller's own lease at the snapshot TSO would never become visible
	// while the caller waits on it, so the adapter rejects the deadlock.
	if err := (LeaseBarrier{Store: store, Lease: lease}).WaitApplied(ctx, 40); !errors.Is(err, ErrBackupBarrierUnavailable) {
		t.Fatalf("self-blocking lease error = %v, want ErrBackupBarrierUnavailable", err)
	}

	lease.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("barrier after release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("barrier did not return after the commit lease was released")
	}
	if got := (LeaseBarrier{Store: store}).AppliedFrontier(); got < 40 {
		t.Fatalf("AppliedFrontier = %d, want at least 40", got)
	}

	// A cancelled context fails closed while a lease is still outstanding.
	lease = store.AcquireCommitLease(60)
	defer lease.Abort()
	waitCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := (LeaseBarrier{Store: store}).WaitApplied(waitCtx, 60); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait error = %v, want context.Canceled", err)
	}
}
