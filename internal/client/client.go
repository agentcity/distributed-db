// Package client provides a smart, leader-aware client for the KV v2 API.
//
// The client resolves the leader of a key through a Router, forwards the
// operation to that node through a Transport, and retries transient routing
// failures (ErrNotLeader and ErrUnavailable) with exponential backoff and
// jitter. Leaders are cached in a bounded LRU keyed by a routing prefix of the
// key, so repeated operations on a key (or on nearby keys) avoid a router
// round trip.
//
// The prefix is a routing approximation: the Router API exposes the leader of
// an exact key but not range boundaries. If a cached leader turns out to be
// stale, the node answers ErrNotLeader, the entry is invalidated, the router is
// queried again and the operation is retried. A stale hit therefore costs at
// most one extra attempt and never changes correctness.
//
// Get returns ErrNotFound for a missing key. Not-found results, malformed
// arguments and context cancellation are never retried. All errors are wrapped
// with the operation and key/range so callers can log them directly.
package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// Sentinel errors returned by the client and expected from Transport/Router
// implementations.
var (
	// ErrNotLeader means the contacted node does not serve the key's range.
	// The client invalidates the cached leader and retries.
	ErrNotLeader = errors.New("client: not leader")

	// ErrUnavailable means the contacted node is temporarily unable to serve
	// the request. The client refreshes the route and retries.
	ErrUnavailable = errors.New("client: unavailable")

	// ErrNotFound means the requested key does not exist. Get and Scan never
	// translate this into a retry.
	ErrNotFound = errors.New("client: key not found")
)

// KV is one key/value pair returned by Scan.
type KV struct {
	Key   []byte
	Value []byte
}

// Router resolves the leader address responsible for key. Implementations may
// return ErrNotLeader or ErrUnavailable to request a refresh and retry.
type Router interface {
	Leader(ctx context.Context, key []byte) (addr string, err error)
}

// Transport performs one request against the node at addr. Implementations
// should wrap ErrNotLeader, ErrUnavailable and ErrNotFound so that errors.Is
// can classify the failure.
type Transport interface {
	Get(ctx context.Context, addr string, key []byte) ([]byte, error)
	Put(ctx context.Context, addr string, key, value []byte) error
	Delete(ctx context.Context, addr string, key []byte) error
	Scan(ctx context.Context, addr string, start, end []byte, limit int) ([]KV, error)
}

// Options configures retry and caching behaviour. The zero value is usable:
// it means the defaults documented on each field.
type Options struct {
	// MaxRetries is the number of retries after the initial attempt. Zero
	// selects the default (3). A negative value disables retries.
	MaxRetries int

	// Backoff is the base delay between attempts; it doubles after every
	// failed retry and is randomised in [base/2, base]. Zero selects the
	// default (25ms). A negative value retries immediately without sleeping.
	Backoff time.Duration

	// Timeout bounds one logical operation, including leader lookups, all
	// retries and the backoff between them. Zero selects the default (5s).
	// A negative value disables the client-side deadline; the caller's
	// context is still honoured.
	Timeout time.Duration
}

const (
	defaultMaxRetries = 3
	defaultBackoff    = 25 * time.Millisecond
	defaultTimeout    = 5 * time.Second
	maxBackoff        = 2 * time.Second

	// leaderCacheEntries bounds the leader LRU. It is deliberately small: the
	// cache accelerates repeated access, it is not a range map.
	leaderCacheEntries = 1024

	// leaderPrefixBytes is how much of a key identifies a cached leader. The
	// Router API has no range metadata, so a fixed-size prefix is the best
	// available approximation. Stale entries self-heal on ErrNotLeader.
	leaderPrefixBytes = 16
)

// Client is a concurrency-safe smart KV client. It is safe to share one Client
// between goroutines.
type Client struct {
	router    Router
	transport Transport

	maxRetries int
	backoff    time.Duration
	timeout    time.Duration

	leaders *leaderCache
}

// New returns a Client. A nil router or transport makes every operation fail
// with an explicit error instead of panicking. opts fields documented as zero
// defaults are normalised once, so later changes to opts do not affect c.
func New(router Router, transport Transport, opts Options) *Client {
	maxRetries := opts.MaxRetries
	switch {
	case maxRetries < 0:
		maxRetries = 0
	case maxRetries == 0:
		maxRetries = defaultMaxRetries
	}

	backoff := opts.Backoff
	if backoff == 0 {
		backoff = defaultBackoff
	}

	timeout := opts.Timeout
	switch {
	case timeout < 0:
		timeout = 0
	case timeout == 0:
		timeout = defaultTimeout
	}

	return &Client{
		router:     router,
		transport:  transport,
		maxRetries: maxRetries,
		backoff:    backoff,
		timeout:    timeout,
		leaders:    newLeaderCache(leaderCacheEntries),
	}
}

// Get returns the value stored at key. A missing key returns ErrNotFound.
func (c *Client) Get(ctx context.Context, key []byte) ([]byte, error) {
	if err := validateKey(key); err != nil {
		return nil, fmt.Errorf("client: get key %s: %w", quoteKey(key), err)
	}
	detail := "key " + quoteKey(key)
	return execute(c, ctx, "get", key, detail, func(callCtx context.Context, addr string) ([]byte, error) {
		return c.transport.Get(callCtx, addr, key)
	})
}

// Put stores value at key.
func (c *Client) Put(ctx context.Context, key, value []byte) error {
	if err := validateKey(key); err != nil {
		return fmt.Errorf("client: put key %s: %w", quoteKey(key), err)
	}
	detail := "key " + quoteKey(key)
	_, err := execute(c, ctx, "put", key, detail, func(callCtx context.Context, addr string) (struct{}, error) {
		return struct{}{}, c.transport.Put(callCtx, addr, key, value)
	})
	return err
}

// Delete removes key. Deleting a missing key is not an error.
func (c *Client) Delete(ctx context.Context, key []byte) error {
	if err := validateKey(key); err != nil {
		return fmt.Errorf("client: delete key %s: %w", quoteKey(key), err)
	}
	detail := "key " + quoteKey(key)
	_, err := execute(c, ctx, "delete", key, detail, func(callCtx context.Context, addr string) (struct{}, error) {
		return struct{}{}, c.transport.Delete(callCtx, addr, key)
	})
	return err
}

// Scan returns up to limit key/value pairs in the half-open range [start, end).
// A nil bound means unbounded; limit <= 0 means no client-side limit. Scan is
// routed using start, since Transport.Scan targets a single node.
func (c *Client) Scan(ctx context.Context, start, end []byte, limit int) ([]KV, error) {
	if err := validateScan(start, end, limit); err != nil {
		return nil, fmt.Errorf("client: scan range [%s, %s) limit %d: %w",
			quoteKey(start), quoteKey(end), limit, err)
	}
	detail := fmt.Sprintf("range [%s, %s) limit %d", quoteKey(start), quoteKey(end), limit)
	return execute(c, ctx, "scan", start, detail, func(callCtx context.Context, addr string) ([]KV, error) {
		return c.transport.Scan(callCtx, addr, start, end, limit)
	})
}

var (
	errNilClient  = errors.New("client is not initialised: nil router or transport")
	errEmptyKey   = errors.New("invalid key: key must not be empty")
	errBadRange   = errors.New("invalid range: start must be less than end")
	errBadLimit   = errors.New("invalid limit: must not be negative")
	errEmptyRoute = errors.New("router returned an empty leader address")
)

func validateKey(key []byte) error {
	if len(key) == 0 {
		return errEmptyKey
	}
	return nil
}

func validateScan(start, end []byte, limit int) error {
	if limit < 0 {
		return errBadLimit
	}
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return errBadRange
	}
	return nil
}

// execute runs one logical operation: resolve a leader, call the transport and
// retry retryable failures with exponential backoff. It is generic only so the
// four public operations can share exactly one retry/cache implementation.
func execute[T any](
	c *Client,
	ctx context.Context,
	op string,
	routeKey []byte,
	detail string,
	call func(context.Context, string) (T, error),
) (T, error) {
	var zero T

	if c == nil || c.router == nil || c.transport == nil {
		return zero, fmt.Errorf("client: %s %s: %w", op, detail, errNilClient)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	opCtx, cancel := c.operationContext(ctx)
	defer cancel()

	prefix := routePrefix(routeKey)
	var lastErr error
	addr := ""
	attempts := 0

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(opCtx, c.retryDelay(attempt-1)); err != nil {
				return zero, wrapOpError(op, detail, addr, attempts, err)
			}
		}
		if err := opCtx.Err(); err != nil {
			return zero, wrapOpError(op, detail, addr, attempts, err)
		}

		attempts++
		// Retries bypass the cache: a not-leader/unavailable reply means the
		// cached route is suspect and the router must be asked again.
		node, err := c.resolveLeader(opCtx, prefix, routeKey, attempt > 0)
		if err != nil {
			lastErr = err
			c.leaders.invalidate(prefix)
			if !isRetryable(err) || opCtx.Err() != nil {
				return zero, wrapOpError(op, detail, node, attempts, err)
			}
			continue
		}
		addr = node

		value, err := call(opCtx, node)
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !isRetryable(err) || opCtx.Err() != nil {
			return zero, wrapOpError(op, detail, node, attempts, err)
		}
		c.leaders.invalidate(prefix)
	}

	return zero, wrapOpError(op, detail, addr, attempts, lastErr)
}

// resolveLeader returns a leader for key, either from the LRU or from the
// router. bypass forces a router lookup, which retries use so that a stale
// cache entry cannot be reused after a routing failure.
func (c *Client) resolveLeader(ctx context.Context, prefix string, key []byte, bypass bool) (string, error) {
	if !bypass {
		if addr, ok := c.leaders.get(prefix); ok {
			return addr, nil
		}
	}

	addr, err := c.router.Leader(ctx, key)
	if err != nil {
		return "", fmt.Errorf("router leader lookup: %w", err)
	}
	if addr == "" {
		// An empty address cannot receive the request; treat it as a transient
		// routing failure so the normal refresh path applies.
		return "", fmt.Errorf("router leader lookup: %w", errEmptyRoute)
	}
	c.leaders.put(prefix, addr)
	return addr, nil
}

// operationContext applies the configured deadline to one logical operation.
func (c *Client) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.timeout)
}

// retryDelay returns the jittered exponential delay before the given retry.
// retry=0 is the first retry. Jitter is in [base/2, base], which spreads
// concurrent clients without making tests depend on exact sleep durations.
func (c *Client) retryDelay(retry int) time.Duration {
	if c.backoff <= 0 {
		return 0
	}

	delay := c.backoff
	for i := 0; i < retry; i++ {
		if delay >= maxBackoff/2 {
			delay = maxBackoff
			break
		}
		delay *= 2
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}

	half := delay / 2
	if half <= 0 {
		return delay
	}
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// sleepCtx sleeps for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isRetryable(err error) bool {
	return errors.Is(err, ErrNotLeader) || errors.Is(err, ErrUnavailable)
}

// wrapOpError adds operation, key/range, target and attempt context while
// preserving errors.Is/errors.As for the underlying cause.
func wrapOpError(op, detail, addr string, attempts int, err error) error {
	if err == nil {
		err = errors.New("operation failed without an error")
	}

	attemptSuffix := ""
	if attempts > 0 {
		attemptSuffix = fmt.Sprintf(" (after %d attempt(s))", attempts)
	}
	if addr != "" {
		return fmt.Errorf("client: %s %s at %q: %w%s", op, detail, addr, err, attemptSuffix)
	}
	return fmt.Errorf("client: %s %s: %w%s", op, detail, err, attemptSuffix)
}

// quoteKey renders a key for error messages without dumping arbitrarily large
// keys. nil is shown distinctly because it is a valid Scan bound.
func quoteKey(key []byte) string {
	if key == nil {
		return "<nil>"
	}
	const maxShown = 64
	if len(key) > maxShown {
		return fmt.Sprintf("%q...(%d bytes)", key[:maxShown], len(key))
	}
	return fmt.Sprintf("%q", key)
}
