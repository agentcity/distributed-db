package kvserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

const (
	// defaultCommitBatchMax is the maximum number of CommitSingle commands
	// coalesced into one Raft entry. It matches Raft's own proposal batch cap
	// so one application batch also fits into one durable log append.
	defaultCommitBatchMax = 256
	// defaultCommitBatchWindow bounds how long the first CommitSingle of a
	// batch waits for concurrent requests to join. It is deliberately tiny:
	// every 1PC commit pays it once.
	defaultCommitBatchWindow = 500 * time.Microsecond
	// defaultMaxInflightBatches bounds how many application batches may be
	// proposed concurrently. Pipelining them keeps high-concurrency rounds
	// from serialising on one Raft round trip, while the cap keeps the number
	// of transient goroutines, results and unapplied entries small.
	defaultMaxInflightBatches = 4

	// batchResultTTL and maxBatchResults bound the per-state-machine result
	// memory. Only the leader's batcher consumes results; replicas that never
	// lead would otherwise retain one result set per batch forever.
	batchResultTTL  = 30 * time.Second
	maxBatchResults = 1024
)

// txnBatch is the JSON payload stored in model.Entry.Value for a
// model.CommandTxnBatch entry. The commands are full applyCommand values so
// every command keeps its own startTS/commitTS/primary/mutations.
type txnBatch struct {
	ID       uint64         `json:"id"`
	Commands []applyCommand `json:"commands"`
}

// batchResult is the per-command outcome of one applied batch. Errors are kept
// in memory (not serialized into the Raft entry) and handed to the waiting
// leader-side callers.
type batchResult struct {
	errs []error
	at   time.Time
}

// batchResultStore is a bounded, TTL-cleaned map of batch ID to per-command
// outcomes. A batch's result is consumed exactly once by the proposing
// batcher; results that are never consumed (replicas that never become
// leader) are dropped after batchResultTTL or when the map exceeds
// maxBatchResults.
type batchResultStore struct {
	mu      sync.Mutex
	results map[uint64]batchResult
	order   []uint64
}

func (s *batchResultStore) put(id uint64, errs []error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.results == nil {
		s.results = make(map[uint64]batchResult, 8)
	}
	s.results[id] = batchResult{errs: append([]error(nil), errs...), at: now}
	s.order = append(s.order, id)

	// Drop expired results and, if a replica still holds more than the cap
	// (for example a burst of batches applied while it was a follower), drop
	// the oldest applied results first.
	for len(s.order) > 0 {
		head := s.order[0]
		res, ok := s.results[head]
		if !ok {
			s.order = s.order[1:]
			continue
		}
		if now.Sub(res.at) <= batchResultTTL && len(s.results) <= maxBatchResults {
			break
		}
		delete(s.results, head)
		s.order = s.order[1:]
	}
}

func (s *batchResultStore) take(id uint64) ([]error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.results[id]
	if !ok {
		return nil, false
	}
	delete(s.results, id)
	return append([]error(nil), res.errs...), true
}

// proposeFunc matches raft.Node.Propose. It is a field so tests can inject a
// failing or blocking proposal without a real Raft node.
type proposeFunc func(ctx context.Context, entry model.Entry) (uint64, error)

// batchWaiter is one CommitSingle request waiting for its own result. The
// result channel is buffered so a waiter that gave up on its context can never
// block the batcher.
type batchWaiter struct {
	cmd    applyCommand
	result chan error
}

func (w *batchWaiter) complete(err error) { w.result <- err }

// commitBatcher coalesces concurrent CommitSingle commands of one range into a
// single model.CommandTxnBatch Raft entry.
//
// One goroutine (run) collects batches; a bounded number of worker goroutines
// propose them. Waiters hand their command over on submitCh and then wait on
// their private result channel, so a canceled waiter never affects the rest of
// its batch. The batcher uses its own context, tied to the group lifetime, for
// the shared proposals: one caller's cancellation cannot abort a batch that
// other callers depend on.
type commitBatcher struct {
	g *rangeGroup

	window   time.Duration
	maxBatch int

	submitCh chan *batchWaiter
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once

	// inflight is a semaphore bounding concurrently proposed batches; workers
	// tracks them so stop waits for every proposal goroutine to finish.
	inflight chan struct{}
	workers  sync.WaitGroup

	ctx    context.Context
	cancel context.CancelFunc

	nextBatchID atomic.Uint64
	proposeFn   atomic.Pointer[proposeFunc]

	// Counters exposed for tests and benchmarks. batchProposals counts
	// CommandTxnBatch entries proposed, singleProposals counts the
	// one-command fallback to CommandPrewriteCommit, and commandsBatched
	// counts commands accepted into multi-command batches.
	batchProposals  atomic.Uint64
	singleProposals atomic.Uint64
	commandsBatched atomic.Uint64
}

func newCommitBatcher(g *rangeGroup) *commitBatcher {
	ctx, cancel := context.WithCancel(context.Background())
	b := &commitBatcher{
		g:        g,
		window:   defaultCommitBatchWindow,
		maxBatch: defaultCommitBatchMax,
		submitCh: make(chan *batchWaiter),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		inflight: make(chan struct{}, defaultMaxInflightBatches),
		ctx:      ctx,
		cancel:   cancel,
	}
	// Batch IDs only need to be unique within this process. A time-derived
	// base avoids clashing with result entries left by an earlier leader.
	b.nextBatchID.Store(uint64(time.Now().UnixNano()))
	go b.run()
	return b
}

// submit enqueues one 1PC command and waits for its individual result. If ctx
// ends first the command may still be applied (the batch is already built),
// exactly like a timed-out raft Propose; other waiters are unaffected.
func (b *commitBatcher) submit(ctx context.Context, cmd applyCommand) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w := &batchWaiter{cmd: cmd, result: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.stopCh:
		return raft.ErrStopped
	case b.submitCh <- w:
	}

	// The batcher owns w now and always completes it, including during stop,
	// so waiting only for the result keeps the outcome unambiguous.
	select {
	case err := <-w.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stop ends the batcher goroutine and lets in-flight batches fail through
// their proposal context. It waits for the goroutine to exit, so a group stop
// leaves no batcher behind.
func (b *commitBatcher) stop() {
	b.stopOnce.Do(func() {
		close(b.stopCh)
		b.cancel()
		<-b.doneCh
	})
}

func (b *commitBatcher) run() {
	defer func() {
		// In-flight proposals use b.ctx, which stop cancels, so they finish
		// before doneCh closes. This is what makes stop leak-free.
		b.workers.Wait()
		close(b.doneCh)
	}()
	for {
		select {
		case <-b.stopCh:
			return
		case first := <-b.submitCh:
			batch := b.collect(first)
			if len(batch) == 0 {
				continue
			}
			// Bound the number of concurrently proposed batches. Blocking
			// here applies backpressure to submitters instead of spawning an
			// unbounded number of goroutines.
			select {
			case b.inflight <- struct{}{}:
			case <-b.stopCh:
				b.completeAll(batch, raft.ErrStopped)
				return
			}
			b.workers.Add(1)
			go func() {
				defer b.workers.Done()
				defer func() { <-b.inflight }()
				b.process(batch)
			}()
		}
	}
}

// collect drains up to maxBatch waiters or waits for the batching window,
// whichever happens first. On stop it fails the already-collected waiters so
// every accepted request gets a result.
func (b *commitBatcher) collect(first *batchWaiter) []*batchWaiter {
	batch := make([]*batchWaiter, 0, b.maxBatch)
	batch = append(batch, first)
	timer := time.NewTimer(b.window)
	defer timer.Stop()
	for len(batch) < b.maxBatch {
		select {
		case w := <-b.submitCh:
			batch = append(batch, w)
		case <-timer.C:
			return batch
		case <-b.stopCh:
			for _, w := range batch {
				w.complete(raft.ErrStopped)
			}
			return nil
		}
	}
	return batch
}

func (b *commitBatcher) process(batch []*batchWaiter) {
	if len(batch) == 1 {
		// A lone request is cheaper as a plain 1PC entry: no batch ID, no
		// result map entry, and the state-machine error comes back through
		// Propose as before.
		b.singleProposals.Add(1)
		cmd := batch[0].cmd
		payload, err := encodeCommand(cmd)
		if err == nil {
			_, err = b.propose(b.ctx, model.Entry{Type: model.CommandPrewriteCommit, Value: payload})
		}
		batch[0].complete(err)
		return
	}

	cmds := make([]applyCommand, len(batch))
	for i, w := range batch {
		cmds[i] = w.cmd
	}
	id := b.nextBatchID.Add(1)
	payload, err := json.Marshal(txnBatch{ID: id, Commands: cmds})
	if err != nil {
		b.completeAll(batch, fmt.Errorf("kvserver: encode txn batch: %w", err))
		return
	}
	b.batchProposals.Add(1)
	b.commandsBatched.Add(uint64(len(batch)))
	if _, err := b.propose(b.ctx, model.Entry{Type: model.CommandTxnBatch, Value: payload}); err != nil {
		// The whole entry did not commit/apply: every command in it failed.
		b.completeAll(batch, err)
		return
	}

	errs, ok := b.g.sm.batchResults.take(id)
	if !ok {
		b.completeAll(batch, fmt.Errorf("kvserver: txn batch %d: results missing", id))
		return
	}
	if len(errs) != len(batch) {
		b.completeAll(batch, fmt.Errorf("kvserver: txn batch %d: got %d results for %d commands",
			id, len(errs), len(batch)))
		return
	}
	for i, w := range batch {
		w.complete(errs[i])
	}
}

func (b *commitBatcher) completeAll(batch []*batchWaiter, err error) {
	for _, w := range batch {
		w.complete(err)
	}
}

func (b *commitBatcher) propose(ctx context.Context, entry model.Entry) (uint64, error) {
	if fn := b.proposeFn.Load(); fn != nil {
		return (*fn)(ctx, entry)
	}
	return b.g.node.Propose(ctx, entry)
}

// setProposeHook replaces the proposer for tests. Passing nil restores the
// real Raft node.
func (b *commitBatcher) setProposeHook(fn proposeFunc) {
	if fn == nil {
		b.proposeFn.Store(nil)
		return
	}
	b.proposeFn.Store(&fn)
}

// commitBatchStats is a snapshot of the batcher counters.
type commitBatchStats struct {
	Batches  uint64
	Singles  uint64
	Commands uint64
}

func (b *commitBatcher) stats() commitBatchStats {
	return commitBatchStats{
		Batches:  b.batchProposals.Load(),
		Singles:  b.singleProposals.Load(),
		Commands: b.commandsBatched.Load(),
	}
}

// setWindow changes the coalescing window. Tests use it to make concurrent
// submissions join one batch deterministically; it must not be called while
// requests are in flight.
func (b *commitBatcher) setWindow(d time.Duration) { b.window = d }
