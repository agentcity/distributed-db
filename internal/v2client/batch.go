package v2client

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

const (
	// DefaultCommitBatchConcurrency is the number of transactions CommitBatch
	// commits at the same time when no explicit limit is given.
	DefaultCommitBatchConcurrency = 32
)

// errNilBatchTxn is returned in a CommitBatch result slot whose transaction is
// nil. It is a programming error, not a transaction outcome.
var errNilBatchTxn = errors.New("v2client: nil transaction in batch")

// errDuplicateBatchTxn is returned for every slot of a transaction that appears
// more than once in one batch. Such a batch is rejected before any commit
// starts, because two workers driving the same Txn would race on its state.
var errDuplicateBatchTxn = errors.New("v2client: transaction appears more than once in batch")

// CommitBatch commits independent transactions concurrently and returns one
// error per transaction in input order. It does not make the batch atomic:
// each Txn has its own 1PC/2PC commit point, so a failure in one transaction
// never rolls back another. A nil result slot means that transaction committed
// successfully.
//
// At most DefaultCommitBatchConcurrency transactions are in flight at once.
// Use CommitBatchWithLimit to choose a different bound.
//
// Each *Txn must be committed at most once and must not be used concurrently
// by the caller while it is part of a batch: Txn is not safe for concurrent
// use. Committing the same *Txn concurrently from two different batches is a
// data race. Repeating one *Txn inside a single batch is detected before any
// commit starts; every slot holding that transaction then reports a
// "transaction appears more than once in batch" error.
//
// A transaction that has already finished (committed or rolled back) returns
// its existing "v2client: transaction is finished" error for its slot. A nil
// transaction returns a non-nil error for its slot as well.
//
// Context cancellation stops transactions that have not started yet: their
// slots receive ctx.Err(). Commitments already in flight are attempted with the
// same context and may still complete or fail on their own; their real result
// is reported. CommitBatch itself does not return until every slot is filled.
func (c *Client) CommitBatch(ctx context.Context, txns []*Txn) []error {
	return c.CommitBatchWithLimit(ctx, txns, DefaultCommitBatchConcurrency)
}

// CommitBatchWithLimit is CommitBatch with an explicit concurrency limit. A
// limit <= 0 uses DefaultCommitBatchConcurrency; a limit larger than len(txns)
// is clamped to len(txns). The limit bounds the number of transactions being
// committed at once, not the number of goroutines created: the implementation
// starts at most limit workers.
func (c *Client) CommitBatchWithLimit(ctx context.Context, txns []*Txn, limit int) []error {
	errs := make([]error, len(txns))
	if len(txns) == 0 {
		return errs
	}
	if limit <= 0 {
		limit = DefaultCommitBatchConcurrency
	}
	if limit > len(txns) {
		limit = len(txns)
	}

	// Reject repeated transactions up front. This is the only case where two
	// workers could be handed the same Txn, and Txn is not concurrency-safe.
	seen := make(map[*Txn]struct{}, len(txns))
	dup := make(map[*Txn]struct{})
	for _, txn := range txns {
		if txn == nil {
			continue
		}
		if _, ok := seen[txn]; ok {
			dup[txn] = struct{}{}
			continue
		}
		seen[txn] = struct{}{}
	}
	skip := make([]bool, len(txns))
	if len(dup) > 0 {
		for i, txn := range txns {
			if _, ok := dup[txn]; ok {
				errs[i] = errDuplicateBatchTxn
				skip[i] = true
			}
		}
	}

	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(limit)
	for w := 0; w < limit; w++ {
		go func() {
			defer wg.Done()
			for {
				// Claim the next slot. Every index is owned by exactly one
				// worker, so writing errs[i] below needs no lock and the result
				// slice stays in input order regardless of completion order.
				i := int(next.Add(1)) - 1
				if i >= len(txns) {
					return
				}
				if skip[i] {
					continue
				}
				errs[i] = c.commitBatchTxn(ctx, txns[i])
			}
		}()
	}
	wg.Wait()
	return errs
}

// commitBatchTxn commits one slot. It checks the context immediately before the
// transaction starts, which is what makes the "not started means ctx.Err()"
// guarantee deterministic even when cancellation races with a worker becoming
// free.
func (c *Client) commitBatchTxn(ctx context.Context, txn *Txn) error {
	if txn == nil {
		return errNilBatchTxn
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return txn.Commit(ctx)
}
