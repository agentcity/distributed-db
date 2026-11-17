package mvcc

import (
	"container/heap"
	"context"
	"fmt"
	"sync"
)

// CommitLease is an in-memory reservation for a commit timestamp that has been
// allocated but whose commit batch is not known to be applied yet.
//
// A lease is acquired before a commitTS is exposed to a client (or before the
// commit is proposed/applied) and released once the commit batch is visible in
// the engine. While the lease is outstanding, Store.WaitApplied blocks snapshot
// barriers at or above its commitTS.
//
// Release and Abort are terminal and idempotent; the first call wins. Release
// means "the commit is applied and visible", Abort means "this commit timestamp
// will not become visible" (rejected, rolled back, or failed before applying).
// Use Abort only for an outcome that can never later produce a visible version
// at this timestamp; a retry must acquire a fresh lease.
//
// A lease is process-local. It is not persisted and does not survive a restart;
// see the package documentation for the crash/recovery contract.
type CommitLease struct {
	barrier *commitBarrier
	ts      uint64
	once    sync.Once
}

// CommitTS returns the commit timestamp reserved by the lease.
func (l *CommitLease) CommitTS() uint64 {
	if l == nil {
		return 0
	}
	return l.ts
}

// Release marks the commit as applied/visible and removes the lease from the
// barrier. It is idempotent.
func (l *CommitLease) Release() {
	if l == nil || l.barrier == nil {
		return
	}
	l.once.Do(func() { l.barrier.release(l.ts, true) })
}

// Abort removes the lease without marking the commit as applied. It is used
// when the commit is rejected or fails before its batch is applied. It is
// idempotent.
func (l *CommitLease) Abort() {
	if l == nil || l.barrier == nil {
		return
	}
	l.once.Do(func() { l.barrier.release(l.ts, false) })
}

// commitBarrier tracks commits that own a commitTS but are not yet applied.
//
// The zero value is not usable; Store.New initialises it. All methods are safe
// for concurrent use.
type commitBarrier struct {
	mu sync.Mutex
	// pending maps a commitTS to the number of outstanding leases. More than
	// one lease can cover the same timestamp (for example a caller-held
	// allocation lease plus the commit call's own apply lease).
	pending map[uint64]int
	// total is the sum of pending values, used for O(1) emptiness checks.
	total int
	// minHeap is a lazy min-heap of outstanding timestamps. Entries are only
	// removed when they reach the top and no longer have a pending lease.
	minHeap tsHeap
	// maxApplied is the highest timestamp released as applied. It can move
	// ahead of lower timestamps that are still pending or were aborted.
	maxApplied uint64
	// changed is closed and replaced on every state change so waiters can
	// wake without polling and without holding the mutex.
	changed chan struct{}
}

func newCommitBarrier() *commitBarrier {
	return &commitBarrier{
		pending: make(map[uint64]int),
		changed: make(chan struct{}),
	}
}

// acquire registers commitTS as in flight. The caller must eventually Release
// or Abort the returned lease.
func (b *commitBarrier) acquire(ts uint64) *CommitLease {
	b.mu.Lock()
	b.pending[ts]++
	b.total++
	heap.Push(&b.minHeap, ts)
	b.broadcastLocked()
	b.mu.Unlock()
	return &CommitLease{barrier: b, ts: ts}
}

// release removes one lease for ts. When applied is true the commit is visible
// and ts becomes part of the applied frontier.
func (b *commitBarrier) release(ts uint64, applied bool) {
	b.mu.Lock()
	if n := b.pending[ts]; n > 1 {
		b.pending[ts] = n - 1
	} else {
		delete(b.pending, ts)
	}
	if b.total > 0 {
		b.total--
	}
	if applied && ts > b.maxApplied {
		b.maxApplied = ts
	}
	b.compactLocked()
	b.broadcastLocked()
	b.mu.Unlock()
}

// compactLocked rebuilds the lazy heap once stale entries dominate, so a
// long-lived lease plus a stream of later commits cannot grow the heap without
// bound. The amortised cost is O(1) per release.
func (b *commitBarrier) compactLocked() {
	if len(b.minHeap) <= 2*len(b.pending)+64 {
		return
	}
	compacted := make(tsHeap, 0, len(b.pending))
	for ts := range b.pending {
		compacted = append(compacted, ts)
	}
	heap.Init(&compacted)
	b.minHeap = compacted
}

func (b *commitBarrier) broadcastLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// minPendingLocked returns the smallest timestamp with an outstanding lease.
// The heap is pruned lazily: an entry is discarded only once it reaches the
// top and its pending count has dropped to zero.
func (b *commitBarrier) minPendingLocked() (uint64, bool) {
	for len(b.minHeap) > 0 {
		ts := b.minHeap[0]
		if b.pending[ts] > 0 {
			return ts, true
		}
		heap.Pop(&b.minHeap)
	}
	return 0, false
}

// wait blocks until no lease at or below ts is outstanding, or ctx is done.
// The barrier mutex is never held while waiting.
func (b *commitBarrier) wait(ctx context.Context, ts uint64) error {
	if ts == 0 {
		// Zero is not a TSO a caller can snapshot; there is nothing to wait
		// for and no commit can be at or below it.
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("mvcc: wait for commits applied through %d: %w", ts, err)
		}

		b.mu.Lock()
		min, ok := b.minPendingLocked()
		if !ok || min > ts {
			b.mu.Unlock()
			return nil
		}
		changed := b.changed
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return fmt.Errorf("mvcc: wait for commits applied through %d: %w", ts, ctx.Err())
		case <-changed:
		}
	}
}

// appliedTS returns a timestamp through which every registered commit is
// applied. It is capped by the highest applied timestamp so it never claims
// progress past what this process has actually observed.
func (b *commitBarrier) appliedTS() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if min, ok := b.minPendingLocked(); ok {
		if min == 0 {
			return 0
		}
		if min-1 < b.maxApplied {
			return min - 1
		}
	}
	return b.maxApplied
}

func (b *commitBarrier) pendingCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// AcquireCommitLease registers commitTS as a commit that received a commit
// timestamp but is not yet known to be applied. The returned lease must be
// released with Release (applied) or Abort (will not become visible).
//
// Callers that allocate a commitTS outside this package should acquire the
// lease before the timestamp is exposed (before the client is acknowledged or
// before the commit is proposed) and release it only after the commit batch is
// applied. This closes the window where a snapshot barrier at T could run
// between TSO allocation and CommitSingle/Commit and miss a commit at or below
// T. CommitSingle, CommitSingles and Commit acquire their own lease for the
// apply window, so an external lease is only needed across the allocation and
// retry window.
//
// Leases are in-memory only; see the package documentation for the restart
// contract.
func (s *Store) AcquireCommitLease(commitTS uint64) *CommitLease {
	return s.barrier.acquire(commitTS)
}

// WaitApplied blocks until every commit registered with a commitTS at or below
// ts is applied (its engine batch is visible), or until ctx is done. It returns
// a nil error when the barrier is clear and a wrapped ctx.Err() (test with
// errors.Is) when the wait is cancelled or times out.
//
// WaitApplied never holds an engine lock and never blocks commit progress: it
// only observes the in-memory lease table and waits on a change notification.
// A caller taking a consistent snapshot at ts should acquire leases before
// exposing commit timestamps, call WaitApplied(ctx, ts), and then scan. A
// timed-out wait fails closed: the snapshot must be retried or abandoned.
//
// The barrier is empty after a restart, so WaitApplied can return nil while
// recovery/replay is still restoring applied state. Callers must gate snapshot
// requests on recovery completion before relying on this barrier.
//
// ts == 0 is treated as "no snapshot point" and returns nil immediately.
func (s *Store) WaitApplied(ctx context.Context, ts uint64) error {
	return s.barrier.wait(ctx, ts)
}

// AppliedTS returns a timestamp through which every commit registered with the
// barrier is applied. It can move backwards when a commit with an older
// timestamp starts after a newer one was applied, so callers should prefer
// WaitApplied for a stable snapshot barrier.
//
// AppliedTS is only a safety statement about commits that acquired a lease; it
// says nothing about timestamps allocated by a caller that did not register
// them before exposing them. After a restart the barrier is empty and
// AppliedTS is 0 until commits are applied again; see the package
// documentation.
func (s *Store) AppliedTS() uint64 {
	return s.barrier.appliedTS()
}

// PendingCommits reports the number of outstanding commit leases. It is a
// diagnostic hook for tests and metrics; the lease table is process-local.
func (s *Store) PendingCommits() int {
	return s.barrier.pendingCount()
}

// tsHeap is a min-heap of commit timestamps, used to find the oldest in-flight
// commit without scanning the lease map.
type tsHeap []uint64

func (h tsHeap) Len() int            { return len(h) }
func (h tsHeap) Less(i, j int) bool  { return h[i] < h[j] }
func (h tsHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *tsHeap) Push(x interface{}) { *h = append(*h, x.(uint64)) }
func (h *tsHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
