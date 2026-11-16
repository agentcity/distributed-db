package v2client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	pbv2 "distributed-db/internal/pb/v2"
)

const (
	// defaultTSOBatchSize is the number of timestamps reserved by one meta
	// AllocTS call. A batch is consumed locally before the next RPC.
	defaultTSOBatchSize int32 = 128
	// defaultTSOBatchTTL bounds how long a partially consumed batch may be
	// used before it is refreshed.
	defaultTSOBatchTTL = time.Second
	// defaultTSOIdleCheck forces a meta leader probe (not a new allocation)
	// when the batch has been idle, so a failover is noticed quickly.
	defaultTSOIdleCheck = 50 * time.Millisecond
	// defaultTSOValidateEvery forces a leader probe after this many local
	// allocations, bounding how long a busy client can keep using a batch
	// allocated by an old meta leader.
	defaultTSOValidateEvery uint64 = 64
	tsoRetryDelay                  = 50 * time.Millisecond
)

// tsoBatcher holds the client-local TSO reservation. All fields are guarded by
// mu; inflight ensures that at most one goroutine refreshes the batch.
type tsoBatcher struct {
	mu         sync.Mutex
	next       uint64 // next timestamp to hand out
	end        uint64 // exclusive end of the current reservation
	expires    time.Time
	gen        uint64 // meta generation the batch was allocated from
	lastUsed   time.Time
	served     uint64 // timestamps handed out since the last leader check
	lastIssued uint64 // highest timestamp handed to a caller
	inflight   *tsoCall
}

type tsoCall struct {
	done chan struct{}
	err  error
}

// AllocTS reserves one timestamp. Timestamps are fetched from meta in batches
// and consumed locally; a batch is refilled when it is empty, older than the
// batch TTL, or the meta leader generation changes. The returned value is
// strictly greater than every value previously returned by this Client.
func (c *Client) AllocTS(ctx context.Context) (uint64, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return 0, fmt.Errorf("v2client: alloc ts: %w", lastErr)
			}
			return 0, err
		}

		b := &c.tso
		now := time.Now()
		b.mu.Lock()
		if call := b.inflight; call != nil {
			b.mu.Unlock()
			select {
			case <-call.done:
				if call.err != nil {
					lastErr = call.err
					if errors.Is(call.err, errClientClosed) {
						return 0, call.err
					}
				}
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			continue
		}

		gen := c.metaGeneration()
		valid := b.next != 0 && b.next < b.end && now.Before(b.expires) && b.gen == gen
		if valid && (now.Sub(b.lastUsed) >= c.tsoIdleThreshold() || b.served >= c.tsoValidateLimit()) {
			// Keep the batch, but make sure the meta leader has not moved.
			// refillTSBatch only probes and reuses the reservation when the
			// generation is unchanged.
		} else if valid {
			ts := b.next
			b.next++
			b.lastUsed = now
			b.served++
			if ts > b.lastIssued {
				b.lastIssued = ts
				b.mu.Unlock()
				return ts, nil
			}
			// Defensive: never hand out a timestamp that is not newer than
			// one already returned. Drop the remainder and refill.
			b.next, b.end = 0, 0
			b.expires = time.Time{}
			b.gen = 0
			valid = false
		} else {
			b.next, b.end = 0, 0
			b.expires = time.Time{}
			b.gen = 0
		}
		call := &tsoCall{done: make(chan struct{})}
		b.inflight = call
		b.mu.Unlock()

		err := c.refillTSBatch(ctx, valid, gen)

		b.mu.Lock()
		call.err = err
		b.inflight = nil
		close(call.done)
		b.mu.Unlock()

		if err != nil {
			lastErr = err
			if errors.Is(err, errClientClosed) {
				return 0, err
			}
			select {
			case <-ctx.Done():
				return 0, fmt.Errorf("v2client: alloc ts: %w", lastErr)
			case <-time.After(tsoRetryDelay):
			}
		}
	}
}

// refillTSBatch validates the existing batch or reserves a new one. valid and
// batchGen describe the batch observed by the caller; when the meta generation
// still matches, a validation probe reuses the reservation instead of wasting
// its remaining timestamps.
func (c *Client) refillTSBatch(ctx context.Context, valid bool, batchGen uint64) error {
	var lastErr error
	for {
		mc, gen, err := c.metaClientFresh(ctx)
		if err == nil {
			if valid && gen == batchGen && c.reuseTSBatch(batchGen) {
				return nil
			}
			size := c.tsoBatchSizeValue()
			resp, aerr := mc.AllocTS(ctx, &pbv2.AllocTSRequest{Count: size})
			if aerr == nil {
				start, count := resp.GetStart(), int(resp.GetCount())
				switch {
				case start == 0 || count <= 0:
					aerr = fmt.Errorf("v2client: invalid TSO response start=%d count=%d", start, count)
				case uint64(count-1) > ^uint64(0)-start:
					aerr = fmt.Errorf("v2client: TSO batch overflows: start=%d count=%d", start, count)
				default:
					if c.installTSBatch(start, count, gen) {
						return nil
					}
					aerr = fmt.Errorf("v2client: TSO went backwards: batch starts at %d", start)
				}
			}
			lastErr = aerr
		} else {
			lastErr = err
			if errors.Is(err, errClientClosed) {
				return err
			}
		}

		// Any allocation or discovery failure invalidates the cached meta
		// leader and every range/TSO value derived from it.
		c.invalidateMetaLeader()
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("v2client: alloc ts: %w", lastErr)
			}
			return ctx.Err()
		case <-time.After(tsoRetryDelay):
		}
	}
}

// reuseTSBatch keeps a validated batch and resets its idle/validation budget.
func (c *Client) reuseTSBatch(gen uint64) bool {
	b := &c.tso
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.next == 0 || b.next >= b.end || b.gen != gen || !time.Now().Before(b.expires) {
		return false
	}
	b.lastUsed = time.Now()
	b.served = 0
	return true
}

// installTSBatch publishes a freshly allocated reservation. It returns false
// when the whole reservation is older than a timestamp already returned by
// this client (which should only happen if meta state went backwards).
func (c *Client) installTSBatch(start uint64, count int, gen uint64) bool {
	if count <= 0 {
		return false
	}
	end := start + uint64(count)
	now := time.Now()
	b := &c.tso
	b.mu.Lock()
	defer b.mu.Unlock()

	b.next = start
	b.end = end
	b.expires = now.Add(c.tsoBatchTTLValue())
	b.gen = gen
	b.lastUsed = now
	b.served = 0
	if b.lastIssued != 0 && start <= b.lastIssued {
		next := b.lastIssued + 1
		if next >= end {
			b.next, b.end = 0, 0
			b.expires = time.Time{}
			b.gen = 0
			return false
		}
		// Timestamps before next were allocated previously; skipping them
		// keeps the per-client monotonic guarantee. Gaps are fine.
		b.next = next
	}
	return true
}

func (c *Client) tsoBatchSizeValue() int32 {
	if c.tsBatchSize <= 0 {
		return defaultTSOBatchSize
	}
	return c.tsBatchSize
}

func (c *Client) tsoBatchTTLValue() time.Duration {
	if c.tsBatchTTL <= 0 {
		return defaultTSOBatchTTL
	}
	return c.tsBatchTTL
}

func (c *Client) tsoIdleThreshold() time.Duration {
	if c.tsIdleCheck <= 0 {
		return defaultTSOIdleCheck
	}
	return c.tsIdleCheck
}

func (c *Client) tsoValidateLimit() uint64 {
	if c.tsValidateEvery == 0 {
		return defaultTSOValidateEvery
	}
	return c.tsValidateEvery
}
